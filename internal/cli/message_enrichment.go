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
		flags.BoolVar(&request.Excerpt, "with-excerpt", false, "read at most 256 KiB of local RFC source or a 64 KiB IMAP text prefix per message for an excerpt; disclose source and completeness")
	}
	requirement := "requires --fields excerpt"
	if page {
		requirement = "requires --with-excerpt"
	}
	flags.IntVar(&request.ExcerptLength, "excerpt-length", mail.DefaultExcerptLength, "maximum excerpt runes (1-1000); "+requirement)
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

// enrichmentPageSourceBytes caps the excerpt source bytes one page may read.
const enrichmentPageSourceBytes = int64(8 << 20)

// enrichSummaries fills requested reply metadata in place, in row order, as
// one page request. The store charges the excerpt budget with the bytes it
// reads; rows past it keep their reply metadata and name the budget in
// enrichment_error.
func enrichSummaries(ctx context.Context, service *mail.Service, summaries []*mail.MessageSummary, request mail.MessageEnrichmentRequest) error {
	if !request.Threading && !request.Excerpt {
		return nil
	}
	if request.Excerpt {
		request.ExcerptSourceBudget = enrichmentPageSourceBytes
	}
	return service.EnrichMessages(ctx, summaries, request)
}
