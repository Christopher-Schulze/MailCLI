package cli

import (
	"context"
	"flag"
	"mailcli/internal/mail"
)

func addMessageEnrichmentFlags(flags *flag.FlagSet, page bool) *mail.MessageEnrichmentRequest {
	request := &mail.MessageEnrichmentRequest{ExcerptLength: mail.DefaultExcerptLength}
	if page {
		flags.BoolVar(&request.Threading, "with-threading", false, "read bounded RFC headers for reply IDs and structured sender; disclose threading_complete")
		flags.BoolVar(&request.Excerpt, "with-excerpt", false, "read at most 256 KiB of RFC source per message for an excerpt; disclose source and completeness")
	}
	flags.IntVar(&request.ExcerptLength, "excerpt-length", mail.DefaultExcerptLength, "maximum excerpt runes (1-1000); requires excerpt selection or --with-excerpt")
	return request
}

func validateMessageEnrichment(request mail.MessageEnrichmentRequest) error {
	if request.ExcerptLength < 1 || request.ExcerptLength > mail.MaximumExcerptLength {
		return &commandError{code: "invalid_argument", message: "--excerpt-length must be between 1 and 1000"}
	}
	return nil
}

func newMessageMetadataField(field string) bool {
	return field == "header_fields" || field == "excerpt" || field == "excerpt_complete" || field == "excerpt_source"
}

func enrichMessagePage(ctx context.Context, service *mail.Service, messages []mail.MessageSummary, request mail.MessageEnrichmentRequest) error {
	for index := range messages {
		if !request.Threading && !request.Excerpt {
			return nil
		}
		enriched, err := service.EnrichMessage(ctx, messages[index], request)
		if err != nil {
			return err
		}
		messages[index] = enriched
	}
	return nil
}
