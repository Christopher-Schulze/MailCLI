package mail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

func createDraftListFixture(t testing.TB, count, bodyBytes int) (*Service, []string) {
	t.Helper()
	service := NewServiceWithDraftRoot(nil, filepath.Join(t.TempDir(), "drafts"))
	refs := make([]string, 0, count)
	body := strings.Repeat("x", bodyBytes)
	for range count {
		draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
			To: []Recipient{{Address: "recipient@example.com"}}, Subject: "Listed", Body: body,
		}})
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, draft.Ref)
	}
	sort.Strings(refs)
	return service, refs
}

func TestDraftListPagesCoverLargeDirectoryExactly(t *testing.T) {
	service, refs := createDraftListFixture(t, 2049, 32)
	var observed []string
	request := ListDraftsRequest{}
	var revision string
	for number := 0; ; number++ {
		page, err := service.ListDrafts(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		wantLimit := request.Limit
		if wantLimit == 0 {
			wantLimit = DefaultDraftListLimit
		}
		if page.Pagination.Limit != wantLimit || len(page.Drafts) != min(wantLimit, len(refs)-len(observed)) {
			t.Fatalf("page %d count=%d, limit=%d, observed=%d", number, len(page.Drafts), page.Pagination.Limit, len(observed))
		}
		if number == 0 {
			revision = page.Pagination.Revision
		}
		if revision == "" || page.Pagination.Revision != revision {
			t.Fatalf("unchanged collection revision changed on page %d", number)
		}
		for _, draft := range page.Drafts {
			if draft.Subject != "Listed" || draft.StateError != "" {
				t.Fatalf("unexpected summary: %+v", draft)
			}
			observed = append(observed, draft.Ref)
		}
		if page.Pagination.NextCursor == "" {
			break
		}
		if len(observed) >= len(refs) || page.Pagination.NextCursor == request.Cursor {
			t.Fatal("continuation did not advance or points beyond the final entry")
		}
		request = ListDraftsRequest{Limit: MaximumDraftListLimit, Cursor: page.Pagination.NextCursor}
		if number%2 == 0 {
			request.Limit = 37
		}
	}
	if !slices.Equal(observed, refs) {
		t.Fatalf("pagination lost, repeated, or reordered records: got %d, want %d", len(observed), len(refs))
	}
	assertNoDraftLockFiles(t, service.draftRoot)
}

func TestDraftListRejectsInvalidRequestsBeforeCreatingStorage(t *testing.T) {
	for _, test := range []struct {
		name    string
		request ListDraftsRequest
		code    string
	}{
		{"negative limit", ListDraftsRequest{Limit: -1}, "invalid_argument"},
		{"excessive limit", ListDraftsRequest{Limit: 201}, "invalid_argument"},
		{"malformed cursor", ListDraftsRequest{Cursor: "wrong cursor"}, "invalid_cursor"},
		{"oversized cursor", ListDraftsRequest{Cursor: strings.Repeat("x", 513)}, "invalid_cursor"},
		{"wrong version", ListDraftsRequest{Cursor: base64.RawURLEncoding.EncodeToString([]byte("dl2\n" + strings.Repeat("a", 64) + "\ndraft_test"))}, "invalid_cursor"},
		{"path cursor", ListDraftsRequest{Cursor: base64.RawURLEncoding.EncodeToString([]byte("dl1\n" + strings.Repeat("a", 64) + "\ndraft_../test"))}, "invalid_cursor"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "absent")
			service := NewServiceWithDraftRoot(nil, root)
			page, err := service.ListDrafts(context.Background(), test.request)
			if errorCode(err) != test.code || len(page.Drafts) != 0 {
				t.Fatalf("page=%+v, error=%v", page, err)
			}
			if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid request touched storage: %v", err)
			}
		})
	}
}

func TestDraftListCursorRejectsCollectionChanges(t *testing.T) {
	for _, mutation := range []string{"create", "update", "discard", "claim", "root replacement", "other root"} {
		t.Run(mutation, func(t *testing.T) {
			service, refs := createDraftListFixture(t, 3, 32)
			first, err := service.ListDrafts(context.Background(), ListDraftsRequest{Limit: 1})
			if err != nil || first.Pagination.NextCursor == "" {
				t.Fatalf("first page=%+v, error=%v", first, err)
			}
			draft, err := service.GetDraft(refs[0])
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "create":
				_, err = service.CreateDraft(CreateDraftRequest{Input: DraftInput{To: draft.To, Body: "added"}})
			case "update":
				_, err = service.UpdateDraft(UpdateDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision, Input: DraftInput{To: draft.To, Body: "changed"}})
			case "discard":
				err = service.DiscardDraft(draft.Ref)
			case "claim":
				_, err = beginSendAttempt(service.draftRoot, draft.Ref, "", "")
			case "root replacement":
				var original os.FileInfo
				original, err = os.Stat(service.draftRoot)
				if err == nil {
					err = os.Rename(service.draftRoot, service.draftRoot+"-retained")
				}
				if err == nil {
					err = os.Mkdir(service.draftRoot, 0o700)
				}
				if err == nil {
					err = os.Chtimes(service.draftRoot, original.ModTime(), original.ModTime())
				}
			case "other root":
				service, _ = createDraftListFixture(t, 3, 32)
			}
			if err != nil {
				t.Fatal(err)
			}
			page, err := service.ListDrafts(context.Background(), ListDraftsRequest{Cursor: first.Pagination.NextCursor})
			if errorCode(err) != "invalid_cursor" || len(page.Drafts) != 0 || page.Pagination.NextCursor != "" {
				t.Fatalf("changed collection accepted: page=%+v, error=%v", page, err)
			}
		})
	}
}

// Context checks provide a deterministic IO-boundary trigger while using the
// real cancellation state and real filesystem operations underneath.
type draftListBoundaryContext struct {
	context.Context
	checks  int
	trigger int
	action  func()
}

func (c *draftListBoundaryContext) Err() error {
	c.checks++
	if c.checks == c.trigger {
		c.action()
	}
	return c.Context.Err()
}

func TestDraftListCancelsAndRejectsConcurrentChanges(t *testing.T) {
	for _, test := range []struct {
		name    string
		trigger int
		change  bool
	}{
		{"cancel before IO", 1, false},
		{"cancel during directory scan", 3, false},
		{"cancel during record reads", 8, false},
		{"change during directory scan", 3, true},
		{"change during record reads", 8, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, refs := createDraftListFixture(t, 300, 64*1024)
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &draftListBoundaryContext{Context: base, trigger: test.trigger, action: cancel}
			if test.change {
				ctx.action = func() {
					if err := os.Rename(filepath.Join(service.draftRoot, refs[0]+".json"), filepath.Join(service.draftRoot, "retained.json")); err != nil {
						t.Fatal(err)
					}
				}
			}
			started := time.Now()
			page, err := service.ListDrafts(ctx, ListDraftsRequest{Limit: 2})
			wantCode := "draft_operation_canceled"
			if test.change {
				wantCode = "invalid_cursor"
			}
			if ctx.checks < test.trigger || errorCode(err) != wantCode || len(page.Drafts) != 0 {
				t.Fatalf("checks=%d, page=%+v, error=%v, want %s", ctx.checks, page, err, wantCode)
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("bounded interrupted page took %s", elapsed)
			}
		})
	}
}

func TestDraftListKeepsUnreadableAndMalformedRecordsVisible(t *testing.T) {
	service, refs := createDraftListFixture(t, 1, 1024*1024)
	for index, content := range []string{`{"body":"confidential\q"}`, `{"subject":"confidential","to":"wrong type"}`} {
		ref := fmt.Sprintf("draft_%024d", index)
		if err := os.WriteFile(filepath.Join(service.draftRoot, ref+".json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	fifoRef := "draft_000000000000000000000002"
	if err := syscall.Mkfifo(filepath.Join(service.draftRoot, fifoRef+".json"), 0o600); err != nil {
		t.Fatal(err)
	}
	refs = append(refs, fifoRef)
	page, err := service.ListDrafts(context.Background(), ListDraftsRequest{})
	if err != nil || len(page.Drafts) != len(refs) {
		t.Fatalf("page=%+v, error=%v", page, err)
	}
	errorsFound := 0
	for _, draft := range page.Drafts {
		if draft.StateError != "" {
			errorsFound++
		} else if draft.Ref != refs[0] || draft.Subject != "Listed" {
			t.Fatalf("incorrect intact draft summary: %+v", draft)
		}
	}
	payload, err := json.Marshal(page)
	if err != nil || errorsFound != 3 || strings.Contains(string(payload), "confidential") || strings.Contains(string(payload), `"body":`) {
		t.Fatalf("state errors=%d, serialization error=%v, output=%s", errorsFound, err, payload)
	}
}

func TestDraftPruneCollectsEveryPageBeforeMutating(t *testing.T) {
	service, refs := createDraftListFixture(t, MaximumDraftListLimit+1, 8)
	for _, ref := range refs {
		ageDraftFile(t, service.draftRoot, ref, 40)
	}
	result, err := service.PruneDraftsContext(context.Background(), PruneDraftsRequest{OlderThan: 30 * 24 * time.Hour, Confirm: true})
	if err != nil || !slices.Equal(result.Removed, refs) || len(result.Candidates) != len(refs) {
		t.Fatalf("pruned=%d, candidates=%d, want=%d, error=%v", len(result.Removed), len(result.Candidates), len(refs), err)
	}
	page, err := service.ListDrafts(context.Background(), ListDraftsRequest{})
	if err != nil || len(page.Drafts) != 0 || page.Drafts == nil {
		t.Fatalf("empty page=%+v, error=%v", page, err)
	}
}

func BenchmarkDraftListPage(b *testing.B) {
	for _, count := range []int{1, 2049} {
		service, _ := createDraftListFixture(b, count, 32)
		b.Run(fmt.Sprintf("records-%d", count), func(b *testing.B) {
			request := ListDraftsRequest{Limit: 25}
			page, err := service.ListDrafts(context.Background(), request)
			if err != nil {
				b.Fatal(err)
			}
			output, err := json.Marshal(page)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			b.ReportMetric(float64(len(output)), "output_B/op")
			b.ReportMetric(float64(len(page.Drafts)), "records/op")
			for range b.N {
				page, err := service.ListDrafts(context.Background(), request)
				if err != nil || len(page.Drafts) != min(count, request.Limit) {
					b.Fatalf("page count=%d, error=%v", len(page.Drafts), err)
				}
			}
		})
	}
}
