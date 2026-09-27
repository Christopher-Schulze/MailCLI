package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailstore"
)

func goldenWorkflowStore(t *testing.T) *mailstore.Client {
	t.Helper()
	const account = "951FB17E-B847-452E-9D98-DFF9EEDCC009"
	const store = "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA"
	config := createNotFoundRecoveryStore(t, store, account)
	database, err := sql.Open("sqlite3", filepath.Join(config.MailStorePath, "MailData", "Envelope Index"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, query := range []string{
		`INSERT INTO mailboxes VALUES (1,'imap://` + account + `/INBOX',6,6,0,1),(2,'imap://` + account + `/Sent',1,0,0,1)`,
		`INSERT INTO addresses VALUES (1,'author@example.com','Author'),(2,'sender@icloud.com','Sender')`,
		`INSERT INTO subjects VALUES (100,'Sender identity')`,
		`INSERT INTO summaries VALUES (100,'Sender identity')`,
		`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (100,100,100,2,100,100,1,1,2,0,1,0,0,100,100,0,1,0)`,
	} {
		if _, err := database.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	accountRoot := filepath.Join(config.MailStorePath, account)
	if err := os.MkdirAll(accountRoot, 0700); err != nil {
		t.Fatal(err)
	}
	cache := `<?xml version="1.0"?><plist version="1.0"><dict><key>mboxes</key><dict><key>Sent</key><dict><key>MailboxPathComponent</key><string>Sent</string><key>IMAPMailboxAttributes</key><integer>32768</integer><key>IMAPMailboxChildren</key><dict/></dict></dict></dict></plist>`
	if err := os.WriteFile(filepath.Join(accountRoot, ".mboxCache.plist"), []byte(cache), 0600); err != nil {
		t.Fatal(err)
	}
	sentSource := "From: sender@icloud.com\r\nTo: author@example.com\r\nSubject: Sender identity\r\nMessage-ID: <identity@icloud.com>\r\nContent-Type: text/plain\r\n\r\nPrevious sent message\r\n"
	sentDirectory := filepath.Join(accountRoot, "Sent.mbox", store, "Data", "Messages")
	if err := os.MkdirAll(sentDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	sentFrame := fmt.Sprintf("%-10d\n%s", len(sentSource), sentSource) + `<?xml version="1.0"?><plist version="1.0"><dict/></plist>`
	if err := os.WriteFile(filepath.Join(sentDirectory, "100.emlx"), []byte(sentFrame), 0600); err != nil {
		t.Fatal(err)
	}
	for id := 1; id <= 6; id++ {
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{`INSERT INTO subjects VALUES (?,?)`, []any{id, fmt.Sprintf("Workflow %d", id)}},
			{`INSERT INTO summaries VALUES (?,?)`, []any{id, "Metadata summary is not the body"}},
			{`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (?,?,?,?,?,?,?, ?,1,0,0,0,0,100,42,0,?,0)`, []any{id, id, id, 1, id, id, id + 100, id + 100, id + 100}},
			{`INSERT INTO recipients(message,address,type,position) VALUES (?,2,0,0)`, []any{id}},
		} {
			if _, err := database.Exec(statement.query, statement.args...); err != nil {
				t.Fatal(err)
			}
		}
		source := fmt.Sprintf("From: Author <author@example.com>\r\nTo: sender@icloud.com\r\nSubject: Workflow %d\r\nMessage-ID: <workflow-%d@example.com>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nActual workflow body %d\r\n", id, id, id)
		path := filepath.Join(accountRoot, "INBOX.mbox", store, "Data", "Messages", fmt.Sprintf("%d.emlx", id))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		frame := fmt.Sprintf("%-10d\n%s", len(source), source) + `<?xml version="1.0"?><plist version="1.0"><dict/></plist>`
		if err := os.WriteFile(path, []byte(frame), 0600); err != nil {
			t.Fatal(err)
		}
	}
	client := mailstore.NewClient(context.Background(), nil, config, mail.SendTransport{})
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	return client
}

func invokeGoldenWorkflow(t *testing.T, service *mail.Service, wantCode int, args ...string) (envelope, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	normalized, jsonOutput, err := ResolveOutputMode(args, &stdout, "")
	if err != nil || !jsonOutput {
		t.Fatalf("default piped output: %t %v", jsonOutput, err)
	}
	code := Run(context.Background(), service, normalized, &stdout, &stderr)
	var result envelope
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || code != wantCode || stderr.Len() != 0 || result.OK != (wantCode == 0) {
		t.Fatalf("%v: code=%d decode=%v stdout=%s stderr=%s", args, code, err, &stdout, &stderr)
	}
	return result, stdout.Len()
}

func goldenMessagePage(t *testing.T, result envelope) mail.MessagePage {
	t.Helper()
	var page mail.MessagePage
	if result.Data.Page == nil || json.Unmarshal(*result.Data.Page, &page) != nil {
		t.Fatalf("missing message page: %+v", result)
	}
	if result.Command == "messages.search" || result.Command == "messages.filter" {
		var search mail.SearchPage
		if err := json.Unmarshal(*result.Data.Page, &search); err != nil || search.Coverage.PartialSources != 0 || search.Coverage.MissingSources != 0 || (search.NextCursor == "" && !search.Coverage.Complete) {
			t.Fatalf("incomplete source or terminal search/filter coverage: %v %+v", err, search.Coverage)
		}
		page.Messages = nil
		for _, message := range search.Messages {
			page.Messages = append(page.Messages, message.Summary)
		}
	}
	for _, message := range page.Messages {
		if message.Ref == "" {
			t.Fatal("page member has no usable reference")
		}
	}
	return page
}

func TestGoldenReadWorkflowsUseRealStoreAndBoundedContinuation(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(goldenWorkflowStore(t), t.TempDir())
	listed, listBytes := invokeGoldenWorkflow(t, service, 0, "messages", "list")
	page := goldenMessagePage(t, listed)
	if len(page.Messages) != 6 {
		t.Fatalf("unified inbox messages = %d", len(page.Messages))
	}
	read, readBytes := invokeGoldenWorkflow(t, service, 0, "messages", "get", page.Messages[0].Ref, "--view", "plain")
	if read.Data.Message == nil || !strings.Contains(read.Data.Message.Content, "Actual workflow body 6") || !read.Data.Message.ContentComplete {
		t.Fatalf("triage did not read actual complete content: %+v", read.Data.Message)
	}
	if listBytes+readBytes > 8*1024 {
		t.Fatalf("representative triage = %d bytes, limit 8192", listBytes+readBytes)
	}
	t.Logf("triage first page plus body: %d bytes", listBytes+readBytes)
	thread, threadBytes := invokeGoldenWorkflow(t, service, 0, "messages", "thread", page.Messages[3].Ref)
	if thread.Data.Thread == nil || len(thread.Data.Thread.Messages) != 6 || threadBytes > 16*1024 {
		t.Fatalf("thread first page: %d bytes %+v", threadBytes, thread.Data.Thread)
	}
	seed := page.Messages[3].Ref
	initial, _ := invokeGoldenWorkflow(t, service, 0, "messages", "thread", seed, "--limit", "2")
	seen := map[string]bool{}
	for _, message := range initial.Data.Thread.Messages {
		seen[message.Ref] = true
	}
	for _, direction := range []string{"older", "newer"} {
		cursor := initial.Data.Thread.PrevCursor
		if direction == "newer" {
			cursor = initial.Data.Thread.NextCursor
		}
		for steps := 0; cursor != ""; steps++ {
			if steps >= 6 {
				t.Fatal("thread cursor did not terminate")
			}
			next, _ := invokeGoldenWorkflow(t, service, 0, "messages", "thread", seed, "--limit", "2", "--cursor", cursor)
			for _, message := range next.Data.Thread.Messages {
				if seen[message.Ref] {
					t.Fatal("thread continuation repeated a member")
				}
				seen[message.Ref] = true
			}
			cursor = next.Data.Thread.PrevCursor
			if direction == "newer" {
				cursor = next.Data.Thread.NextCursor
			}
		}
	}
	if len(seen) != 6 {
		t.Fatalf("two-direction traversal visited %d of 6 members", len(seen))
	}
	searched, searchBytes := invokeGoldenWorkflow(t, service, 0, "messages", "search", "--query", "Workflow")
	if len(goldenMessagePage(t, searched).Messages) != 6 || searchBytes > 8*1024 {
		t.Fatalf("search first page: %d bytes", searchBytes)
	}
	t.Logf("thread/search first pages: %d/%d bytes", threadBytes, searchBytes)
	for _, operation := range []string{"list", "search", "filter"} {
		args := []string{"messages", operation, "--limit", "2"}
		if operation == "search" {
			args = append(args, "--query", "Workflow")
		}
		if operation == "filter" {
			args = append(args, "--read", "false")
		}
		seen := map[string]bool{}
		for steps, cursor := 0, ""; ; steps++ {
			if steps >= 6 {
				t.Fatal("page cursor did not terminate")
			}
			invocation := append([]string(nil), args...)
			if cursor != "" {
				invocation = append(invocation, "--cursor", cursor)
			}
			result, _ := invokeGoldenWorkflow(t, service, 0, invocation...)
			page := goldenMessagePage(t, result)
			for _, message := range page.Messages {
				if seen[message.Ref] {
					t.Fatalf("%s repeated a message", operation)
				}
				seen[message.Ref] = true
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		if len(seen) != 6 {
			t.Fatalf("%s visited %d of 6 messages", operation, len(seen))
		}
	}
	limited, _ := invokeGoldenWorkflow(t, service, 1, "messages", "get", seed, "--view", "plain", "--max-bytes", "1")
	if limited.Error == nil || limited.Error.Code != "output_too_large" || limited.Next == nil || limited.Next.Do != "fix_input" || limited.Data.Message == nil || limited.Data.Message.Content != "" || limited.Data.RequiredBytes == nil || *limited.Data.RequiredBytes <= 1 {
		t.Fatalf("unsafe content overflow: %+v", limited)
	}
	recovered, recoveredBytes := invokeGoldenWorkflow(t, service, 0, "messages", "get", seed, "--view", "plain", "--max-bytes", strconv.FormatInt(*limited.Data.RequiredBytes, 10))
	if recovered.Data.Message == nil || !strings.Contains(recovered.Data.Message.Content, "Actual workflow body 3") || !recovered.Data.Message.ContentComplete || int64(recoveredBytes) != *limited.Data.RequiredBytes {
		t.Fatalf("read overflow did not recover complete content at its exact byte budget: %+v", recovered.Data.Message)
	}
}

func TestGoldenReplyAndSendUsePreviewRevisionAndNeverResubmit(t *testing.T) {
	submit := &cliSubmitter{}
	mirror := &cliMirror{}
	service := mail.NewServiceWithTransport(goldenWorkflowStore(t), t.TempDir(), mail.SendTransport{
		Submitter: submit, Mirror: mirror, Credentials: cliCredentials{},
	})
	listed, _ := invokeGoldenWorkflow(t, service, 0, "messages", "list")
	ref := goldenMessagePage(t, listed).Messages[0].Ref
	bodyFile := filepath.Join(t.TempDir(), "reply.txt")
	if err := os.WriteFile(bodyFile, []byte("Reviewed reply body"), 0600); err != nil {
		t.Fatal(err)
	}
	reply, _ := invokeGoldenWorkflow(t, service, 0, "messages", "reply", ref, "--body-file", bodyFile, "--from", "sender@icloud.com")
	if reply.Data.Draft == nil {
		t.Fatal("reply did not return its draft ref")
	}
	draft := reply.Data.Draft.Ref
	preview, _ := invokeGoldenWorkflow(t, service, 0, "drafts", "preview", draft)
	review := preview.Data.DraftPreview
	if review == nil || review.Revision == "" || review.Body != "Reviewed reply body" || review.Subject != "Re: Workflow 6" || len(review.To) != 1 || review.To[0].Address != "author@example.com" {
		t.Fatalf("incomplete reply review: %+v", review)
	}
	invokeGoldenWorkflow(t, service, 1, "drafts", "send", draft, "--expected-revision", review.Revision)
	updated, _ := invokeGoldenWorkflow(t, service, 0, "drafts", "update", draft, "--expected-revision", review.Revision, "--subject", "Reviewed updated subject")
	if updated.Data.Draft == nil || updated.Data.Draft.Revision == review.Revision {
		t.Fatal("content update did not invalidate the review revision")
	}
	stale, _ := invokeGoldenWorkflow(t, service, 1, "drafts", "send", draft, "--confirm", "--expected-revision", review.Revision)
	if stale.Error == nil || stale.Error.Code != "draft_revision_conflict" || submit.calls != 0 || mirror.calls != 0 {
		t.Fatalf("unconfirmed/stale send reached transport: %+v %d/%d", stale.Error, submit.calls, mirror.calls)
	}
	preview, _ = invokeGoldenWorkflow(t, service, 0, "drafts", "preview", draft)
	revision := preview.Data.DraftPreview.Revision
	sent, _ := invokeGoldenWorkflow(t, service, 0, "drafts", "send", draft, "--confirm", "--expected-revision", revision)
	if sent.Data.SendResult == nil || sent.Data.SendResult.Outcome != mail.SendOutcomeSent || !sent.Data.SendResult.SubmissionAccepted || !sent.Data.SendResult.SentCopyObserved || submit.calls != 1 || mirror.calls != 1 {
		t.Fatalf("reviewed send did not produce one submission/persistence: %+v %d/%d", sent.Data.SendResult, submit.calls, mirror.calls)
	}
	invokeGoldenWorkflow(t, service, 0, "drafts", "send", draft, "--confirm", "--expected-revision", revision)
	if submit.calls != 1 || mirror.calls != 1 {
		t.Fatal("retained receipt allowed a duplicate submission")
	}
}
