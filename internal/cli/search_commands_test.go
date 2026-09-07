package cli

import (
	"bytes"
	"context"
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
