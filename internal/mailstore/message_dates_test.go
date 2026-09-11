package mailstore

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

func TestMessageDatesPreserveNullAndEpochAcrossResults(t *testing.T) {
	store, inbox := newSearchFixture(t, 13)
	closeTestResource(t, store, "message date store")
	before, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inbox, Limit: 20})
	if err != nil || len(before.Messages) != 16 {
		t.Fatalf("initial message inventory: %+v, %v", before, err)
	}
	refs := make(map[int]string, 16)
	for _, summary := range before.Messages {
		ref, err := mailref.DecodeMessage(summary.Ref)
		if err != nil {
			t.Fatal(err)
		}
		id, err := strconv.Atoi(ref.LibraryID)
		if err != nil {
			t.Fatal(err)
		}
		refs[id] = summary.Ref
	}
	updateFixtureMessage(t, store, `UPDATE messages SET
		date_received = CASE (ROWID - 101) / 4 WHEN 0 THEN NULL WHEN 1 THEN -1 WHEN 2 THEN 0 ELSE 1 END,
		date_sent = CASE (ROWID - 101) % 4 WHEN 0 THEN NULL WHEN 1 THEN -1 WHEN 2 THEN 0 ELSE 1 END`)
	wantIDs := []int{116, 115, 114, 113, 112, 111, 110, 109, 108, 107, 106, 105, 104, 103, 102, 101}
	var listed []int
	request := mail.ListMessagesRequest{MailboxRef: inbox, Limit: 3}
	for pageNumber := 0; pageNumber < 7; pageNumber++ {
		page, err := store.ListMessages(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		for _, summary := range page.Messages {
			id := verifyFixtureMessageDates(t, summary, refs)
			listed = append(listed, id)
			detail, err := store.GetMessage(context.Background(), summary.Ref)
			if err != nil {
				t.Fatal(err)
			}
			if actual := verifyFixtureMessageDates(t, detail.Summary, refs); actual != id {
				t.Fatalf("detail identity changed: %d, want %d", actual, id)
			}
		}
		if page.NextCursor == "" {
			break
		}
		request.Cursor = page.NextCursor
	}
	if !reflect.DeepEqual(listed, wantIDs) {
		t.Fatalf("list date ordering/continuation: %v, want %v", listed, wantIDs)
	}
	for _, test := range []struct {
		name, after, before string
		ids                 []int
	}{
		{"unbounded", "", "", wantIDs},
		{"epoch and positive", "1970-01-01T00:00:00Z", "", wantIDs[:8]},
		{"negative only", "", "1970-01-01T00:00:00Z", wantIDs[8:12]},
		{"epoch only", "1970-01-01T00:00:00Z", "1970-01-01T00:00:01Z", wantIDs[4:8]},
	} {
		for _, text := range []string{"", "alice"} {
			t.Run(test.name+"/text="+text, func(t *testing.T) {
				query := mail.Query{MailboxRef: inbox, After: test.after, Before: test.before, Text: text, Limit: 3}
				var found []int
				for pageNumber := 0; pageNumber < 7; pageNumber++ {
					prepared, err := mail.PrepareQuery(query)
					if err != nil {
						t.Fatal(err)
					}
					page, err := store.SearchMessages(context.Background(), prepared)
					if err != nil {
						t.Fatal(err)
					}
					for _, message := range page.Messages {
						found = append(found, verifyFixtureMessageDates(t, message.Summary, refs))
					}
					if page.NextCursor == "" {
						if !page.Coverage.Complete {
							t.Fatalf("date search ended incomplete: %+v", page.Coverage)
						}
						break
					}
					query.Cursor = page.NextCursor
				}
				if !reflect.DeepEqual(found, test.ids) {
					t.Fatalf("search date ordering/continuation: %v, want %v", found, test.ids)
				}
			})
		}
	}
}

func verifyFixtureMessageDates(t *testing.T, summary mail.MessageSummary, refs map[int]string) int {
	t.Helper()
	ref, err := mailref.DecodeMessage(summary.Ref)
	if err != nil {
		t.Fatal(err)
	}
	id, err := strconv.Atoi(ref.LibraryID)
	if err != nil || id < 101 || id > 116 {
		t.Fatalf("unexpected fixture message identity: %+v, %v", ref, err)
	}
	// Each group of four rows fixes received presence/value while cycling
	// through every sent-date state, independently of the formatter.
	dates := []string{"", "1969-12-31T23:59:59Z", "1970-01-01T00:00:00Z", "1970-01-01T00:00:01Z"}
	wantReceived, wantSent := dates[(id-101)/4], dates[(id-101)%4]
	if summary.DateReceived != wantReceived || summary.DateSent != wantSent || summary.Ref != refs[id] {
		t.Errorf("message %d dates=(%q,%q), want=(%q,%q), identity unchanged=%t", id, summary.DateReceived, summary.DateSent, wantReceived, wantSent, summary.Ref == refs[id])
	}
	return id
}

func TestObservedMessageDatesPreserveNullAndEpoch(t *testing.T) {
	for _, received := range []struct{ sql, want string }{
		{"NULL", ""}, {"-1", "1969-12-31T23:59:59Z"}, {"0", "1970-01-01T00:00:00Z"}, {"1", "1970-01-01T00:00:01Z"},
	} {
		for _, sent := range []struct{ sql, want string }{
			{"NULL", ""}, {"-1", "1969-12-31T23:59:59Z"}, {"0", "1970-01-01T00:00:00Z"}, {"1", "1970-01-01T00:00:01Z"},
		} {
			t.Run(received.sql+"/"+sent.sql, func(t *testing.T) {
				store, _ := newSearchFixture(t)
				closeTestResource(t, store, "observation date store")
				installSentMailboxFixture(t, store)
				baseline, err := store.captureSendBaseline(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				insertSentMessageFixture(t, store, 104)
				updateFixtureMessage(t, store, fmt.Sprintf("UPDATE messages SET date_received = %s, date_sent = %s WHERE ROWID = 104", received.sql, sent.sql))
				baseline.CapturedUnix = 1
				for _, requireSentDate := range []bool{false, true} {
					summary, found, err := store.findMailboxCandidate(context.Background(), baseline, observedDraft("Body"), requireSentDate)
					wantFound := !requireSentDate || sent.sql != "NULL"
					if err != nil || found != wantFound {
						t.Fatalf("observation date filter changed: found=%t want=%t error=%v", found, wantFound, err)
					}
					if found && (summary.DateReceived != received.want || summary.DateSent != sent.want) {
						t.Errorf("observed dates=(%q,%q), want=(%q,%q)", summary.DateReceived, summary.DateSent, received.want, sent.want)
					}
				}
			})
		}
	}
}
