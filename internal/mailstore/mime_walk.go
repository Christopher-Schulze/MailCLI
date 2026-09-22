package mailstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/emersion/go-message"
	"mailcli/internal/mail"
)

func parseMIMEEntity(
	entity *message.Entity,
	path []int,
	partErr error,
	partial bool,
	hashAttachments bool,
	document *mimeDocument,
) (mimeTextRepresentation, error) {
	if err := document.contextErr(); err != nil {
		return mimeTextRepresentation{}, err
	}
	if budgetErr := document.budget.visit(int64(len(path))); budgetErr != nil {
		markMIMEBudgetExceeded(document, budgetErr)
		return mimeTextRepresentation{}, budgetErr
	}
	if partErr != nil {
		markMissingPart(document, mimePartID(path))
		if budgetErr := document.budget.error(); budgetErr != nil {
			markMIMEBudgetExceeded(document, budgetErr)
			return mimeTextRepresentation{}, budgetErr
		}
	}
	mediaType, parameters, contentTypeErr := entity.Header.ContentType()
	if contentTypeErr != nil {
		mediaType = "application/octet-stream"
		document.Complete = false
	}
	mediaType = strings.ToLower(mediaType)
	if strings.HasPrefix(mediaType, "multipart/") {
		return parseMIMEMultipart(entity, path, mediaType, partial, hashAttachments, document)
	}
	disposition, dispositionParameters, dispositionErr := entity.Header.ContentDisposition()
	if dispositionErr != nil && entity.Header.Get("Content-Disposition") != "" {
		document.Complete = false
	}
	filename := dispositionParameters["filename"]
	if filename == "" {
		filename = parameters["name"]
	}
	partID := mimePartID(path)
	embedded := isEmbeddedMessageType(mediaType)
	if strings.EqualFold(disposition, "attachment") || filename != "" || embedded {
		if !document.retainMetadata(mimePartMetadataBytes(partID, filename, mediaType, hashAttachments)) {
			return mimeTextRepresentation{}, document.budget.error()
		}
		if document.skipNonTextBodies && !embedded {
			// Search path: skip the decode, not the I/O. The walker still
			// reads and discards raw bytes. Names and counts stay exact.
			if document.Parts == nil {
				document.Parts = make(map[string]mimePart)
			}
			document.Parts[partID] = mimePart{
				ID: partID, Name: filename, MIMEType: mediaType, Size: -1,
				Complete: !partial,
			}
			if partial {
				document.Complete = false
			}
			return mimeTextRepresentation{}, nil
		}
		var size int64
		var digest string
		var err error
		if embedded {
			size, digest, err = consumeEmbeddedMessage(entity.Body, int64(len(path))+1, document, hashAttachments && !document.skipNonTextBodies)
		} else {
			size, digest, err = consumeMIMEAttachmentContext(document.ctx, entity.Body, hashAttachments)
		}
		complete := partErr == nil && err == nil && !missingAppleContent(
			entity.Header.Get("X-Apple-Content-Length"), size, true,
		)
		if embedded && partial {
			complete = false
		}
		if !complete {
			digest = ""
		}
		if document.Parts == nil {
			document.Parts = make(map[string]mimePart)
		}
		document.Parts[partID] = mimePart{
			ID: partID, Name: filename, MIMEType: mediaType, Size: size,
			SHA256: digest, Complete: complete,
		}
		if !complete {
			markMissingPart(document, partID)
		}
		if budgetErr := mimeBudgetError(document, err); budgetErr != nil {
			markMIMEBudgetExceeded(document, budgetErr)
			return mimeTextRepresentation{}, budgetErr
		}
		if contextErr := document.contextErr(); contextErr != nil {
			return mimeTextRepresentation{}, contextErr
		}
		return mimeTextRepresentation{}, nil
	}
	if mediaType != "text/plain" && mediaType != "text/html" {
		if document.skipNonTextBodies {
			if partial {
				document.Complete = false
			}
			return mimeTextRepresentation{}, nil
		}
		_, err := io.Copy(io.Discard, mimeContextReader{ctx: document.ctx, reader: entity.Body})
		if err != nil {
			document.Complete = false
			if budgetErr := mimeBudgetError(document, err); budgetErr != nil {
				markMIMEBudgetExceeded(document, budgetErr)
				return mimeTextRepresentation{}, budgetErr
			}
			if contextErr := document.contextErr(); contextErr != nil {
				return mimeTextRepresentation{}, contextErr
			}
		}
		return mimeTextRepresentation{}, err
	}
	appleLength, hasAppleLength := parseAppleContentLength(entity.Header.Get("X-Apple-Content-Length"))
	remainingText := document.budget.remainingTextBytes()
	if remainingText <= 0 {
		budgetErr := document.budget.exhaust(mimeBudgetTextBytes, document.budget.textBytes+1, document.budget.limits.textBytes)
		markMIMEBudgetExceeded(document, budgetErr)
		return mimeTextRepresentation{}, budgetErr
	}
	partMaximum := int64(maximumTextPartBytes)
	aggregateLimited := remainingText <= partMaximum
	if remainingText < partMaximum {
		partMaximum = remainingText
	}
	body, truncated, err := readBoundedPartContext(
		document.ctx, entity.Body, partMaximum, appleLength, hasAppleLength, !aggregateLimited,
	)
	if len(body) > 0 && !document.budget.consumeText(int64(len(body))) {
		budgetErr := document.budget.error()
		markMIMEBudgetExceeded(document, budgetErr)
		return mimeTextRepresentation{}, budgetErr
	}
	if err != nil || truncated {
		document.Complete = false
	}
	if truncated && aggregateLimited {
		budgetErr := document.budget.exhaust(
			mimeBudgetTextBytes, document.budget.textBytes+1, document.budget.limits.textBytes,
		)
		markMIMEBudgetExceeded(document, budgetErr)
		err = budgetErr
	}
	if hasAppleLength && partial && appleLength > int64(len(body)) {
		markMissingPart(document, partID)
		if budgetErr := mimeBudgetError(document, err); budgetErr != nil {
			return mimeTextRepresentation{}, budgetErr
		}
		if contextErr := document.contextErr(); contextErr != nil {
			return mimeTextRepresentation{}, contextErr
		}
		return mimeTextRepresentation{}, nil
	}
	rank := mimeTextPlain
	var text string
	if mediaType == "text/html" {
		maximumOutput := int64(len(body)) + document.budget.remainingTextBytes()
		var conversionErr error
		text, conversionErr = mail.HTMLToPlainTextContext(document.ctx, body, int(maximumOutput))
		if conversionErr != nil {
			if contextErr := document.contextErr(); contextErr != nil {
				return mimeTextRepresentation{}, contextErr
			}
			stage := "parse"
			var htmlErr *mail.HTMLConversionError
			if errors.As(conversionErr, &htmlErr) {
				stage = htmlErr.Stage
			}
			markMissingPart(document, "mime:html:"+stage)
		}
		if extra := len(text) - len(body); extra > 0 && !document.budget.consumeText(int64(extra)) {
			return mimeTextRepresentation{}, document.budget.error()
		}
		rank = mimeTextHTML
	} else {
		text = string(bytes.TrimSpace(body))
	}
	if text == "" {
		return mimeTextRepresentation{}, err
	}
	if budgetErr := mimeBudgetError(document, err); budgetErr != nil {
		markMIMEBudgetExceeded(document, budgetErr)
		return mimeTextRepresentation{Text: text, Rank: rank}, budgetErr
	}
	if contextErr := document.contextErr(); contextErr != nil {
		return mimeTextRepresentation{Text: text, Rank: rank}, contextErr
	}
	return mimeTextRepresentation{Text: text, Rank: rank}, err
}

func parseMIMEMultipart(
	entity *message.Entity,
	path []int,
	mediaType string,
	partial bool,
	hashAttachments bool,
	document *mimeDocument,
) (result mimeTextRepresentation, resultErr error) {
	reader := entity.MultipartReader()
	if reader == nil {
		return mimeTextRepresentation{}, fmt.Errorf("multipart entity has no reader")
	}
	defer joinCloseError(&resultErr, reader, "MIME multipart reader")
	var children []mimeTextRepresentation
	for index := 0; ; index++ {
		if contextErr := document.contextErr(); contextErr != nil {
			return combineMIMEText(mediaType, children), contextErr
		}
		if budgetErr := document.budget.error(); budgetErr != nil {
			return combineMIMEText(mediaType, children), budgetErr
		}
		child, err := reader.NextPart()
		if contextErr := document.contextErr(); contextErr != nil {
			return combineMIMEText(mediaType, children), contextErr
		}
		if budgetErr := document.budget.error(); budgetErr != nil {
			return combineMIMEText(mediaType, children), budgetErr
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if child == nil {
			return combineMIMEText(mediaType, children), err
		}
		if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
			return combineMIMEText(mediaType, children), err
		}
		childDepth := int64(len(path)) + 1
		if budgetErr := document.budget.checkDepth(childDepth); budgetErr != nil {
			markMIMEBudgetExceeded(document, budgetErr)
			return combineMIMEText(mediaType, children), budgetErr
		}
		childPath := make([]int, len(path)+1)
		copy(childPath, path)
		childPath[len(path)] = index
		if mediaType == "multipart/digest" && !child.Header.Has("Content-Type") {
			child.Header.Set("Content-Type", "message/rfc822")
		}
		representation, childErr := parseMIMEEntity(
			child, childPath, err, partial, hashAttachments, document,
		)
		children = append(children, representation)
		if childErr != nil {
			return combineMIMEText(mediaType, children), childErr
		}
	}
	return combineMIMEText(mediaType, children), nil
}

func combineMIMEText(mediaType string, children []mimeTextRepresentation) mimeTextRepresentation {
	if mediaType == "multipart/alternative" {
		var selected mimeTextRepresentation
		for _, child := range children {
			if child.Text != "" && child.Rank >= selected.Rank {
				selected = child
			}
		}
		return selected
	}
	var builder strings.Builder
	rank := mimeTextNone
	first := true
	for _, child := range children {
		if child.Text != "" {
			if !first {
				builder.WriteString("\n\n")
			}
			builder.WriteString(child.Text)
			first = false
		}
		if child.Rank > rank {
			rank = child.Rank
		}
	}
	return mimeTextRepresentation{Text: builder.String(), Rank: rank}
}

func consumeMIMEAttachmentContext(ctx context.Context, reader io.Reader, withHash bool) (int64, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	contextReader := mimeContextReader{ctx: ctx, reader: reader}
	if !withHash {
		size, err := io.Copy(io.Discard, contextReader)
		return size, "", err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, contextReader)
	return size, hex.EncodeToString(hash.Sum(nil)), err
}

func readBoundedPartContext(
	ctx context.Context,
	reader io.Reader,
	maximum int64,
	sizeHint int64,
	hasSizeHint bool,
	drain bool,
) ([]byte, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if maximum < 0 {
		return nil, false, fmt.Errorf("MIME part limit must not be negative")
	}
	limit := maximum
	if limit < maximumInt64 {
		limit++
	}
	limited := io.LimitReader(mimeContextReader{ctx: ctx, reader: reader}, limit)
	// Pre-size from the decoded-length hint instead of pre-allocating
	// maximum+1 bytes (which can be 16 MiB per text part). io.Copy uses a
	// 32 KiB buffer internally; the bytes.Buffer grows only past the hint.
	var buf bytes.Buffer
	if hasSizeHint && sizeHint > 0 && maximum > 0 {
		buf.Grow(int(min(sizeHint, maximum)))
	}
	if _, err := io.Copy(&buf, limited); err != nil {
		return buf.Bytes(), false, err
	}
	body := buf.Bytes()
	truncated := int64(len(body)) > maximum
	if truncated {
		body = body[:maximum]
	}
	if !drain {
		return body, truncated, nil
	}
	_, drainErr := io.Copy(io.Discard, mimeContextReader{ctx: ctx, reader: reader})
	if drainErr != nil {
		return body, truncated, drainErr
	}
	return body, truncated, nil
}

func mimePartMetadataBytes(partID, name, mediaType string, withHash bool) int64 {
	amount := mimePartMetadataOverhead
	for _, value := range []string{partID, name, mediaType} {
		length := int64(len(value))
		if length > maximumInt64-amount {
			return maximumInt64
		}
		amount += length
	}
	if withHash {
		if 64 > maximumInt64-amount {
			return maximumInt64
		}
		amount += 64
	}
	return amount
}
