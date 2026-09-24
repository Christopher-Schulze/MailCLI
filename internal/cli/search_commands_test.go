package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

type partialSearchGateway struct {
	testGateway
}

type searchQueryCaptureGateway struct {
	testGateway
	query mail.PreparedQuery
}

type searchBudgetGateway struct {
	testGateway
	requiredBytes int64
}

func (g searchBudgetGateway) SearchMessages(context.Context, mail.PreparedQuery) (mail.SearchPage, error) {
	return mail.SearchPage{}, &searchBudgetCLIError{requiredBytes: g.requiredBytes}
}

type searchBudgetCLIError struct {
	requiredBytes int64
}

func (e *searchBudgetCLIError) Error() string {
	return "search candidate requires more bytes; restart the same search without --cursor"
}

func (e *searchBudgetCLIError) ErrorCode() string {
	return "search_budget_too_small"
}

func (e *searchBudgetCLIError) RequiredBytes() int64 {
	return e.requiredBytes
}

func (g *searchQueryCaptureGateway) SearchMessages(_ context.Context, query mail.PreparedQuery) (mail.SearchPage, error) {
	g.query = query
	return mail.SearchPage{Coverage: mail.SearchCoverage{CandidateMessagesExact: true, Complete: true}}, nil
}

func (partialSearchGateway) SearchMessages(context.Context, mail.PreparedQuery) (mail.SearchPage, error) {
	return mail.SearchPage{
		Coverage: mail.SearchCoverage{
			Consistency: mail.SearchConsistencyBestEffort, IndexRevision: "revision-1",
			Backend: "emlx_stream", CandidateMessages: 20, ScannedMessages: 10, Complete: false,
		},
	}, nil
}

func TestSearchCommandsJSON(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(
		context.Background(), newTestService(),
		[]string{"messages", "search", "--query", "Needle", "--read=false", "--json"},
		&stdout, &stderr,
	)
	if code != 0 || !strings.Contains(stdout.String(), `"command":"messages.search"`) || !strings.Contains(stdout.String(), `"subject":"Searchable"`) {
		t.Fatalf("search code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run(
		context.Background(), newTestService(),
		[]string{"messages", "filter", "--mailbox", "mbx_ref", "--read=false", "--json"},
		&stdout, &stderr,
	)
	if code != 0 || !strings.Contains(stdout.String(), `"command":"messages.filter"`) || !strings.Contains(stdout.String(), `"subject":"Searchable"`) {
		t.Fatalf("filter code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

func TestSearchBudgetTooSmallJSONIncludesRequiredBytesAndRecovery(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(
		context.Background(), mail.NewService(searchBudgetGateway{requiredBytes: 4096}),
		[]string{"messages", "search", "--query", "needle", "--json"},
		&stdout, &stderr,
	)
	var response struct {
		OK    bool `json:"ok"`
		Error *struct {
			Code          string `json:"code"`
			Message       string `json:"message"`
			RequiredBytes int64  `json:"required_bytes"`
			Guidance      struct {
				Phase           string `json:"phase"`
				EffectCertainty string `json:"effect_certainty"`
				Retryability    string `json:"retryability"`
				ReplayAllowed   bool   `json:"replay_allowed"`
				Recovery        struct {
					Action  string   `json:"action"`
					Command string   `json:"command"`
					Args    []string `json:"args"`
				} `json:"recovery"`
			} `json:"guidance"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode search error: %v; stdout = %q", err, stdout.String())
	}
	if code != 1 || stderr.Len() != 0 || response.OK || response.Error == nil ||
		response.Error.Code != "search_budget_too_small" || response.Error.RequiredBytes != 4096 ||
		!strings.Contains(response.Error.Message, "without --cursor") ||
		response.Error.Guidance.Phase != "read" || response.Error.Guidance.EffectCertainty != "none" ||
		response.Error.Guidance.Retryability != "user_input_required" || response.Error.Guidance.ReplayAllowed ||
		response.Error.Guidance.Recovery.Action != "correct" || response.Error.Guidance.Recovery.Command != "messages.search" ||
		len(response.Error.Guidance.Recovery.Args) != 2 ||
		response.Error.Guidance.Recovery.Args[0] != "--max-bytes" ||
		response.Error.Guidance.Recovery.Args[1] != "4096" {
		t.Fatalf("search error response: code=%d response=%+v stderr=%q", code, response, stderr.String())
	}
}

func TestSearchRejectsInvalidOptionalBoolean(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(
		context.Background(), newTestService(),
		[]string{"messages", "search", "--read=maybe"}, &stdout, &stderr,
	)
	if code != 2 || !strings.Contains(stderr.String(), "read must be true or false") {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
}

func TestSearchExactCountFlag(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "body search", args: []string{"messages", "search", "--query", "needle", "--exact-count", "--json"}},
		{name: "attachment filter", args: []string{"messages", "filter", "--attachment", "true", "--exact-count", "--json"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gateway := &searchQueryCaptureGateway{}
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := Run(context.Background(), mail.NewService(gateway), test.args, &stdout, &stderr)
			if code != 0 || stderr.Len() != 0 || !gateway.query.Query.ExactCount {
				t.Fatalf("code = %d, exact_count = %t, stdout = %q, stderr = %q",
					code, gateway.query.Query.ExactCount, stdout.String(), stderr.String())
			}
		})
	}
}

func TestSearchCommandsPreserveEpochDateBounds(t *testing.T) {
	for _, command := range []string{"filter", "search"} {
		for _, test := range []struct {
			name, after, before   string
			afterUnix, beforeUnix int64
		}{
			{name: "omitted"},
			{name: "epoch before", before: "1970-01-01T00:00:00Z"},
			{name: "epoch after", after: "1970-01-01T00:00:00Z"},
			{name: "negative to epoch", after: "1969-12-31T23:59:59Z", before: "1970-01-01T01:00:00+01:00", afterUnix: -1},
		} {
			t.Run(command+"/"+test.name, func(t *testing.T) {
				gateway := &searchQueryCaptureGateway{}
				args := []string{"messages", command, "--json"}
				if test.after != "" {
					args = append(args, "--after", test.after)
				}
				if test.before != "" {
					args = append(args, "--before", test.before)
				}
				var stdout, stderr bytes.Buffer
				code := Run(context.Background(), mail.NewService(gateway), args, &stdout, &stderr)
				query := gateway.query
				if code != 0 || stderr.Len() != 0 || query.Fingerprint == "" || query.Query.After != test.after || query.Query.Before != test.before || (query.AfterUnix != nil) != (test.after != "") || (query.BeforeUnix != nil) != (test.before != "") {
					t.Fatalf("CLI date query: code=%d, query=%+v, stdout=%s, stderr=%s", code, query, &stdout, &stderr)
				}
				if query.AfterUnix != nil && *query.AfterUnix != test.afterUnix || query.BeforeUnix != nil && *query.BeforeUnix != test.beforeUnix {
					t.Fatalf("CLI changed date values: %+v", query)
				}
			})
		}
	}
}

func TestSearchCommandsRejectInvalidEpochRangeBeforeDispatch(t *testing.T) {
	for _, command := range []string{"filter", "search"} {
		gateway := &searchQueryCaptureGateway{}
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), mail.NewService(gateway), []string{"messages", command, "--after", "1970-01-01T00:00:00Z", "--before", "1969-12-31T23:59:59Z", "--json"}, &stdout, &stderr)
		if code != 2 || !strings.Contains(stdout.String(), "after must be earlier than before") || gateway.query.Fingerprint != "" {
			t.Fatalf("invalid epoch range: code=%d, query=%+v, stdout=%s, stderr=%s", code, gateway.query, &stdout, &stderr)
		}
	}
}

func TestSearchHumanOutputIncludesSnippetAndHonestCoverage(t *testing.T) {
	page := mail.SearchPage{
		Messages: []mail.SearchMessage{{
			Summary: mail.MessageSummary{
				Ref: "msg_ref", DateReceived: "2026-08-23T10:00:00Z",
				Sender: "sender@example.com", Subject: "Subject",
			},
			Snippet: "matching\ncontext",
		}},
		Coverage: mail.SearchCoverage{
			Consistency: mail.SearchConsistencyBestEffort, IndexRevision: "revision-1",
			Backend: "emlx_stream", CandidateMessages: 20, ScannedMessages: 10, Complete: false,
		},
	}
	var output bytes.Buffer
	writeSearchResults(&output, page)
	if !strings.Contains(output.String(), "matching context") ||
		!strings.Contains(output.String(), "consistency=best_effort") ||
		!strings.Contains(output.String(), "revision=revision-1") ||
		!strings.Contains(output.String(), "corpus_complete=false") ||
		!strings.Contains(output.String(), "candidates_exact=false") ||
		strings.Contains(output.String(), "matching\ncontext") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestSearchJSONReportsIncompleteCorpus(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(
		context.Background(), mail.NewService(partialSearchGateway{}),
		[]string{"messages", "search", "--query", "needle", "--json"},
		&stdout, &stderr,
	)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), `"complete":false`) ||
		!strings.Contains(stdout.String(), `"candidate_messages_exact":false`) ||
		!strings.Contains(stdout.String(), `"consistency":"best_effort"`) ||
		!strings.Contains(stdout.String(), `"index_revision":"revision-1"`) {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

type pageProjectionGateway struct {
	testGateway
	listPage      mail.MessagePage
	listPages     []mail.MessagePage
	searchPage    mail.SearchPage
	searchPages   []mail.SearchPage
	listCalls     int
	searchCalls   int
	listRequests  []mail.ListMessagesRequest
	searchQueries []mail.PreparedQuery
}

func (g *pageProjectionGateway) ListMessages(_ context.Context, request mail.ListMessagesRequest) (mail.MessagePage, error) {
	g.listCalls++
	g.listRequests = append(g.listRequests, request)
	if len(g.listPages) > 0 && g.listCalls <= len(g.listPages) {
		return g.listPages[g.listCalls-1], nil
	}
	return g.listPage, nil
}

func (g *pageProjectionGateway) SearchMessages(_ context.Context, query mail.PreparedQuery) (mail.SearchPage, error) {
	g.searchCalls++
	g.searchQueries = append(g.searchQueries, query)
	if len(g.searchPages) > 0 && g.searchCalls <= len(g.searchPages) {
		return g.searchPages[g.searchCalls-1], nil
	}
	return g.searchPage, nil
}

func newPageProjectionGateway() *pageProjectionGateway {
	listPage, searchPage := pageProjectionFixture()
	return &pageProjectionGateway{listPage: listPage, searchPage: searchPage}
}

func pageProjectionFixture() (mail.MessagePage, mail.SearchPage) {
	listPage := mail.MessagePage{Messages: make([]mail.MessageSummary, 25), NextCursor: "next-page-cursor"}
	searchPage := mail.SearchPage{
		Messages: make([]mail.SearchMessage, 25), NextCursor: "next-page-cursor",
		Coverage: mail.SearchCoverage{
			Consistency: mail.SearchConsistencyBestEffort, IndexRevision: "revision-1", Backend: "emlx_stream",
			CandidateMessages: 50, CandidateMessagesExact: true, ScannedMessages: 25, ScannedBytes: 8192,
			FullSources: 25, Complete: true,
		},
	}
	for index := range listPage.Messages {
		message := mail.MessageSummary{
			Ref: fmt.Sprintf("msg_ref_%02d", index), MailboxRef: "mbx_ref", MessageID: fmt.Sprintf("<message-%02d@example.com>", index),
			Subject: strings.Repeat("Quarterly project status ", 5), Sender: "sender@example.com",
			DateReceived: "2026-09-24T10:00:00Z", DateSent: "2026-09-24T09:59:00Z",
			Read: index%2 == 0, Flagged: index%3 == 0, Junk: index%7 == 0, Deleted: index%11 == 0,
			Size: 4096 + int64(index), AttachmentCount: index % 3, ConversationID: int64(index + 1),
			StalenessNote: strings.Repeat("summary metadata retained for later inspection; ", 3),
		}
		listPage.Messages[index] = message
		searchPage.Messages[index] = mail.SearchMessage{Summary: message, Snippet: strings.Repeat("matching phrase in message context; ", 3)}
	}
	return listPage, searchPage
}

func pageProjectionArgs(command, fields string) []string {
	args := []string{"messages", command}
	switch command {
	case "list":
		args = append(args, "--mailbox", "mbx_ref")
	case "filter":
		args = append(args, "--mailbox", "mbx_ref")
	case "search":
		args = append(args, "--query", "needle")
	}
	if fields != "" {
		args = append(args, "--fields", fields)
	}
	return append(args, "--json")
}

func runPageProjectionCommand(gateway *pageProjectionGateway, args []string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), mail.NewService(gateway), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func pageFromEnvelope(t *testing.T, output string) json.RawMessage {
	t.Helper()
	var response struct {
		Data struct {
			Page json.RawMessage `json:"page"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		t.Fatalf("decode page response: %v; output=%s", err, output)
	}
	if len(response.Data.Page) == 0 {
		t.Fatalf("missing data.page: %s", output)
	}
	return response.Data.Page
}

func TestPageFieldsReduceActualTwentyFiveResultCLIOutput(t *testing.T) {
	for _, test := range []struct {
		command string
		fields  string
	}{
		{command: "list", fields: "sender,subject,date_received,read,flagged,attachment_count"},
		{command: "filter", fields: "sender,subject,date_received,snippet,read,flagged,attachment_count"},
		{command: "search", fields: "sender,subject,date_received,snippet,read,flagged,attachment_count"},
	} {
		t.Run(test.command, func(t *testing.T) { assertPageProjectionSavings(t, test.command, test.fields) })
	}
}

func assertPageProjectionSavings(t *testing.T, command, fields string) {
	t.Helper()
	fullGateway := newPageProjectionGateway()
	fullCode, fullOutput, fullStderr := runPageProjectionCommand(fullGateway, pageProjectionArgs(command, ""))
	projectedGateway := newPageProjectionGateway()
	projectedCode, projectedOutput, projectedStderr := runPageProjectionCommand(projectedGateway, pageProjectionArgs(command, fields))
	if fullCode != 0 || fullStderr != "" || projectedCode != 0 || projectedStderr != "" {
		t.Fatalf("full code=%d stderr=%q; projected code=%d stderr=%q", fullCode, fullStderr, projectedCode, projectedStderr)
	}
	fullCalls := fullGateway.listCalls + fullGateway.searchCalls
	projectedCalls := projectedGateway.listCalls + projectedGateway.searchCalls
	if fullCalls != 1 || projectedCalls != 1 {
		t.Fatalf("CLI read calls: full=%d projected=%d", fullCalls, projectedCalls)
	}
	fullPage, projectedPage := pageFromEnvelope(t, fullOutput), pageFromEnvelope(t, projectedOutput)
	var projected struct {
		Messages   []json.RawMessage    `json:"messages"`
		NextCursor string               `json:"next_cursor"`
		Coverage   *mail.SearchCoverage `json:"coverage"`
	}
	if err := json.Unmarshal(projectedPage, &projected); err != nil {
		t.Fatalf("decode projected page: %v", err)
	}
	if len(projected.Messages) != 25 || projected.NextCursor != "next-page-cursor" {
		t.Fatalf("projected page rows/cursor: rows=%d cursor=%q", len(projected.Messages), projected.NextCursor)
	}
	if command != "list" && (projected.Coverage == nil || !projected.Coverage.Complete || projected.Coverage.CandidateMessages != 50) {
		t.Fatalf("projected coverage = %+v", projected.Coverage)
	}
	if !bytes.Contains(projected.Messages[0], []byte(`"ref":"msg_ref_00"`)) ||
		!bytes.Contains(projected.Messages[0], []byte(`"mailbox_ref":"mbx_ref"`)) {
		t.Fatalf("projected row dropped mandatory refs: %s", projected.Messages[0])
	}
	pageReduction := 100 * float64(len(fullPage)-len(projectedPage)) / float64(len(fullPage))
	outputReduction := 100 * float64(len(fullOutput)-len(projectedOutput)) / float64(len(fullOutput))
	t.Logf("serialized CLI bytes: full=%d projected=%d saved=%.1f%%; page bytes saved=%.1f%%", len(fullOutput), len(projectedOutput), outputReduction, pageReduction)
	if outputReduction < 20 {
		t.Fatalf("projected CLI output saved %.1f%%, want at least 20%%", outputReduction)
	}
}

func TestPageFieldsAllPreservesTheDefaultPageAndReportsProjection(t *testing.T) {
	for _, command := range []string{"list", "filter", "search"} {
		t.Run(command, func(t *testing.T) {
			fullCode, fullOutput, fullStderr := runPageProjectionCommand(newPageProjectionGateway(), pageProjectionArgs(command, ""))
			allGateway := newPageProjectionGateway()
			allCode, allOutput, allStderr := runPageProjectionCommand(allGateway, pageProjectionArgs(command, "all"))
			var response struct {
				Data struct {
					Projection *projectionInfo `json:"projection"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(allOutput), &response); err != nil {
				t.Fatalf("decode all-fields response: %v", err)
			}
			if fullCode != 0 || fullStderr != "" || allCode != 0 || allStderr != "" ||
				!bytes.Equal(pageFromEnvelope(t, fullOutput), pageFromEnvelope(t, allOutput)) ||
				response.Data.Projection == nil || response.Data.Projection.View != "custom" ||
				len(response.Data.Projection.Fields) != 1 || response.Data.Projection.Fields[0] != "all" ||
				allGateway.listCalls+allGateway.searchCalls != 1 {
				t.Fatalf("all-fields page mismatch: code=%d/%d projection=%+v", fullCode, allCode, response.Data.Projection)
			}
		})
	}
}

func TestPageFieldValidationPrecedesReads(t *testing.T) {
	for _, command := range []string{"list", "filter", "search"} {
		for _, fields := range []string{"unknown", "sender,sender", "all,sender"} {
			t.Run(command+"/"+fields, func(t *testing.T) {
				gateway := newPageProjectionGateway()
				code, output, stderr := runPageProjectionCommand(gateway, pageProjectionArgs(command, fields))
				if code != 2 || stderr != "" || gateway.listCalls+gateway.searchCalls != 0 ||
					!strings.Contains(output, `"code":"invalid_argument"`) {
					t.Fatalf("validation: code=%d calls=%d stderr=%q output=%s", code, gateway.listCalls+gateway.searchCalls, stderr, output)
				}
			})
		}
	}
}

func TestProjectedPagesKeepEmptyAndPartialMessageFields(t *testing.T) {
	for _, messages := range [][]mail.MessageSummary{nil, {}} {
		gateway := newPageProjectionGateway()
		gateway.listPage = mail.MessagePage{Messages: messages}
		code, output, stderr := runPageProjectionCommand(gateway, pageProjectionArgs("list", "sender,read,size,date_received"))
		var page struct {
			Messages json.RawMessage `json:"messages"`
		}
		if code != 0 || stderr != "" || json.Unmarshal(pageFromEnvelope(t, output), &page) != nil {
			t.Fatalf("empty page: code=%d stderr=%q output=%s", code, stderr, output)
		}
		want := "[]"
		if messages == nil {
			want = "null"
		}
		if string(page.Messages) != want {
			t.Fatalf("empty messages = %s, want %s", page.Messages, want)
		}
	}

	gateway := newPageProjectionGateway()
	gateway.listPage = mail.MessagePage{Messages: []mail.MessageSummary{{Ref: "msg_ref", MailboxRef: "mbx_ref"}}}
	code, output, stderr := runPageProjectionCommand(gateway, pageProjectionArgs("list", "sender,read,size,date_received"))
	page := pageFromEnvelope(t, output)
	if code != 0 || stderr != "" || !bytes.Contains(page, []byte(`"sender":""`)) ||
		!bytes.Contains(page, []byte(`"date_received":""`)) || !bytes.Contains(page, []byte(`"read":false`)) ||
		!bytes.Contains(page, []byte(`"size":0`)) || bytes.Contains(page, []byte(`"subject"`)) {
		t.Fatalf("partial projected page: code=%d stderr=%q page=%s", code, stderr, page)
	}
}

func TestProjectedPagePaginationPassesTheReturnedCursor(t *testing.T) {
	gateway := newPageProjectionGateway()
	gateway.listPages = []mail.MessagePage{
		{Messages: []mail.MessageSummary{{Ref: "first", MailboxRef: "mbx_ref", Sender: "a@example.com"}}, NextCursor: "cursor-page-two"},
		{Messages: []mail.MessageSummary{{Ref: "second", MailboxRef: "mbx_ref", Sender: "b@example.com"}}},
	}
	firstCode, firstOutput, firstStderr := runPageProjectionCommand(gateway, pageProjectionArgs("list", "sender"))
	var first struct {
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(pageFromEnvelope(t, firstOutput), &first); err != nil {
		t.Fatalf("decode first page: %v", err)
	}
	secondArgs := append(pageProjectionArgs("list", "sender"), "--cursor", first.NextCursor)
	secondCode, secondOutput, secondStderr := runPageProjectionCommand(gateway, secondArgs)
	secondPage := pageFromEnvelope(t, secondOutput)
	if firstCode != 0 || firstStderr != "" || first.NextCursor != "cursor-page-two" ||
		secondCode != 0 || secondStderr != "" || gateway.listCalls != 2 ||
		len(gateway.listRequests) != 2 || gateway.listRequests[1].Cursor != first.NextCursor ||
		!bytes.Contains(secondPage, []byte(`"ref":"second"`)) ||
		!bytes.Contains(secondPage, []byte(`"next_cursor":""`)) {
		t.Fatalf("pagination: first=%s second=%s calls=%d requests=%+v", firstOutput, secondOutput, gateway.listCalls, gateway.listRequests)
	}
}

func TestProjectedSearchPaginationPassesTheReturnedCursor(t *testing.T) {
	prepared, err := mail.PrepareQuery(mail.Query{
		Text: "needle", Limit: mail.DefaultPageLimit,
		MaxMessages: mail.DefaultSearchMaxMessages, MaxBytes: mail.DefaultSearchMaxBytes,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	cursor, err := mail.EncodeSearchCursorWithRevision(prepared.Fingerprint, "store-uuid", "revision-1", 1, false, 1)
	if err != nil {
		t.Fatalf("EncodeSearchCursorWithRevision() error = %v", err)
	}
	gateway := newPageProjectionGateway()
	gateway.searchPages = []mail.SearchPage{
		{Messages: []mail.SearchMessage{{Summary: mail.MessageSummary{Ref: "first", MailboxRef: "mbx_ref", Sender: "a@example.com"}}}, NextCursor: cursor, Coverage: gateway.searchPage.Coverage},
		{Messages: []mail.SearchMessage{{Summary: mail.MessageSummary{Ref: "second", MailboxRef: "mbx_ref", Sender: "b@example.com"}}}, Coverage: gateway.searchPage.Coverage},
	}
	firstCode, firstOutput, firstStderr := runPageProjectionCommand(gateway, pageProjectionArgs("search", "sender"))
	var first struct {
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(pageFromEnvelope(t, firstOutput), &first); err != nil {
		t.Fatalf("decode first search page: %v", err)
	}
	secondArgs := append(pageProjectionArgs("search", "sender"), "--cursor", first.NextCursor)
	secondCode, secondOutput, secondStderr := runPageProjectionCommand(gateway, secondArgs)
	secondPage := pageFromEnvelope(t, secondOutput)
	if firstCode != 0 || firstStderr != "" || first.NextCursor != cursor || secondCode != 0 || secondStderr != "" ||
		gateway.searchCalls != 2 || len(gateway.searchQueries) != 2 || gateway.searchQueries[1].Cursor == nil ||
		gateway.searchQueries[1].Cursor.RowID != 1 || !bytes.Contains(secondPage, []byte(`"ref":"second"`)) ||
		!bytes.Contains(secondPage, []byte(`"next_cursor":""`)) {
		t.Fatalf("search pagination: first=%s second=%s queries=%+v", firstOutput, secondOutput, gateway.searchQueries)
	}
}

func TestProjectedSearchRetainsIncompleteCoverageAndTypedFailures(t *testing.T) {
	for _, command := range []string{"filter", "search"} {
		t.Run(command, func(t *testing.T) {
			partial := newPageProjectionGateway()
			partial.searchPage.Coverage.Complete = false
			partial.searchPage.NextCursor = "continue-search"
			code, output, stderr := runPageProjectionCommand(partial, pageProjectionArgs(command, "sender,snippet"))
			var projected mail.SearchPage
			if err := json.Unmarshal(pageFromEnvelope(t, output), &projected); err != nil {
				t.Fatalf("decode incomplete projected page: %v", err)
			}
			if code != 0 || stderr != "" || partial.searchCalls != 1 ||
				projected.Coverage != partial.searchPage.Coverage || projected.NextCursor != "continue-search" {
				t.Fatalf("partial projected page: code=%d calls=%d stderr=%q page=%+v", code, partial.searchCalls, stderr, projected)
			}
		})
	}
}

func TestProjectedSearchRetainsTypedFailures(t *testing.T) {
	var failureOutput bytes.Buffer
	var failureStderr bytes.Buffer
	failureCode := Run(context.Background(), mail.NewService(searchBudgetGateway{requiredBytes: 4096}),
		[]string{"messages", "search", "--query", "needle", "--fields", "sender", "--json"},
		&failureOutput, &failureStderr)
	if failureCode != 1 || failureStderr.Len() != 0 ||
		!strings.Contains(failureOutput.String(), `"code":"search_budget_too_small"`) ||
		!strings.Contains(failureOutput.String(), `"required_bytes":4096`) {
		t.Fatalf("typed projected error: code=%d stderr=%q output=%s", failureCode, failureStderr.String(), failureOutput.String())
	}
}
