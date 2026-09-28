package cli

import (
	"context"
	"flag"

	"golang.org/x/sync/errgroup"
	"mailcli/internal/mail"
)

func addMessageEnrichmentFlags(flags *flag.FlagSet, page bool) *mail.MessageEnrichmentRequest {
	request := &mail.MessageEnrichmentRequest{ExcerptLength: mail.DefaultExcerptLength}
	if page {
		flags.BoolVar(&request.Threading, "with-threading", false, "read bounded RFC headers for reply IDs and structured sender; disclose threading_complete")
		flags.BoolVar(&request.Excerpt, "with-excerpt", false, "read at most 256 KiB of RFC source per message for an excerpt; disclose source and completeness")
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

const (
	// enrichmentPageSourceBytes caps the excerpt source bytes one page may read.
	enrichmentPageSourceBytes = int64(8 << 20)
	enrichmentConcurrency     = 4
	enrichmentBudgetExhausted = "enrichment_page_budget_exhausted"
)

// enrichSummaries fills requested reply metadata in place, in row order, with
// bounded concurrency. Rows past the page's excerpt byte budget are not read
// and name the budget in enrichment_error.
func enrichSummaries(ctx context.Context, service *mail.Service, summaries []*mail.MessageSummary, request mail.MessageEnrichmentRequest) error {
	if !request.Threading && !request.Excerpt {
		return nil
	}
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(enrichmentConcurrency)
	var reserved int64
	for _, summary := range summaries {
		if request.Excerpt {
			reserved += min(max(summary.Size, 1), mail.MaximumExcerptSourceBytes)
			if reserved > enrichmentPageSourceBytes {
				summary.EnrichmentError = enrichmentBudgetExhausted
				continue
			}
		}
		group.Go(func() error {
			enriched, err := service.EnrichMessage(groupCtx, *summary, request)
			if err != nil {
				return err
			}
			*summary = enriched
			return nil
		})
	}
	return group.Wait()
}
