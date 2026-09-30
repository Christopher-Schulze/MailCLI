package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"mailcli/internal/mail"
	"mailcli/internal/mailstore"
)

type observedRecoveryStore struct {
	*mailstore.Client
	queries    []mail.PreparedQuery
	enrichment []mail.MessageEnrichmentRequest
}

func (s *observedRecoveryStore) SearchMessages(ctx context.Context, query mail.PreparedQuery) (mail.SearchPage, error) {
	s.queries = append(s.queries, query)
	return s.Client.SearchMessages(ctx, query)
}

func (s *observedRecoveryStore) EnrichMessages(ctx context.Context, refs []string, request mail.MessageEnrichmentRequest) ([]mail.MessageSummary, error) {
	s.enrichment = append(s.enrichment, request)
	return s.Client.EnrichMessages(ctx, refs, request)
}

func TestSearchAndFilterRecoveryResumeRealSourceScan(t *testing.T) {
	for _, command := range []string{"search", "filter"} {
		for _, outputBytes := range []int64{4096, 1} {
			t.Run(fmt.Sprintf("%s/output-%d", command, outputBytes), func(t *testing.T) {
				store := &observedRecoveryStore{Client: goldenWorkflowStore(t)}
				service := mail.NewService(store)
				listed, _ := invokeGoldenWorkflow(t, service, 0, "messages", "list")
				scope := goldenMessagePage(t, listed).Messages[0]
				args := []string{
					"messages", command, "--account", scope.Account, "--mailbox", scope.MailboxRef,
					"--sender", "author@example.com", "--recipient", "sender@icloud.com", "--subject", "Workflow",
					"--after", "1969-12-31", "--before", "1970-01-02", "--read", "false",
					"--flagged", "false", "--attachment", "false", "--limit", "1", "--exact-count",
					"--max-messages", "99", "--with-threading", "--with-excerpt", "--excerpt-length", "7",
					"--fields", "sender,snippet", "--max-bytes", "4096",
				}
				if command == "search" {
					args = append(args, "--query", "Actual workflow")
				}
				first, _ := invokeGoldenWorkflow(t, service, 0, args...)
				firstPage := goldenMessagePage(t, first)
				if firstPage.NextCursor == "" || len(firstPage.Messages) != 1 {
					t.Fatalf("missing scan continuation: %+v", firstPage)
				}
				args = append(args, "--cursor", firstPage.NextCursor, "--max-scan-bytes", "1", "--max-bytes", fmt.Sprint(outputBytes))
				failure, _ := invokeGoldenWorkflow(t, service, 1, args...)
				if failure.Error == nil || failure.Error.Code != "search_budget_too_small" ||
					failure.Error.RequiredBytes == nil || *failure.Error.RequiredBytes <= 1 ||
					failure.Error.Guidance == nil || failure.Error.Guidance.Recovery.Command != "messages."+command {
					t.Fatalf("missing usable scan recovery: %+v", failure.Error)
				}
				wantCode := 0
				if outputBytes == 1 {
					wantCode = 1
				}
				recovery := append(strings.Split(failure.Error.Guidance.Recovery.Command, "."), failure.Error.Guidance.Recovery.Args...)
				resumed, size := invokeGoldenWorkflow(t, service, wantCode, recovery...)
				if len(store.queries) != 3 || len(store.enrichment) != 2 {
					t.Fatalf("query/enrichment calls = %d/%d", len(store.queries), len(store.enrichment))
				}
				want := store.queries[1]
				want.Query.MaxBytes = *failure.Error.RequiredBytes
				if !reflect.DeepEqual(store.queries[2], want) ||
					!reflect.DeepEqual(store.enrichment[1], mail.MessageEnrichmentRequest{
						Threading: true, Excerpt: true, ExcerptLength: 7, ExcerptSourceBudget: enrichmentPageSourceBytes,
					}) {
					t.Fatalf("recovery changed query or enrichment: want=%+v actual=%+v enrichment=%+v", want, store.queries[2], store.enrichment)
				}
				if outputBytes == 1 {
					if resumed.Error == nil || resumed.Error.Code != "output_too_large" || resumed.Data.LimitBytes == nil || *resumed.Data.LimitBytes != 1 {
						t.Fatalf("recovery lost output limit: %+v", resumed)
					}
					return
				}
				page := goldenMessagePage(t, resumed)
				if size > int(outputBytes) || len(page.Messages) != 1 || page.Messages[0].Ref == firstPage.Messages[0].Ref ||
					!page.Messages[0].ThreadingComplete || page.Messages[0].ExcerptSource != mail.ExcerptSourceLocal ||
					page.Messages[0].Excerpt == "" || utf8.RuneCountInString(page.Messages[0].Excerpt) > 7 ||
					resumed.Data.Projection == nil || !reflect.DeepEqual(resumed.Data.Projection.Fields, []string{"sender", "snippet"}) ||
					bytes.Contains(*resumed.Data.Page, []byte(`"subject"`)) {
					t.Fatalf("recovered page lost cursor/enrichment/projection: size=%d page=%+v projection=%+v", size, page, resumed.Data.Projection)
				}
			})
		}
	}
}

func TestSearchAndFilterUnusableBudgetRecoveryIsTerminal(t *testing.T) {
	for _, command := range []string{"search", "filter"} {
		for _, required := range []int64{-1, 0, 1, mail.MaximumSearchMaxBytes + 1} {
			t.Run(fmt.Sprintf("%s/%d", command, required), func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				code := Run(context.Background(), mail.NewService(searchBudgetGateway{requiredBytes: required}),
					[]string{"messages", command, "--max-scan-bytes", "1", "--json"}, &stdout, &stderr)
				var response envelope
				if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if code != 1 || stderr.Len() != 0 || response.Error == nil || response.Error.Guidance == nil ||
					response.Error.Guidance.Retryability != mail.RetryTerminal || response.Error.Guidance.ReplayAllowed ||
					response.Error.Guidance.Recovery.Command != "" || len(response.Error.Guidance.Recovery.Args) != 0 ||
					!strings.Contains(response.Error.Guidance.Recovery.Instruction, "8 GiB") || response.Next == nil || response.Next.Do != "stop" {
					t.Fatalf("unusable recovery: code=%d stderr=%s response=%+v", code, &stderr, response)
				}
			})
		}
	}
}

func TestSearchRecoveryPreservesHumanModeAndMaximumBudget(t *testing.T) {
	query := mail.Query{Limit: 1, MaxMessages: 3, MaxBytes: 1}
	args := buildSearchRecoveryArgs(query, mail.MessageEnrichmentRequest{ExcerptLength: 17}, "sender", 4096,
		false, &searchBudgetCLIError{requiredBytes: mail.MaximumSearchMaxBytes})
	var stdout, stderr bytes.Buffer
	normalized, jsonMode, err := ResolveOutputMode(append([]string{"messages", "filter"}, args...), &stdout, "json")
	if err != nil || jsonMode || !strings.Contains(strings.Join(args, " "), "--json=false") {
		t.Fatalf("recovery lost human mode: %v mode=%t args=%v", err, jsonMode, args)
	}
	gateway := &searchQueryCaptureGateway{}
	code := Run(context.Background(), mail.NewService(gateway), normalized, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || gateway.query.Query.MaxBytes != mail.MaximumSearchMaxBytes ||
		!strings.Contains(stdout.String(), "coverage\t") || strings.Contains(stdout.String(), `"schema_version"`) {
		t.Fatalf("human recovery: code=%d query=%+v stdout=%s stderr=%s", code, gateway.query, &stdout, &stderr)
	}
}

func TestFilterScanBudgetBoundsRejectBeforeGateway(t *testing.T) {
	for _, args := range [][]string{
		{"--max-messages", "-1"}, {"--max-messages", "100001"},
		{"--max-scan-bytes", "-1"}, {"--max-scan-bytes", "8589934593"}, {"--query", "unsupported"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			gateway := &searchQueryCaptureGateway{}
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), mail.NewService(gateway), append([]string{"messages", "filter", "--json"}, args...), &stdout, &stderr)
			if code != 2 || gateway.query.Fingerprint != "" || !strings.Contains(stdout.String(), `"code":"invalid_argument"`) {
				t.Fatalf("invalid filter read: code=%d query=%+v output=%s stderr=%s", code, gateway.query, &stdout, &stderr)
			}
		})
	}
}
