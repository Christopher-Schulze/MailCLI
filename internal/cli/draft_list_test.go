package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

func TestDraftListCLIContinuationAndErrors(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(nil, filepath.Join(t.TempDir(), "drafts"))
	var want []string
	for range 3 {
		draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
			To: []mail.Recipient{{Address: "recipient@example.com"}}, Subject: "Listed", Body: strings.Repeat("private", 10000),
		}})
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, draft.Ref)
	}
	sort.Strings(want)
	var refs []string
	var cursor, revision string
	for pageNumber := 0; pageNumber < 2; pageNumber++ {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), service, []string{"drafts", "list", "--limit", "2", "--cursor", cursor, "--json"}, &stdout, &stderr)
		var response struct {
			OK   bool `json:"ok"`
			Data struct {
				Drafts []draftListEntry     `json:"drafts"`
				Page   mail.DraftPagination `json:"page"`
			} `json:"data"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || code != 0 || !response.OK || stderr.Len() != 0 {
			t.Fatalf("code=%d, decode=%v, stdout=%s, stderr=%s", code, err, stdout.String(), stderr.String())
		}
		if len(response.Data.Drafts) != 2-pageNumber || response.Data.Page.Limit != 2 || strings.Contains(stdout.String(), "private") {
			t.Fatalf("incorrect or unredacted page: %s", stdout.String())
		}
		if pageNumber == 0 {
			revision = response.Data.Page.Revision
		}
		if revision == "" || response.Data.Page.Revision != revision {
			t.Fatal("page revision missing or changed")
		}
		cursor = response.Data.Page.NextCursor
		if (cursor == "") != (pageNumber == 1) {
			t.Fatal("continuation does not describe remaining records")
		}
		for _, draft := range response.Data.Drafts {
			refs = append(refs, draft.Ref)
		}
	}
	if !slices.Equal(refs, want) {
		t.Fatalf("CLI refs=%v, want=%v", refs, want)
	}
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), service, []string{"drafts", "list", "--limit", "1"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "next_cursor\t") {
		t.Fatalf("human continuation: code=%d, stdout=%s, stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestDraftListCLIRejectsInvalidAndCanceledRequests(t *testing.T) {
	for _, test := range []struct {
		name   string
		args   []string
		cancel bool
		code   int
		error  string
	}{
		{"zero limit", []string{"--limit", "0"}, false, 2, "invalid_argument"},
		{"negative limit", []string{"--limit", "-1"}, false, 2, "invalid_argument"},
		{"excessive limit", []string{"--limit", "201"}, false, 2, "invalid_argument"},
		{"malformed limit", []string{"--limit", "nope"}, false, 2, "invalid_argument"},
		{"malformed cursor", []string{"--cursor", "wrong"}, false, 1, "invalid_cursor"},
		{"canceled", nil, true, 1, "draft_operation_canceled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "absent")
			service := mail.NewServiceWithDraftRoot(nil, root)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.cancel {
				cancel()
			}
			var stdout, stderr bytes.Buffer
			args := append([]string{"drafts", "list", "--json"}, test.args...)
			code := Run(ctx, service, args, &stdout, &stderr)
			var response envelope
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || code != test.code || response.OK || response.Error == nil || response.Error.Code != test.error || stderr.Len() != 0 {
				t.Fatalf("code=%d, decode=%v, stdout=%s, stderr=%s", code, err, stdout.String(), stderr.String())
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("rejected command touched storage: %v", err)
			}
		})
	}
}

func TestDraftListCLIShowsRecordErrorsWithoutLeakingContent(t *testing.T) {
	root := t.TempDir()
	ref := "draft_000000000000000000000000"
	if err := os.WriteFile(filepath.Join(root, ref+".json"), []byte(`{"body":"private\q"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	service := mail.NewServiceWithDraftRoot(nil, root)
	for _, args := range [][]string{{"drafts", "list"}, {"drafts", "list", "--json"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), service, args, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
			t.Fatalf("code=%d, stdout=%s, stderr=%s", code, stdout.String(), stderr.String())
		}
		if !strings.Contains(stdout.String(), ref) || !strings.Contains(stdout.String(), "invalid escape") || strings.Contains(stdout.String(), "private") {
			t.Fatalf("record error absent or content leaked: %s", stdout.String())
		}
	}
}

func TestDraftListSchemaBoundsMatchExecutable(t *testing.T) {
	schema := decodeTestCommandSchema(t, schemaForCommand("drafts.list"))
	flags := schemaFlagsByName(schema)
	limit := flags["--limit"]
	if limit.Default != strconv.Itoa(mail.DefaultDraftListLimit) || limit.Minimum == nil || *limit.Minimum != 1 || limit.Maximum == nil || *limit.Maximum != int64(mail.MaximumDraftListLimit) || flags["--cursor"].ValueType != "cursor" {
		t.Fatalf("incomplete pagination schema: %+v", schema)
	}
	service := mail.NewServiceWithDraftRoot(nil, filepath.Join(t.TempDir(), "drafts"))
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), service, []string{"drafts", "list", "--limit", strconv.FormatInt(*limit.Maximum, 10), "--json"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("published limit rejected: code=%d, stdout=%s, stderr=%s", code, stdout.String(), stderr.String())
	}
}
