package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"mailcli/internal/mail"
)

type deadlinePageGateway struct {
	*pageProjectionGateway
	readErr error
	calls   int
	enrich  func(context.Context, []string) ([]mail.MessageSummary, error)
}

func (g *deadlinePageGateway) ListMessages(ctx context.Context, request mail.ListMessagesRequest) (mail.MessagePage, error) {
	if g.readErr != nil {
		return mail.MessagePage{}, g.readErr
	}
	return g.pageProjectionGateway.ListMessages(ctx, request)
}

func (g *deadlinePageGateway) SearchMessages(ctx context.Context, query mail.PreparedQuery) (mail.SearchPage, error) {
	if g.readErr != nil {
		return mail.SearchPage{}, g.readErr
	}
	return g.pageProjectionGateway.SearchMessages(ctx, query)
}

func (g *deadlinePageGateway) EnrichMessages(ctx context.Context, refs []string, _ mail.MessageEnrichmentRequest) ([]mail.MessageSummary, error) {
	g.calls++
	return g.enrich(ctx, refs)
}

func deadlinePageMetadata(ctx context.Context, refs []string) ([]mail.MessageSummary, error) {
	rows := make([]mail.MessageSummary, len(refs))
	for index, ref := range refs {
		rows[index] = mail.MessageSummary{Ref: ref, ExcerptSource: mail.ExcerptSourceUnavailable, EnrichmentError: "operation_timeout"}
	}
	rows[0].ThreadingComplete, rows[0].ExcerptComplete = true, true
	rows[0].InReplyTo = []string{"<sent@example.com>"}
	rows[0].Excerpt, rows[0].ExcerptSource, rows[0].EnrichmentError = "completed first", mail.ExcerptSourceLocal, ""
	rows[1].ThreadingComplete = true
	rows[2].Excerpt, rows[2].ExcerptSource, rows[2].ExcerptComplete = "completed third", mail.ExcerptSourceLocal, true
	<-ctx.Done()
	return rows, ctx.Err()
}

func TestEnrichmentDeadlinePreservesPageProjectionAndBudget(t *testing.T) {
	for _, command := range []string{"messages.list", "messages.filter", "messages.search"} {
		for _, fields := range []string{"sender", "all"} {
			t.Run(command+"/"+fields, func(t *testing.T) {
				gateway := &deadlinePageGateway{pageProjectionGateway: newPageProjectionGateway(), enrich: deadlinePageMetadata}
				gateway.listPage.Messages = gateway.listPage.Messages[:3]
				gateway.searchPage.Messages = gateway.searchPage.Messages[:3]
				gateway.searchPage.Coverage.Complete = false
				gateway.searchPage.Coverage.SourcesComplete = true
				service := mail.NewService(gateway)
				parent := context.Background()
				var rows []*mail.MessageSummary
				list, search := mail.MessagePage{}, mail.SearchPage{}
				if command == "messages.list" {
					var err error
					list, err = service.ListMessages(parent, mail.ListMessagesRequest{MailboxRef: "mbx_ref", Limit: 3})
					if err != nil {
						t.Fatal(err)
					}
					for index := range list.Messages {
						rows = append(rows, &list.Messages[index])
					}
				} else {
					var err error
					search, err = service.SearchMessages(parent, mail.Query{Limit: 3})
					if err != nil {
						t.Fatal(err)
					}
					for index := range search.Messages {
						rows = append(rows, &search.Messages[index].Summary)
					}
				}
				ctx, cancel := context.WithTimeout(parent, 10*time.Millisecond)
				defer cancel()
				if err := enrichSummaries(parent, ctx, service, rows, mail.MessageEnrichmentRequest{Threading: true, Excerpt: true, ExcerptLength: 240}); err != nil {
					t.Fatalf("successful base page discarded: %v", err)
				}
				selected := map[string]struct{}{fields: {}}
				var page *json.RawMessage
				if command == "messages.list" {
					page = projectMessageListPage(list, selected)
				} else {
					page = projectSearchPage(search, selected)
				}
				data := responseData{Page: page}
				var stdout bytes.Buffer
				if code := writeBoundedListSuccess(&stdout, command, data, defaultJSONOutputBytes, listOutputRecovery(command)); code != 0 {
					t.Fatalf("write preserved page: code=%d output=%s", code, &stdout)
				}
				var projected struct {
					Messages   []json.RawMessage `json:"messages"`
					NextCursor string            `json:"next_cursor"`
					Coverage   mail.SearchCoverage
				}
				if err := json.Unmarshal(pageFromEnvelope(t, stdout.String()), &projected); err != nil {
					t.Fatal(err)
				}
				wantCursor := gateway.listPage.NextCursor
				if command != "messages.list" {
					wantCursor = gateway.searchPage.NextCursor
					if projected.Coverage != gateway.searchPage.Coverage {
						t.Fatalf("base coverage changed: %+v", projected.Coverage)
					}
				}
				if len(projected.Messages) != 3 || projected.NextCursor != wantCursor || gateway.calls != 1 {
					t.Fatalf("page identity/order/continuation lost: %s", &stdout)
				}
				for index, raw := range projected.Messages {
					if command != "messages.list" {
						var wrapped struct{ Summary json.RawMessage }
						if err := json.Unmarshal(raw, &wrapped); err != nil {
							t.Fatal(err)
						}
						raw = wrapped.Summary
					}
					var row mail.MessageSummary
					if err := json.Unmarshal(raw, &row); err != nil {
						t.Fatal(err)
					}
					if row.Ref != rows[index].Ref || row.ThreadingComplete != (index < 2) || row.ExcerptComplete != (index != 1) || row.InReplyTo == nil || row.References == nil ||
						(index == 0 && row.EnrichmentError != "") || (index > 0 && row.EnrichmentError != "operation_timeout") {
						t.Fatalf("row %d partial evidence lost: %s", index, raw)
					}
				}
				stdout.Reset()
				if code := writeBoundedListSuccess(&stdout, command, data, 1, listOutputRecovery(command)); code != 1 ||
					!strings.Contains(stdout.String(), `"code":"output_too_large"`) || strings.Contains(stdout.String(), `"messages":[`) {
					t.Fatalf("deadline bypassed output budget: code=%d output=%s", code, &stdout)
				}
			})
		}
	}
}

func TestEnrichmentDeadlineDoesNotSwallowInvalidPartialsOrParentStop(t *testing.T) {
	for _, test := range []string{"parent canceled", "parent deadline", "wrong length", "wrong ref", "combined error"} {
		t.Run(test, func(t *testing.T) {
			parent := context.Background()
			var cancelParent context.CancelFunc
			switch test {
			case "parent canceled":
				parent, cancelParent = context.WithCancel(parent)
			case "parent deadline":
				parent, cancelParent = context.WithTimeout(parent, 10*time.Millisecond)
			default:
				cancelParent = func() {}
			}
			defer cancelParent()
			ctx, cancel := context.WithTimeout(parent, 20*time.Millisecond)
			defer cancel()
			gateway := &deadlinePageGateway{pageProjectionGateway: newPageProjectionGateway(), enrich: func(ctx context.Context, refs []string) ([]mail.MessageSummary, error) {
				if test == "parent canceled" {
					cancelParent()
				}
				rows, err := deadlinePageMetadata(ctx, refs)
				switch test {
				case "wrong length":
					rows = rows[:1]
				case "wrong ref":
					rows[2].Ref = "wrong"
				case "combined error":
					err = errors.Join(err, errors.New("close failed"))
				}
				return rows, err
			}}
			rows := []mail.MessageSummary{{Ref: "first"}, {Ref: "second"}, {Ref: "third"}}
			before := append([]mail.MessageSummary(nil), rows...)
			err := enrichSummaries(parent, ctx, mail.NewService(gateway), []*mail.MessageSummary{&rows[0], &rows[1], &rows[2]}, mail.MessageEnrichmentRequest{Threading: true, Excerpt: true, ExcerptLength: 240})
			if err == nil || (test != "parent deadline" && !reflect.DeepEqual(rows, before)) {
				t.Fatalf("failure swallowed or invalid partial applied: rows=%+v error=%v", rows, err)
			}
			if test == "parent canceled" && !errors.Is(err, context.Canceled) || test == "parent deadline" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("parent stop changed: %v", err)
			}
		})
	}
}

func TestEnrichmentDeadlineCannotConvertBaseReadFailureOrLiveDeadlineToSuccess(t *testing.T) {
	for _, command := range []string{"list", "filter", "search"} {
		for _, failBase := range []bool{true, false} {
			t.Run(command+"/base="+map[bool]string{true: "failed", false: "successful"}[failBase], func(t *testing.T) {
				gateway := &deadlinePageGateway{pageProjectionGateway: newPageProjectionGateway(), enrich: func(_ context.Context, refs []string) ([]mail.MessageSummary, error) {
					return make([]mail.MessageSummary, len(refs)), context.DeadlineExceeded
				}}
				if failBase {
					gateway.readErr = context.DeadlineExceeded
				}
				var stdout, stderr bytes.Buffer
				args := []string{"messages", command, "--with-threading", "--with-excerpt", "--fields", "sender", "--json"}
				code := Run(context.Background(), mail.NewService(gateway), args, &stdout, &stderr)
				if code != 1 || stderr.Len() != 0 || !strings.Contains(stdout.String(), `"ok":false`) || strings.Contains(stdout.String(), `"messages":[`) ||
					(failBase && gateway.calls != 0) || (!failBase && gateway.calls != 1) {
					t.Fatalf("base failure/live-context deadline swallowed: code=%d calls=%d output=%s stderr=%s", code, gateway.calls, &stdout, &stderr)
				}
			})
		}
	}
}
