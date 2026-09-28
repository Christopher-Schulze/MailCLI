package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"mailcli/internal/mail"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func captureOutputFixture(t *testing.T, service *mail.Service, args ...string) []byte {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), service, append(args, "--json"), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("%v code=%d stdout=%s stderr=%s", args, code, &stdout, &stderr)
	}
	return stdout.Bytes()
}

func TestRealStorePageEnrichmentFlagMatrix(t *testing.T) {
	service := mail.NewService(goldenWorkflowStore(t))
	for _, command := range []string{"search", "list", "filter"} {
		for mask := 0; mask < 4; mask++ {
			t.Run(command+"/"+strconv.Itoa(mask), func(t *testing.T) {
				args := []string{"messages", command, "--limit", "2"}
				if mask&1 != 0 {
					args = append(args, "--with-threading")
				}
				if mask&2 != 0 {
					args = append(args, "--with-excerpt")
				}
				data := captureOutputFixture(t, service, args...)
				var result struct {
					Data struct {
						Page struct{ Messages []json.RawMessage }
					}
				}
				if err := json.Unmarshal(data, &result); err != nil {
					t.Fatal(err)
				}
				if len(result.Data.Page.Messages) != 2 {
					t.Fatalf("expected 2 real rows: %s", data)
				}
				for _, row := range result.Data.Page.Messages {
					var wrapped struct{ Summary json.RawMessage }
					if err := json.Unmarshal(row, &wrapped); err != nil {
						t.Fatal(err)
					}
					if wrapped.Summary != nil {
						row = wrapped.Summary
					}
					var summary mail.MessageSummary
					if err := json.Unmarshal(row, &summary); err != nil {
						t.Fatal(err)
					}
					if summary.ThreadingComplete != (mask&1 != 0) || (summary.From.Address != "") != (mask&1 != 0) || summary.ExcerptComplete != (mask&2 != 0) || (summary.Excerpt != "") != (mask&2 != 0) {
						t.Fatalf("mask=%d row=%s", mask, row)
					}
					if summary.InReplyTo == nil || summary.References == nil {
						t.Fatalf("null metadata arrays: %s", row)
					}
				}
			})
		}
	}
}

func TestRealStoreMessageExcerptAndOrderedHeaders(t *testing.T) {
	service := mail.NewService(goldenWorkflowStore(t))
	list := captureOutputFixture(t, service, "messages", "list", "--limit", "1")
	var page struct {
		Data struct{ Page mail.MessagePage }
	}
	if err := json.Unmarshal(list, &page); err != nil {
		t.Fatal(err)
	}
	ref := page.Data.Page.Messages[0].Ref
	data := captureOutputFixture(t, service, "messages", "get", "--ref", ref, "--fields", "summary,excerpt", "--excerpt-length", "7")
	var result struct {
		Data struct {
			Message struct {
				Summary         mail.MessageSummary
				Excerpt         string
				ExcerptComplete bool   `json:"excerpt_complete"`
				ExcerptSource   string `json:"excerpt_source"`
				Content         *string
			}
		}
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	message := result.Data.Message
	if message.Excerpt != "Actual " && message.Excerpt != "Actual" {
		t.Fatalf("excerpt=%q", message.Excerpt)
	}
	if utf8.RuneCountInString(message.Excerpt) > 7 || !message.ExcerptComplete || message.ExcerptSource != "local" || message.Content != nil || !message.Summary.ThreadingComplete {
		t.Fatalf("bad excerpt contract: %s", data)
	}
	headers := captureOutputFixture(t, service, "messages", "get", "--ref", ref, "--fields", "header_fields")
	if !strings.Contains(string(headers), `"header_fields":[{"name":"From"`) {
		t.Fatalf("ordered headers=%s", headers)
	}
}

type enrichmentProbeGateway struct {
	testGateway
	calls    int
	lastRefs []string
}

func (g *enrichmentProbeGateway) EnrichMessages(_ context.Context, refs []string, request mail.MessageEnrichmentRequest) ([]mail.MessageSummary, error) {
	g.calls++
	g.lastRefs = append([]string(nil), refs...)
	summaries := make([]mail.MessageSummary, len(refs))
	for index, ref := range refs {
		summaries[index] = mail.MessageSummary{MessageID: "<" + ref + "@example.com>", ThreadingComplete: request.Threading}
		if request.Excerpt {
			summaries[index].Excerpt, summaries[index].ExcerptSource = "text", mail.ExcerptSourceLocal
		}
	}
	return summaries, nil
}

func enrichmentProbeRows(count int, size int64) []*mail.MessageSummary {
	rows := make([]*mail.MessageSummary, count)
	for index := range rows {
		rows[index] = &mail.MessageSummary{Ref: "row" + strconv.Itoa(index), Size: size}
	}
	return rows
}

func TestEnrichSummariesKeepsOrderInOnePageRequest(t *testing.T) {
	gateway := &enrichmentProbeGateway{}
	rows := enrichmentProbeRows(12, 1024)
	request := mail.MessageEnrichmentRequest{Threading: true, ExcerptLength: mail.DefaultExcerptLength}
	if err := enrichSummaries(context.Background(), mail.NewService(gateway), rows, request); err != nil {
		t.Fatal(err)
	}
	for index, row := range rows {
		if row.MessageID != "<row"+strconv.Itoa(index)+"@example.com>" || !row.ThreadingComplete || row.EnrichmentError != "" {
			t.Fatalf("row %d = %+v", index, row)
		}
	}
	if gateway.calls != 1 || len(gateway.lastRefs) != len(rows) {
		t.Fatalf("gateway calls = %d with %d refs, want one call with %d refs", gateway.calls, len(gateway.lastRefs), len(rows))
	}
}

func TestEnrichSummariesStopsAtThePageSourceBudget(t *testing.T) {
	rows := enrichmentProbeRows(40, mail.MaximumExcerptSourceBytes)
	request := mail.MessageEnrichmentRequest{Excerpt: true, ExcerptLength: mail.DefaultExcerptLength}
	gateway := &enrichmentProbeGateway{}
	if err := enrichSummaries(context.Background(), mail.NewService(gateway), rows, request); err != nil {
		t.Fatal(err)
	}
	withinBudget := int(enrichmentPageSourceBytes / mail.MaximumExcerptSourceBytes)
	if len(gateway.lastRefs) != withinBudget {
		t.Fatalf("gateway read %d refs, want the %d within budget", len(gateway.lastRefs), withinBudget)
	}
	for index, row := range rows {
		enriched := row.ExcerptSource == mail.ExcerptSourceLocal && row.EnrichmentError == ""
		skipped := row.ExcerptSource == "" && row.EnrichmentError == enrichmentBudgetExhausted
		if (index < withinBudget && !enriched) || (index >= withinBudget && !skipped) {
			t.Fatalf("row %d of %d-row budget = %+v", index, withinBudget, row)
		}
	}
}

func TestExcerptLengthHelpNamesTheCommandsSelector(t *testing.T) {
	for command, want := range map[string]string{"get": "requires --fields excerpt", "search": "requires --with-excerpt", "list": "requires --with-excerpt"} {
		var stdout, stderr bytes.Buffer
		Run(context.Background(), newTestService(), []string{"messages", command, "--help"}, &stdout, &stderr)
		help := stdout.String() + stderr.String()
		if !strings.Contains(help, want) || (command == "get" && strings.Contains(help, "--with-excerpt")) {
			t.Fatalf("messages %s help lacks %q: %s", command, want, help)
		}
	}
}
