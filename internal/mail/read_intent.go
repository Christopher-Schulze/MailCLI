package mail

import "context"

// MessageReadIntent selects the smallest message representation a caller
// needs. Full preserves the historical GetMessage behavior.
type MessageReadIntent string

const (
	MessageReadIntentFull        MessageReadIntent = "full"
	MessageReadIntentIndex       MessageReadIntent = "index"
	MessageReadIntentHeaders     MessageReadIntent = "headers"
	MessageReadIntentAttachments MessageReadIntent = "attachments"
)

// MessageReadGateway is an optional Gateway extension for callers that can
// consume a narrower read without changing legacy gateway implementations.
type MessageReadGateway interface {
	GetMessageWithIntent(context.Context, string, MessageReadIntent) (Message, error)
}

// GetMessageWithIntent reads only the requested message representation when
// the configured gateway supports it; otherwise it preserves the full read.
func (s *Service) GetMessageWithIntent(
	ctx context.Context,
	ref string,
	intent MessageReadIntent,
) (Message, error) {
	if ref == "" {
		return Message{}, validationError("message ref is required")
	}
	switch intent {
	case MessageReadIntentFull:
		return s.gateway.GetMessage(ctx, ref)
	case MessageReadIntentIndex, MessageReadIntentHeaders, MessageReadIntentAttachments:
		reader, ok := s.gateway.(MessageReadGateway)
		if ok {
			return reader.GetMessageWithIntent(ctx, ref, intent)
		}
		return s.gateway.GetMessage(ctx, ref)
	default:
		return Message{}, validationError("message read intent is invalid")
	}
}
