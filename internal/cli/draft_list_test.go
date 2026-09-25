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
	maxBytes := flags["--max-bytes"]
	fields := flags["--fields"]
	if limit.Default != strconv.Itoa(mail.DefaultDraftListLimit) || limit.Minimum == nil || *limit.Minimum != 1 || limit.Maximum == nil || *limit.Maximum != int64(mail.MaximumDraftListLimit) || flags["--cursor"].ValueType != "cursor" ||
		maxBytes.Default != strconv.FormatInt(defaultJSONOutputBytes, 10) || maxBytes.Minimum == nil || *maxBytes.Minimum != 1 || maxBytes.Maximum == nil || *maxBytes.Maximum != maximumJSONOutputBytes ||
		fields.ValueType != "field_list" || !slices.Contains(fields.Values, "age_days") || !slices.Contains(fields.Values, "send_attempt") {
		t.Fatalf("incomplete pagination schema: %+v", schema)
	}
	previewFlags := schemaFlagsByName(decodeTestCommandSchema(t, schemaForCommand("drafts.preview")))
	previewMaxBytes := previewFlags["--max-bytes"]
	if previewMaxBytes.Default != strconv.FormatInt(defaultJSONOutputBytes, 10) || previewMaxBytes.Minimum == nil || *previewMaxBytes.Minimum != 1 || previewMaxBytes.Maximum == nil || *previewMaxBytes.Maximum != maximumJSONOutputBytes {
		t.Fatalf("incomplete preview output schema: %+v", previewFlags)
	}
	service := mail.NewServiceWithDraftRoot(nil, filepath.Join(t.TempDir(), "drafts"))
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), service, []string{"drafts", "list", "--limit", strconv.FormatInt(*limit.Maximum, 10), "--json"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("published limit rejected: code=%d, stdout=%s, stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestDraftListCustomFieldsPreserveReviewAndCursorState(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(nil, filepath.Join(t.TempDir(), "drafts"))
	wantRefs := make([]string, 0, 3)
	for index := range 3 {
		draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
			To:      []mail.Recipient{{Address: "recipient" + strconv.Itoa(index) + "@example.com"}},
			Subject: "Reviewed " + strconv.Itoa(index), Body: "complete body",
		}})
		if err != nil {
			t.Fatal(err)
		}
		wantRefs = append(wantRefs, draft.Ref)
	}
	sort.Strings(wantRefs)
	cursor := ""
	var revision string
	var refs []string
	for pageNumber := 0; pageNumber < 2; pageNumber++ {
		var stdout, stderr bytes.Buffer
		args := []string{"drafts", "list", "--limit", "2", "--fields", "age_days", "--cursor", cursor, "--json"}
		if code := Run(context.Background(), service, args, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
			t.Fatalf("code=%d, stdout=%s, stderr=%s", code, stdout.String(), stderr.String())
		}
		var response envelope
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || !response.OK || response.Data.Projection == nil || response.Data.Projection.View != "custom" || response.Data.Drafts == nil {
			t.Fatalf("invalid projected page: decode=%v response=%+v output=%s", err, response, stdout.String())
		}
		if len(*response.Data.Drafts) != 2-pageNumber || response.Data.Page == nil {
			t.Fatalf("projection lost page metadata or refs: %s", stdout.String())
		}
		var pagination mail.DraftPagination
		if err := json.Unmarshal(*response.Data.Page, &pagination); err != nil || pagination.Limit != 2 || pagination.Revision == "" {
			t.Fatalf("invalid projected pagination: %v %+v", err, pagination)
		}
		if pageNumber == 0 {
			revision = pagination.Revision
		}
		if pagination.Revision != revision || (pagination.NextCursor == "") != (pageNumber == 1) {
			t.Fatalf("projection changed listing continuation: %+v", pagination)
		}
		for _, draft := range *response.Data.Drafts {
			refs = append(refs, draft.Ref)
		}
		if !strings.Contains(stdout.String(), `"subject":"Reviewed `) || !strings.Contains(stdout.String(), `"to":[`) ||
			!strings.Contains(stdout.String(), `"ever_sent":false`) || !strings.Contains(stdout.String(), `"age_days":`) ||
			strings.Contains(stdout.String(), `"created_at"`) || strings.Contains(stdout.String(), `"updated_at"`) {
			t.Fatalf("custom fields hid review state or retained omitted timestamps: %s", stdout.String())
		}
		cursor = pagination.NextCursor
	}
	if !slices.Equal(refs, wantRefs) {
		t.Fatalf("projected refs=%v, want=%v", refs, wantRefs)
	}
}

func TestDraftListBudgetPreservesDefaultPayloadAndExactBoundary(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(nil, filepath.Join(t.TempDir(), "drafts"))
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		To: []mail.Recipient{{Address: "recipient@example.com"}}, Subject: "Stable payload", Body: "body",
	}})
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.ListDrafts(context.Background(), mail.ListDraftsRequest{Limit: mail.DefaultDraftListLimit})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), service, []string{"drafts", "list", "--json"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("default listing failed: code=%d, stdout=%s, stderr=%s", code, stdout.String(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || !response.OK || response.Data.Projection != nil || response.Data.Drafts == nil || len(*response.Data.Drafts) != 1 {
		t.Fatalf("default payload shape changed: decode=%v output=%s", err, stdout.String())
	}
	if (*response.Data.Drafts)[0].Ref != draft.Ref || (*response.Data.Drafts)[0].Subject != draft.Subject {
		t.Fatalf("default summary changed: %+v", (*response.Data.Drafts)[0])
	}
	entries := []draftListEntry{{DraftSummary: page.Drafts[0], AgeDays: (*response.Data.Drafts)[0].AgeDays}}
	want := &bytes.Buffer{}
	if code := writeSuccess(want, "drafts.list", responseData{Drafts: &entries, Page: rawResponsePage(page.Pagination)}); code != 0 || !bytes.Equal(stdout.Bytes(), want.Bytes()) {
		t.Fatalf("default JSON payload changed:\ngot  %s\nwant %s", stdout.String(), want.String())
	}

	largeArgs := []string{"drafts", "list", "--json", "--max-bytes", strconv.FormatInt(maximumJSONOutputBytes, 10)}
	stdout.Reset()
	if code := Run(context.Background(), service, largeArgs, &stdout, &stderr); code != 0 {
		t.Fatalf("large budget listing failed: code=%d, stdout=%s", code, stdout.String())
	}
	boundaryArgs := []string{"drafts", "list", "--json", "--max-bytes", strconv.Itoa(stdout.Len())}
	boundary := append([]byte(nil), stdout.Bytes()...)
	stdout.Reset()
	if code := Run(context.Background(), service, boundaryArgs, &stdout, &stderr); code != 0 || !bytes.Equal(stdout.Bytes(), boundary) {
		t.Fatalf("exact output boundary rejected: code=%d, output=%s", code, stdout.String())
	}
	boundaryArgs[len(boundaryArgs)-1] = strconv.Itoa(len(boundary) - 1)
	stdout.Reset()
	if code := Run(context.Background(), service, boundaryArgs, &stdout, &stderr); code != 1 || json.Unmarshal(stdout.Bytes(), &response) != nil || response.OK || response.Error == nil || response.Error.Code != "output_too_large" || !strings.Contains(response.Error.Message, draft.Ref) || !strings.Contains(response.Error.Message, "--export /absolute/new/path") {
		t.Fatalf("one-byte-over response lacks recovery: code=%d, stdout=%s", code, stdout.String())
	}
}

func TestDraftListTwoHundredLongSummariesReturnRecoverableJSON(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(nil, filepath.Join(t.TempDir(), "drafts"))
	for index := range mail.MaximumDraftListLimit {
		_, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
			To:      []mail.Recipient{{Address: "recipient" + strconv.Itoa(index) + "@example.com"}},
			Subject: strings.Repeat("s", 6000), Body: "",
		}})
		if err != nil {
			t.Fatalf("create long summary %d: %v", index, err)
		}
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), service, []string{"drafts", "list", "--limit", "200", "--json"}, &stdout, &stderr)
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || code != 1 || response.OK || response.Error == nil || response.Error.Code != "output_too_large" ||
		response.Data.Drafts != nil || response.Data.Page == nil || stdout.Len() >= int(defaultJSONOutputBytes) || stderr.Len() != 0 {
		t.Fatalf("oversized list was not reported with retained refs/page: code=%d decode=%v output=%s stderr=%s", code, err, stdout.String(), stderr.String())
	}
	var pagination mail.DraftPagination
	if err := json.Unmarshal(*response.Data.Page, &pagination); err != nil || pagination.Limit != mail.MaximumDraftListLimit || pagination.Revision == "" {
		t.Fatalf("oversized list lost pagination evidence: %v %+v", err, pagination)
	}
	firstPage, err := service.ListDrafts(context.Background(), mail.ListDraftsRequest{Limit: 1})
	if err != nil || len(firstPage.Drafts) != 1 || !strings.Contains(response.Error.Message, "drafts inspect --ref "+firstPage.Drafts[0].Ref) || !strings.Contains(response.Error.Message, "--export /absolute/new/path") {
		t.Fatalf("oversized list has no concrete inspect/export route: %s", response.Error.Message)
	}
}

func TestDraftListOversizedEmptyPageHasRetryRoute(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(nil, filepath.Join(t.TempDir(), "drafts"))
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), service, []string{"drafts", "list", "--max-bytes", "1", "--json"}, &stdout, &stderr)
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || code != 1 || response.OK || response.Error == nil ||
		response.Error.Code != "output_too_large" || !strings.Contains(response.Error.Message, "retry with 'mailcli drafts list --limit 50 --max-bytes 67108864 --json'") ||
		response.Data.Page == nil || stderr.Len() != 0 {
		t.Fatalf("oversized empty list had no concrete retry route: code=%d decode=%v output=%s stderr=%s", code, err, stdout.String(), stderr.String())
	}
}
