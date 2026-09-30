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
					threadingKeys := strings.Contains(string(row), `"in_reply_to"`) || strings.Contains(string(row), `"references"`) ||
						strings.Contains(string(row), `"threading_complete"`) || strings.Contains(string(row), `"from"`)
					excerptKeys := strings.Contains(string(row), `"excerpt"`) || strings.Contains(string(row), `"excerpt_source"`)
					if threadingKeys != (mask&1 != 0) || excerptKeys != (mask&2 != 0) {
						t.Fatalf("mask=%d has unrequested or missing enrichment keys: %s", mask, row)
					}
					if mask&1 != 0 && (summary.InReplyTo == nil || summary.References == nil) {
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
	calls       int
	lastRefs    []string
	lastRequest mail.MessageEnrichmentRequest
}

func (g *enrichmentProbeGateway) EnrichMessages(_ context.Context, refs []string, request mail.MessageEnrichmentRequest) ([]mail.MessageSummary, error) {
	g.calls++
	g.lastRefs = append([]string(nil), refs...)
	g.lastRequest = request
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

func TestEnrichSummariesPassesThePageSourceBudgetAndEveryRow(t *testing.T) {
	for _, test := range []struct {
		name       string
		request    mail.MessageEnrichmentRequest
		wantBudget int64
	}{
		{"excerpt", mail.MessageEnrichmentRequest{Excerpt: true, ExcerptLength: mail.DefaultExcerptLength}, enrichmentPageSourceBytes},
		{"threading and excerpt", mail.MessageEnrichmentRequest{Threading: true, Excerpt: true, ExcerptLength: mail.DefaultExcerptLength}, enrichmentPageSourceBytes},
		{"threading only", mail.MessageEnrichmentRequest{Threading: true, ExcerptLength: mail.DefaultExcerptLength}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			rows := enrichmentProbeRows(40, mail.MaximumExcerptSourceBytes)
			gateway := &enrichmentProbeGateway{}
			if err := enrichSummaries(context.Background(), mail.NewService(gateway), rows, test.request); err != nil {
				t.Fatal(err)
			}
			if gateway.calls != 1 || len(gateway.lastRefs) != len(rows) {
				t.Fatalf("gateway calls = %d with %d refs, want one call with all %d rows", gateway.calls, len(gateway.lastRefs), len(rows))
			}
			if gateway.lastRequest.ExcerptSourceBudget != test.wantBudget {
				t.Fatalf("budget = %d, want %d", gateway.lastRequest.ExcerptSourceBudget, test.wantBudget)
			}
			for index, row := range rows {
				if row.EnrichmentError != "" || (test.request.Threading && !row.ThreadingComplete) {
					t.Fatalf("row %d = %+v", index, row)
				}
			}
		})
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

type threadingHeaderGateway struct {
	testGateway
	headers string
}

func (g *threadingHeaderGateway) GetMessage(context.Context, string) (mail.Message, error) {
	return mail.Message{Summary: mail.MessageSummary{Ref: "msg_ref", Sender: "unchanged"}, Headers: g.headers}, nil
}

func TestGetProjectionRetainsPartialThreadingEvidence(t *testing.T) {
	for _, test := range []struct{ name, headers string }{
		{"discarded spans", "From: First <Local@EXAMPLE.COM>\r\nReferences: lost <a@b> between <c@d> trailing\r\n"},
		{"malformed later sender", "From: First <Local@EXAMPLE.COM>\r\nFrom: malformed\r\nReferences: <a@b> <c@d>\r\n"},
		{"ambiguous sender", "From: First <Local@EXAMPLE.COM>, other@example.com\r\nReferences: <a@b> <c@d>\r\n"},
	} {
		for _, fields := range []string{"summary", "summary,header_fields"} {
			t.Run(test.name+"/"+fields, func(t *testing.T) {
				service := mail.NewService(&threadingHeaderGateway{headers: test.headers + "\r\n"})
				data := captureOutputFixture(t, service, "messages", "get", "--ref", "msg_ref", "--fields", fields)
				var response struct {
					OK   bool `json:"ok"`
					Data struct {
						Message struct {
							Summary struct {
								Complete   *bool          `json:"threading_complete"`
								References []string       `json:"references"`
								InReplyTo  []string       `json:"in_reply_to"`
								From       mail.Recipient `json:"from"`
								Sender     string         `json:"sender"`
							}
						}
					}
				}
				if err := json.Unmarshal(data, &response); err != nil {
					t.Fatal(err)
				}
				summary := response.Data.Message.Summary
				if !response.OK || summary.Complete == nil || *summary.Complete || len(summary.References) != 2 || summary.References[0] != "<a@b>" || summary.References[1] != "<c@d>" || summary.InReplyTo == nil || len(summary.InReplyTo) != 0 || summary.From != (mail.Recipient{Name: "First", Address: "Local@example.com"}) || summary.Sender != "unchanged" {
					t.Fatalf("partial metadata lost or overstated: %s", data)
				}
			})
		}
	}
}
