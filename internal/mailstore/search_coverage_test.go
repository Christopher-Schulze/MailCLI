package mailstore

import (
	"context"
	"os"
	"testing"

	"mailcli/internal/mail"
)

func TestSearchPageSourceCompletenessSurvivesPagination(t *testing.T) {
	tests := []struct {
		name string
		loss bool
	}{
		{name: "full sources"},
		{name: "missing", loss: true},
		{name: "partial", loss: true},
		{name: "malformed RFC", loss: true},
		{name: "incomplete MIME", loss: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store, inboxRef := newSearchFixture(t, 2)
			closeTestResource(t, store, "source coverage fixture store")
			location, err := parseMailboxURL("imap://" + testAccountID + "/INBOX")
			if err != nil {
				t.Fatal(err)
			}
			base, err := store.messageBasePath(location, 104)
			if err != nil {
				t.Fatal(err)
			}
			switch test.name {
			case "missing":
				if err := os.Remove(base + ".emlx"); err != nil {
					t.Fatal(err)
				}
			case "partial":
				if err := os.Rename(base+".emlx", base+".partial.emlx"); err != nil {
					t.Fatal(err)
				}
			case "malformed RFC":
				writeFixtureEMLX(t, store, 104, "imap://"+testAccountID+"/INBOX", []byte("broken header\r\n\r\nneedle\r\n"))
			case "incomplete MIME":
				writeFixtureEMLX(t, store, 104, "imap://"+testAccountID+"/INBOX", []byte(
					"To: broken <\r\nContent-Type: text/plain\r\n\r\nneedle\r\n",
				))
			}
			query := mail.Query{MailboxRef: inboxRef, Subject: "Status", Text: "needle", Limit: 1}
			allSourcesComplete := true
			revision := ""
			for pageNumber := 0; pageNumber < 4; pageNumber++ {
				page, err := store.SearchMessages(context.Background(), mustPrepareQuery(t, query))
				if err != nil {
					t.Fatal(err)
				}
				wantSources := !test.loss || pageNumber > 0
				if page.Coverage.SourcesComplete != wantSources {
					t.Fatalf("page %d coverage = %+v, want sources_complete=%t", pageNumber, page.Coverage, wantSources)
				}
				if pageNumber == 0 {
					revision = page.Coverage.IndexRevision
					if page.Coverage.Complete || page.NextCursor == "" {
						t.Fatalf("first page must continue: %+v", page)
					}
					if test.name == "missing" && page.Coverage.MissingSources != 1 ||
						test.name == "partial" && page.Coverage.PartialSources != 1 ||
						(test.name == "malformed RFC" || test.name == "incomplete MIME") &&
							(page.Coverage.FullSources == 0 || page.Coverage.PartialSources != 0 || page.Coverage.MissingSources != 0) {
						t.Fatalf("source classification = %+v", page.Coverage)
					}
				}
				if page.Coverage.Consistency != mail.SearchConsistencyBestEffort || page.Coverage.IndexRevision != revision {
					t.Fatalf("scan identity/consistency changed: %+v", page.Coverage)
				}
				allSourcesComplete = allSourcesComplete && page.Coverage.SourcesComplete
				if page.NextCursor == "" {
					if pageNumber == 0 || !page.Coverage.Complete || !page.Coverage.SourcesComplete || allSourcesComplete == test.loss {
						t.Fatalf("terminal page %d = %+v, scan sources_complete=%t", pageNumber, page, allSourcesComplete)
					}
					return
				}
				query.Cursor = page.NextCursor
			}
			t.Fatal("search did not reach a clean terminal page")
		})
	}
}

func TestSearchSourceCompletenessUsesCatalogProof(t *testing.T) {
	for _, hasAttachment := range []bool{true, false} {
		t.Run(map[bool]string{true: "positive", false: "negative"}[hasAttachment], func(t *testing.T) {
			t.Parallel()
			store, inboxRef := newSearchFixture(t)
			closeTestResource(t, store, "catalog coverage fixture store")
			location, err := parseMailboxURL("imap://" + testAccountID + "/%5BGmail%5D/All")
			if err != nil {
				t.Fatal(err)
			}
			base, err := store.messageBasePath(location, 101)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(base + ".emlx"); err != nil {
				t.Fatal(err)
			}
			query := mail.Query{MailboxRef: inboxRef, Subject: "Quarterly", HasAttachment: &hasAttachment, MaxBytes: 1}
			page, err := store.SearchMessages(context.Background(), mustPrepareQuery(t, query))
			wantMatches := 0
			if hasAttachment {
				wantMatches = 1
			}
			if err != nil || len(page.Messages) != wantMatches || page.NextCursor != "" || !page.Coverage.Complete ||
				!page.Coverage.SourcesComplete || page.Coverage.CatalogProvenMessages != 1 ||
				page.Coverage.ScannedMessages != 0 || page.Coverage.ScannedBytes != 0 || page.Coverage.MissingSources != 0 {
				t.Fatalf("catalog-proven page = %+v, error = %v", page, err)
			}
			query.Text = "needle"
			page, err = store.SearchMessages(context.Background(), mustPrepareQuery(t, query))
			if err != nil || page.Coverage.SourcesComplete || page.Coverage.Complete ||
				page.Coverage.MissingSources != 1 || page.Coverage.CatalogProvenMessages != 0 {
				t.Fatalf("catalog does not prove text: %+v, error = %v", page, err)
			}
		})
	}
}

func TestSearchContinuationDoesNotLoseSources(t *testing.T) {
	for _, sourceScan := range []bool{false, true} {
		page := emptySearchPage(sourceScan)
		if !page.Coverage.SourcesComplete || !page.Coverage.Complete || page.Messages == nil || len(page.Messages) != 0 {
			t.Fatalf("empty page source_scan=%t: %+v", sourceScan, page)
		}
	}
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "continuation coverage fixture store")
	metadata, err := store.SearchMessages(context.Background(), mustPrepareQuery(t, mail.Query{MailboxRef: inboxRef, Limit: 1}))
	if err != nil {
		t.Fatal(err)
	}
	_, source, err := store.openMessageSource(context.Background(), metadata.Messages[0].Summary.Ref)
	if err != nil {
		t.Fatal(err)
	}
	maxBytes := source.length
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		query     mail.Query
		continues bool
	}{
		{name: "SQL pagination", query: mail.Query{MailboxRef: inboxRef, Limit: 1}, continues: true},
		{name: "empty SQL", query: mail.Query{Subject: "absent"}},
		{name: "empty stream", query: mail.Query{Subject: "absent", Text: "needle"}},
		{name: "message budget", query: mail.Query{MailboxRef: inboxRef, Text: "needle", MaxMessages: 1}, continues: true},
		{name: "byte budget", query: mail.Query{MailboxRef: inboxRef, Text: "needle", MaxBytes: maxBytes}, continues: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			page, err := store.SearchMessages(context.Background(), mustPrepareQuery(t, test.query))
			if err != nil || !page.Coverage.SourcesComplete || (page.NextCursor != "") != test.continues {
				t.Fatalf("page = %+v, error = %v", page, err)
			}
			if test.query.Text != "" && test.continues && page.Coverage.Complete {
				t.Fatalf("budget continuation must retain old complete=false: %+v", page)
			}
		})
	}
}
