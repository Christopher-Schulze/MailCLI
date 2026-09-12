package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDraftSummaryProjectionValidatesSkippedStrings(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		valid bool
	}{
		{"escaped body", `{"body":"text\n\u00e4\"\\\/","subject":"retained"}`, true},
		{"escaped key", `{"b\u006fdy":"omitted","subject":"retained"}`, true},
		{"nested materialization", `{"attempt":{"materialized":{"body":"omitted","from":"sender"}}}`, true},
		{"body-looking subject", `{"subject":"body: \"retained\"","body":"omitted"}`, true},
		{"lone surrogate compatibility", `{"body":"\ud800"}`, true},
		{"invalid escape", `{"body":"secret\q"}`, false},
		{"invalid unicode escape", `{"body":"secret\u12xx"}`, false},
		{"truncated escape", `{"body":"secret\`, false},
		{"raw newline", "{\"body\":\"secret\ntext\"}", false},
		{"truncated body", `{"body":"secret`, false},
		{"truncated object", `{"body":"secret"`, false},
		{"trailing value", `{"body":"secret"} true`, false},
		{"separated number", `{"body":1 2}`, false},
		{"wrong delimiters", `{"body":["secret"}}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			projected, _, err := projectDraftSummaryJSON(context.Background(), strings.NewReader(test.input), int64(len(test.input)))
			valid := err == nil && json.Valid(projected)
			if valid != test.valid {
				t.Fatalf("projection validity = %t, want %t, error = %v", valid, test.valid, err)
			}
			if valid && strings.Contains(string(projected), "omitted") {
				t.Fatal("projection retained hidden body")
			}
		})
	}
}

func TestDraftSummaryProjectionBoundsMetadataAndReadsBodyFully(t *testing.T) {
	for _, size := range []int{1, 1024 * 1024} {
		payload := `{"body":"` + strings.Repeat("b", size) + `","subject":"retained"}`
		projection, readBytes, err := projectDraftSummaryJSON(context.Background(), strings.NewReader(payload), int64(len(payload)))
		if err != nil || string(projection) != `{"body":"","subject":"retained"}` || readBytes != int64(len(payload)) {
			t.Fatalf("size %d: projected=%q, read=%d, error=%v", size, projection, readBytes, err)
		}
	}
	payload := `{"subject":"` + strings.Repeat("s", maximumDraftSummaryBytes) + `"}`
	if _, _, err := projectDraftSummaryJSON(context.Background(), strings.NewReader(payload), int64(len(payload))); err == nil {
		t.Fatal("oversized metadata accepted")
	}
	payload = `{"body":"` + strings.Repeat("b", int(maximumDraftStateBytes)) + `"}`
	if _, _, err := projectDraftSummaryJSON(context.Background(), strings.NewReader(payload), int64(len(payload))); err == nil {
		t.Fatal("oversized record accepted")
	}
}

func TestDraftSummaryProjectionCancelsDuringLargeBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	payload := `{"body":"` + strings.Repeat("b", 1024*1024) + `"}`
	reads := 0
	reader := attachmentFingerprintReader{ctx: ctx, reader: strings.NewReader(payload), afterRead: func(count int) {
		reads += count
		if reads >= 8192 {
			cancel()
		}
	}}
	_, readBytes, err := projectDraftSummaryJSON(ctx, reader, int64(len(payload)))
	if !errors.Is(err, context.Canceled) || readBytes >= int64(len(payload)) || readBytes != int64(reads) {
		t.Fatalf("mid-body cancellation: bytes=%d, error=%v", readBytes, err)
	}
}

func TestDraftSummaryMatchesFullRecordAndRetainsClaimValidation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(nil, root)
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Summary", Body: strings.Repeat("x", 1024*1024),
	}})
	if err != nil {
		t.Fatal(err)
	}
	summary, err := readDraftSummary(context.Background(), draft.Ref, &draftStorage{rootName: root})
	if err != nil || !reflect.DeepEqual(summary, draftSummaryFrom(draft)) {
		t.Fatalf("summary differs from full record: %+v, %v", summary, err)
	}
	attempt := writeLegacyDraftSaveAttempt(t, root, draft.Ref, &SendObservationBaseline{
		StoreUUID: "store", MaximumRowID: 1, CapturedUnix: 1, SentMailboxIDs: []int64{1},
	})
	body := strings.Repeat("confidential", 100000)
	attempt.Materialized = &SendMaterialization{From: "sender@example.com",
		To: []Recipient{{Address: "recipient@example.com"}}, Subject: "secret subject", Body: &body}
	if err := replaceDraftSaveAttempt(root, draft.Ref, attempt); err != nil {
		t.Fatal(err)
	}
	summary, err = readDraftSummary(context.Background(), draft.Ref, &draftStorage{rootName: root})
	if err != nil || summary.SaveAttempt == nil {
		t.Fatalf("legacy claim projection = %+v, %v", summary, err)
	}
	encoded, err := json.Marshal(summary)
	if err != nil || bytes.Contains(encoded, []byte("confidential")) || bytes.Contains(encoded, []byte("secret subject")) {
		t.Fatalf("legacy content leaked into summary: %v", err)
	}
	attempt.Materialized.From = "invalid address"
	// Encode a corrupt persisted claim directly, bypassing the writer's input
	// validation so this exercises the production read-side validator.
	encoded, err = json.Marshal(storedDraftSaveAttempt{Version: 1, DraftRef: draft.Ref, Attempt: attempt})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, draft.Ref+".save-claim"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readDraftSummary(context.Background(), draft.Ref, &draftStorage{rootName: root}); err == nil {
		t.Fatal("summary accepted a corrupt claim envelope")
	}
}

func FuzzDraftSummaryProjectionPreservesJSONValidity(f *testing.F) {
	for _, seed := range []string{`{"body":"large","subject":"small"}`, `{"body":["wrong type"]}`, `{"body":"\u0000"}`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > 64*1024 {
			return
		}
		projection, _, err := projectDraftSummaryJSON(context.Background(), strings.NewReader(input), int64(len(input)))
		if err == nil && json.Valid(projection) && !json.Valid([]byte(input)) {
			t.Fatal("projection made malformed JSON valid")
		}
	})
}

func BenchmarkDraftSummaryBodies(b *testing.B) {
	for _, fixture := range []struct {
		name string
		size int
	}{{"tiny", 1}, {"1MiB", 1024 * 1024}} {
		b.Run(fixture.name, func(b *testing.B) {
			root := filepath.Join(b.TempDir(), "drafts")
			service := NewServiceWithDraftRoot(nil, root)
			draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
				To: []Recipient{{Address: "recipient@example.com"}}, Subject: "Summary", Body: strings.Repeat("x", fixture.size),
			}})
			if err != nil {
				b.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(root, draft.Ref+".json"))
			if err != nil {
				b.Fatal(err)
			}
			summaryOutput, err := json.Marshal(draftSummaryFrom(draft))
			if err != nil {
				b.Fatal(err)
			}
			page, err := service.ListDrafts(context.Background(), ListDraftsRequest{Limit: 1})
			if err != nil {
				b.Fatal(err)
			}
			pageOutput, err := json.Marshal(page)
			if err != nil {
				b.Fatal(err)
			}
			for _, mode := range []string{"full-record-baseline", "streaming-summary", "paged-service"} {
				b.Run(mode, func(b *testing.B) {
					b.ReportAllocs()
					b.SetBytes(info.Size())
					b.ReportMetric(float64(info.Size()), "file_B/op")
					outputBytes := len(summaryOutput)
					if mode == "paged-service" {
						outputBytes = len(pageOutput)
					}
					b.ReportMetric(float64(outputBytes), "output_B/op")
					for range b.N {
						var summary DraftSummary
						switch mode {
						case "full-record-baseline":
							loaded, readErr := loadDraftDocument(root, draft.Ref)
							if readErr == nil {
								readErr = attachDraftAttempts(root, draft.Ref, &loaded)
							}
							if readErr != nil {
								b.Fatal(readErr)
							}
							summary = draftSummaryFrom(loaded)
						case "paged-service":
							page, readErr := service.ListDrafts(context.Background(), ListDraftsRequest{Limit: 1})
							if readErr != nil || len(page.Drafts) != 1 {
								b.Fatalf("page count=%d, error=%v", len(page.Drafts), readErr)
							}
							summary = page.Drafts[0]
						default:
							var readErr error
							summary, readErr = readDraftSummary(context.Background(), draft.Ref, &draftStorage{rootName: root})
							if readErr != nil {
								b.Fatal(readErr)
							}
						}
						if summary.Ref != draft.Ref {
							b.Fatal(io.ErrUnexpectedEOF)
						}
					}
				})
			}
		})
	}
}
