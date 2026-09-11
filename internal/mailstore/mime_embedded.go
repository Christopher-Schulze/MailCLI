package mailstore

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"

	"github.com/emersion/go-message"
	messageMail "github.com/emersion/go-message/mail"
)

func isEmbeddedMessageType(mediaType string) bool {
	return mediaType == "message/rfc822" || mediaType == "message/global"
}

// The exposed part is the entire encapsulated RFC message. Validate its
// contents without flattening nested headers, retaining a second body, or
// adding its children again to the outer attachment list. Hash before nested
// transfer decoding so extraction retains the original embedded MIME bytes.
func consumeEmbeddedMessage(reader io.Reader, depth int64, document *mimeDocument, withHash bool) (int64, string, error) {
	tracked := embeddedPartReader{reader: mimeContextReader{ctx: document.ctx, reader: reader}}
	var digest hash.Hash
	if withHash {
		digest = sha256.New()
		tracked.reader = io.TeeReader(tracked.reader, digest)
	}
	err := validateEmbeddedMessage(&tracked, depth, document)
	if contextErr := document.contextErr(); contextErr != nil {
		return tracked.size, "", contextErr
	}
	if budgetErr := document.budget.error(); budgetErr != nil {
		return tracked.size, "", budgetErr
	}
	// Consume any epilogue or unparsed remainder within the same source budget.
	// A malformed child never turns these retained bytes into complete proof.
	_, drainErr := io.Copy(io.Discard, &tracked)
	err = errors.Join(err, drainErr)
	if err != nil || digest == nil {
		return tracked.size, "", err
	}
	return tracked.size, hex.EncodeToString(digest.Sum(nil)), nil
}

type embeddedPartReader struct {
	reader io.Reader
	size   int64
}

func (r *embeddedPartReader) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	r.size += int64(read)
	return read, err
}

func validateEmbeddedMessage(reader io.Reader, depth int64, document *mimeDocument) error {
	if err := document.contextErr(); err != nil {
		return err
	}
	if err := document.budget.checkDepth(depth); err != nil {
		return err
	}
	// Keep buffered body bytes when enforcing a real header/body boundary;
	// go-message alone also accepts a header block terminated only by EOF.
	limited := &io.LimitedReader{R: reader, N: int64(maximumHeaderBytes) + 1}
	buffered := bufio.NewReaderSize(limited, mimeHeaderReaderBuffer)
	headers, err := readRawHeaderBlock(buffered)
	if err != nil {
		return err
	}
	limited.N = maximumInt64
	entity, err := message.Read(io.MultiReader(strings.NewReader(headers), buffered))
	if err != nil {
		return err
	}
	if !entity.Header.Has("From") && !entity.Header.Has("Subject") && !entity.Header.Has("Date") {
		return fmt.Errorf("embedded message has no From, Subject or Date header")
	}
	header := messageMail.Header{Header: entity.Header}
	for _, key := range []string{"From", "Sender", "Reply-To", "To", "Cc", "Bcc"} {
		if _, complete, err := headerRecipients(&header, key); !complete {
			return fmt.Errorf("invalid embedded address header: %w", err)
		}
	}
	return validateEmbeddedMIMEEntity(entity, depth, document)
}

func validateEmbeddedMIMEEntity(entity *message.Entity, depth int64, document *mimeDocument) (resultErr error) {
	if err := document.contextErr(); err != nil {
		return err
	}
	if err := document.budget.visit(depth); err != nil {
		return err
	}
	fields := entity.Header.Fields()
	for fields.Next() {
		if !document.retainMetadata(mimeStringBytes(fields.Key(), fields.Value()) + mimePartMetadataOverhead) {
			return document.budget.error()
		}
	}
	mediaType, _, err := entity.Header.ContentType()
	if err != nil {
		return err
	}
	mediaType = strings.ToLower(mediaType)
	if entity.Header.Has("Content-Disposition") {
		if _, _, err := entity.Header.ContentDisposition(); err != nil {
			return err
		}
	}
	if hint := entity.Header.Get("X-Apple-Content-Length"); hint != "" {
		tracked := &embeddedPartReader{reader: mimeContextReader{ctx: document.ctx, reader: entity.Body}}
		entity.Body = tracked
		defer func() {
			if resultErr != nil {
				return
			}
			// Include multipart epilogues in the original decoded body length.
			_, resultErr = io.Copy(io.Discard, tracked)
			if resultErr == nil && missingAppleContent(hint, tracked.size, true) {
				resultErr = fmt.Errorf("embedded MIME part is externalized or truncated")
			}
		}()
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		return validateEmbeddedMultipart(entity, mediaType, depth, document)
	}
	if isEmbeddedMessageType(mediaType) {
		return validateEmbeddedMessage(entity.Body, depth+1, document)
	}
	if strings.HasPrefix(mediaType, "message/") {
		return fmt.Errorf("embedded MIME contains an unsupported message subtype")
	}
	return consumeEmbeddedLeaf(entity, mediaType, document)
}

func validateEmbeddedMultipart(entity *message.Entity, mediaType string, depth int64, document *mimeDocument) (resultErr error) {
	reader := entity.MultipartReader()
	if reader == nil {
		return fmt.Errorf("embedded multipart entity has no reader")
	}
	defer joinCloseError(&resultErr, reader, "embedded MIME multipart reader")
	for {
		if err := document.contextErr(); err != nil {
			return err
		}
		child, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if mediaType == "multipart/digest" && !child.Header.Has("Content-Type") {
			child.Header.Set("Content-Type", "message/rfc822")
		}
		if err := validateEmbeddedMIMEEntity(child, depth+1, document); err != nil {
			return err
		}
	}
}

func consumeEmbeddedLeaf(entity *message.Entity, mediaType string, document *mimeDocument) error {
	reader := mimeContextReader{ctx: document.ctx, reader: entity.Body}
	if !strings.HasPrefix(mediaType, "text/") {
		_, err := io.Copy(io.Discard, reader)
		return err
	}
	remaining := document.budget.remainingTextBytes()
	limit := min(remaining, int64(maximumTextPartBytes))
	size, err := io.Copy(io.Discard, io.LimitReader(reader, limit+1))
	if !document.budget.consumeText(min(size, limit)) {
		return document.budget.error()
	}
	if size > limit {
		if limit == remaining {
			return document.budget.exhaust(mimeBudgetTextBytes, document.budget.textBytes+1, document.budget.limits.textBytes)
		}
		return fmt.Errorf("embedded text part exceeds 16 MiB")
	}
	return err
}
