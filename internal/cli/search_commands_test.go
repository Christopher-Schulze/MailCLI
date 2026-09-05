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

func (partialSearchGateway) SearchMessages(context.Context, mail.PreparedQuery) (mail.SearchPage, error) {
	return mail.SearchPage{
		Coverage: mail.SearchCoverage{
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
			Backend: "emlx_stream", CandidateMessages: 20, ScannedMessages: 10, Complete: false,
		},
	}
	var output bytes.Buffer
	writeSearchResults(&output, page)
	if !strings.Contains(output.String(), "matching context") ||
		!strings.Contains(output.String(), "corpus_complete=false") ||
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
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), `"complete":false`) {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}
