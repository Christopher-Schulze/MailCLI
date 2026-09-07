package mail

import (
	"context"
	"errors"
	"io"

	"mailcli/internal/transport"
)

func composeDraftSpool(ctx context.Context, draft Draft, messageID string) (*ComposedMessage, error) {
	if err := preflightDraftAttachmentsContext(ctx, draft.Attachments); err != nil {
		return nil, err
	}
	return ComposeMessageSpoolContext(ctx, draft, messageID)
}

func submitComposedMessage(
	ctx context.Context,
	submitter transport.Submitter,
	cfg transport.SubmitConfig,
	from string,
	recipients []string,
	message *ComposedMessage,
) (evidence transport.SubmitEvidence, resultErr error) {
	if streaming, ok := submitter.(transport.StreamingSubmitter); ok {
		reader, err := message.Open()
		if err != nil {
			return transport.SubmitEvidence{}, err
		}
		defer func() {
			if err := reader.Close(); err != nil {
				resultErr = errors.Join(resultErr, err)
			}
		}()
		return streaming.SubmitReader(ctx, cfg, from, recipients, message.MessageID(), reader, message.Size())
	}
	payload, err := readComposedMessage(message)
	if err != nil {
		return transport.SubmitEvidence{}, err
	}
	return submitter.Submit(ctx, cfg, from, recipients, payload)
}

func mirrorComposedMessage(
	ctx context.Context,
	mirror transport.SentMirror,
	cfg transport.ImapConfig,
	message *ComposedMessage,
	messageID string,
) (evidence transport.AppendEvidence, resultErr error) {
	if streaming, ok := mirror.(transport.StreamingSentMirror); ok {
		reader, err := message.Open()
		if err != nil {
			return transport.AppendEvidence{}, err
		}
		defer func() {
			if err := reader.Close(); err != nil {
				resultErr = errors.Join(resultErr, err)
			}
		}()
		return streaming.AppendToSentReader(ctx, cfg, reader, message.Size(), messageID)
	}
	payload, err := readComposedMessage(message)
	if err != nil {
		return transport.AppendEvidence{}, err
	}
	return mirror.AppendToSent(ctx, cfg, payload, messageID)
}

func readComposedMessage(message *ComposedMessage) (payload []byte, resultErr error) {
	reader, err := message.Open()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := reader.Close(); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	return io.ReadAll(reader)
}
