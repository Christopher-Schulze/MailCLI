package mailstore

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/emersion/go-message"
	messageMail "github.com/emersion/go-message/mail"
	"mailcli/internal/mail"
)

type mimePart struct {
	ID       string
	Name     string
	MIMEType string
	Size     int64
	SHA256   string
	Complete bool
}

type mimeDocument struct {
	MessageID    string
	ReplyTo      string
	To           []mail.Recipient
	CC           []mail.Recipient
	BCC          []mail.Recipient
	Content      string
	Complete     bool
	MissingParts []string
	Parts        map[string]mimePart
	// skipNonTextBodies avoids decoding non-text part bodies (no base64/QP
	// decode, no drain through the decoded stream). The walker still reads
	// and discards the raw bytes, so I/O is unchanged. Search-only: names
	// and counts stay exact, sizes stay unknown.
	skipNonTextBodies bool
	ctx               context.Context
	budget            *mimeParseBudget
}

type mimeTextRepresentation struct {
	Text string
	Rank int
}

const (
	mimeTextNone = iota
	mimeTextHTML
	mimeTextPlain
)

func parseMIMEDocument(reader io.Reader, partial bool, hashAttachments bool, skipNonTextBodies bool) (mimeDocument, error) {
	return parseMIMEDocumentWithContext(
		context.Background(), reader, partial, hashAttachments, skipNonTextBodies,
	)
}

func parseMIMEDocumentWithContext(
	ctx context.Context,
	reader io.Reader,
	partial bool,
	hashAttachments bool,
	skipNonTextBodies bool,
) (mimeDocument, error) {
	return parseMIMEDocumentWithLimits(
		ctx, reader, partial, hashAttachments, skipNonTextBodies,
		defaultMIMEParseBudgetLimits(),
	)
}

func parseMIMEDocumentWithLimits(
	ctx context.Context,
	reader io.Reader,
	partial bool,
	hashAttachments bool,
	skipNonTextBodies bool,
	limits mimeParseBudgetLimits,
) (mimeDocument, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	budget := newMIMEParseBudget(limits)
	document := mimeDocument{
		Complete:          false,
		skipNonTextBodies: skipNonTextBodies,
		ctx:               ctx,
		budget:            budget,
	}
	if err := ctx.Err(); err != nil {
		markMIMECanceled(&document)
		return document, err
	}
	trackedReader := &mimeBudgetReader{ctx: ctx, reader: reader, budget: budget}
	stopCloseWatcher := closeMIMEReaderOnCancel(ctx, reader)
	defer stopCloseWatcher()
	entity, readErr := message.Read(trackedReader)
	if entity == nil {
		if err := ctx.Err(); err != nil {
			markMIMECanceled(&document)
			return document, err
		}
		if budgetErr := budget.error(); budgetErr != nil {
			markMIMEBudgetExceeded(&document, budgetErr)
			return document, nil
		}
	}
	if entity == nil || (readErr != nil && !message.IsUnknownCharset(readErr) && !message.IsUnknownEncoding(readErr)) {
		return mimeDocument{}, operationError("invalid_message_source", fmt.Sprintf("parse RFC message: %v", readErr))
	}
	document.Complete = readErr == nil && !partial
	if readErr != nil {
		markMissingPart(&document, "mime-decoding")
	}
	header := messageMail.Header{Header: entity.Header}
	if messageID, err := header.MessageID(); err == nil {
		if document.retainMetadata(int64(len(messageID))) {
			document.MessageID = messageID
		}
	}
	var replyToComplete bool
	document.ReplyTo, replyToComplete = firstFormattedAddress(&header, "Reply-To")
	if document.ReplyTo != "" && !document.retainMetadata(int64(len(document.ReplyTo))) {
		document.ReplyTo = ""
	}
	if !replyToComplete {
		markMissingPart(&document, "header:reply-to")
	}
	document.To = documentRecipients(&document, &header, "To")
	document.CC = documentRecipients(&document, &header, "Cc")
	document.BCC = documentRecipients(&document, &header, "Bcc")
	if budgetErr := budget.error(); budgetErr != nil {
		markMIMEBudgetExceeded(&document, budgetErr)
		return document, nil
	}
	representation, walkErr := parseMIMEEntity(
		entity, nil, readErr, partial, hashAttachments, &document,
	)
	document.Content = representation.Text
	if walkErr != nil {
		if err := ctx.Err(); err != nil {
			markMIMECanceled(&document)
			return document, err
		}
		if budgetErr := mimeBudgetError(&document, walkErr); budgetErr != nil {
			markMIMEBudgetExceeded(&document, budgetErr)
			return document, nil
		}
		markMissingPart(&document, "mime-structure")
	}
	if err := ctx.Err(); err != nil {
		markMIMECanceled(&document)
		return document, err
	}
	return document, nil
}

func (document *mimeDocument) contextErr() error {
	if document.ctx == nil {
		return nil
	}
	return document.ctx.Err()
}

func (document *mimeDocument) retainMetadata(amount int64) bool {
	if document.budget == nil || document.budget.reserveMetadata(amount) {
		return true
	}
	markMIMEBudgetExceeded(document, document.budget.error())
	return false
}

func mimeBudgetError(document *mimeDocument, err error) *mimeResourceLimitError {
	if document != nil && document.budget != nil {
		if budgetErr := document.budget.error(); budgetErr != nil {
			return budgetErr
		}
	}
	var budgetErr *mimeResourceLimitError
	if errors.As(err, &budgetErr) {
		return budgetErr
	}
	return nil
}

func appendMissingPart(document *mimeDocument, identifier string) {
	for _, existing := range document.MissingParts {
		if existing == identifier {
			return
		}
	}
	document.MissingParts = append(document.MissingParts, identifier)
}

func markMIMEBudgetExceeded(document *mimeDocument, budgetErr *mimeResourceLimitError) {
	if budgetErr == nil {
		return
	}
	document.Complete = false
	appendMissingPart(document, mimeBudgetDiagnosticID+string(budgetErr.resource))
}

func markMIMECanceled(document *mimeDocument) {
	document.Complete = false
	appendMissingPart(document, mimeCanceledDiagnosticID)
}
