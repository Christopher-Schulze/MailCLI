package mailstore

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

func TestSearchExplicitEpochBeforeExcludesModernMessages(t *testing.T) {
	store, inbox := newSearchFixture(t)
	closeTestResource(t, store, "epoch filter store")
	updateFixtureMessage(t, store, `UPDATE messages SET date_received = date_received + 1700000000`)
	service := mail.NewService(&Client{store: store})
	for _, test := range []struct {
		name, before string
		count        int
	}{
		{"omitted", "", 3},
		{"explicit UTC epoch", "1970-01-01T00:00:00Z", 0},
		{"equivalent offset epoch", "1970-01-01T01:00:00+01:00", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			page, err := service.SearchMessages(context.Background(), mail.Query{MailboxRef: inbox, Before: test.before, Limit: 10, ExactCount: true})
			if err != nil || len(page.Messages) != test.count || page.Coverage.CandidateMessages != test.count || !page.Coverage.CandidateMessagesExact || !page.Coverage.Complete {
				t.Fatalf("before=%q: page=%+v, error=%v", test.before, page, err)
			}
		})
	}
}

func TestSearchDatePaginationPreservesBoundsAndNullness(t *testing.T) {
	for _, test := range []struct {
		name, after, before string
		ids                 []string
		localMidnight       bool
	}{
		{"unbounded", "", "", []string{"104", "103", "102", "101", "105"}, false},
		{"before epoch", "", "1970-01-01T00:00:00Z", []string{"102", "101"}, false},
		{"after epoch", "1970-01-01T00:00:00Z", "", []string{"104", "103"}, false},
		{"negative through zero", "1969-12-31T23:59:58Z", "1970-01-01T00:00:01Z", []string{"103", "102", "101"}, false},
		{"offset epoch", "1969-12-31T19:00:00-05:00", "1970-01-01T01:00:02+01:00", []string{"104", "103"}, false},
		{"local calendar dates", "1970-01-01", "1970-01-02", []string{"104", "103"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, inbox := newSearchFixture(t, 2)
			closeTestResource(t, store, "paginated date store")
			epoch := int64(0)
			if test.localMidnight {
				epoch = time.Date(1970, time.January, 1, 0, 0, 0, 0, time.Local).Unix()
			}
			updateFixtureMessage(t, store, fmt.Sprintf(`UPDATE messages SET date_received = CASE ROWID WHEN 101 THEN -2 WHEN 102 THEN -1 WHEN 103 THEN 0 WHEN 104 THEN 1 ELSE NULL END + %d`, epoch))
			service := mail.NewService(&Client{store: store})
			for _, text := range []string{"", "alice"} {
				query := mail.Query{MailboxRef: inbox, After: test.after, Before: test.before, Text: text, Limit: 1}
				var ids []string
				for pageNumber := 0; pageNumber < 6; pageNumber++ {
					page, err := service.SearchMessages(context.Background(), query)
					if err != nil {
						t.Fatal(err)
					}
					for _, result := range page.Messages {
						ref, err := mailref.DecodeMessage(result.Summary.Ref)
						if err != nil {
							t.Fatal(err)
						}
						ids = append(ids, ref.LibraryID)
					}
					if page.NextCursor == "" {
						if !page.Coverage.Complete {
							t.Fatalf("last date page incomplete: %+v", page)
						}
						break
					}
					prepared, err := mail.PrepareQuery(query)
					if err != nil {
						t.Fatal(err)
					}
					cursor, err := mail.DecodeSearchCursor(page.NextCursor, prepared.Fingerprint)
					if err != nil || cursor.ReceivedAtNull {
						t.Fatalf("date cursor invalid: %+v, error=%v", cursor, err)
					}
					if cursor.RowID == 103 && cursor.ReceivedAt != epoch {
						t.Fatalf("epoch cursor changed: %+v", cursor)
					}
					query.Cursor, query.Limit = page.NextCursor, 2
				}
				if !reflect.DeepEqual(ids, test.ids) {
					t.Fatalf("text=%q: paginated IDs=%v, want=%v", text, ids, test.ids)
				}
			}
		})
	}
}

func TestSearchDateBoundsIncludeEpochAndNegativeTimes(t *testing.T) {
	store, inbox := newSearchFixture(t, 2)
	closeTestResource(t, store, "mixed date store")
	updateFixtureMessage(t, store, `UPDATE messages SET date_received = CASE ROWID WHEN 101 THEN -2 WHEN 102 THEN -1 WHEN 103 THEN 0 WHEN 104 THEN 1 ELSE NULL END`)
	service := mail.NewService(&Client{store: store})
	for _, test := range []struct {
		name, after, before string
		ids                 []string
	}{
		{"unbounded includes NULL", "", "", []string{"104", "103", "102", "101", "105"}},
		{"before epoch", "", "1970-01-01T00:00:00Z", []string{"102", "101"}},
		{"after epoch inclusive", "1970-01-01T00:00:00Z", "", []string{"104", "103"}},
		{"negative through epoch", "1969-12-31T23:59:58Z", "1970-01-01T00:00:00Z", []string{"102", "101"}},
		{"negative before", "", "1969-12-31T23:59:59Z", []string{"101"}},
		{"negative after", "1969-12-31T23:59:59Z", "", []string{"104", "103", "102"}},
		{"epoch only", "1970-01-01T00:00:00Z", "1970-01-01T00:00:01Z", []string{"103"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, text := range []string{"", "alice"} {
				for _, exact := range []bool{false, true} {
					page, err := service.SearchMessages(context.Background(), mail.Query{MailboxRef: inbox, After: test.after, Before: test.before, Text: text, ExactCount: exact, Limit: 10})
					if err != nil {
						t.Fatal(err)
					}
					var ids []string
					for _, result := range page.Messages {
						ref, err := mailref.DecodeMessage(result.Summary.Ref)
						if err != nil {
							t.Fatal(err)
						}
						ids = append(ids, ref.LibraryID)
					}
					if !reflect.DeepEqual(ids, test.ids) || !page.Coverage.Complete || !page.Coverage.CandidateMessagesExact || page.Coverage.CandidateMessages != len(test.ids) || page.NextCursor != "" {
						t.Fatalf("text=%q, exact=%v: ids=%v, want=%v, page=%+v", text, exact, ids, test.ids, page)
					}
				}
			}
		})
	}
}
