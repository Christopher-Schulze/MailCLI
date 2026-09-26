package mail

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestParseQueryTimeTable(t *testing.T) {
	localDate := time.Date(2026, time.August, 23, 0, 0, 0, 0, time.Local).Unix()
	tests := []struct {
		name    string
		value   string
		want    int64
		present bool
		wantErr bool
	}{
		{name: "empty", value: "", want: 0},
		{name: "local calendar day", value: "2026-08-23", want: localDate, present: true},
		{name: "explicit RFC3339 offset", value: "2026-08-23T00:00:00+02:00", want: 1787436000, present: true},
		{name: "UTC epoch", value: "1970-01-01T00:00:00Z", want: 0, present: true},
		{name: "offset epoch", value: "1969-12-31T19:00:00-05:00", want: 0, present: true},
		{name: "negative timestamp", value: "1969-12-31T23:59:59Z", want: -1, present: true},
		{name: "year one is explicit", value: "0001-01-01T00:00:00Z", want: -62135596800, present: true},
		{name: "invalid", value: "23.08.2026", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseQueryTime(test.value)
			if (err != nil) != test.wantErr || (got != nil) != test.present {
				t.Fatalf("parseQueryTime(%q) = %v, %v; want presence %t, wantErr %t", test.value, got, err, test.present, test.wantErr)
			}
			if got != nil && *got != test.want {
				t.Fatalf("parseQueryTime(%q) = %d; want %d", test.value, *got, test.want)
			}
		})
	}
}

func TestPrepareQueryRejectsInvalidDateRangesTable(t *testing.T) {
	tests := []struct {
		name   string
		after  string
		before string
	}{
		{name: "equal bounds", after: "2026-08-23", before: "2026-08-23"},
		{name: "inverted bounds", after: "2026-08-24", before: "2026-08-23"},
		{name: "equal epoch bounds", after: "1970-01-01T00:00:00Z", before: "1970-01-01T00:00:00Z"},
		{name: "zero after negative before", after: "1970-01-01T00:00:00Z", before: "1969-12-31T23:59:59Z"},
		{name: "positive after zero before", after: "1970-01-01T00:00:01Z", before: "1970-01-01T00:00:00Z"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := PrepareQuery(Query{After: test.after, Before: test.before})
			if err == nil || err.Error() != "after must be earlier than before" {
				t.Fatalf("PrepareQuery() error = %v", err)
			}
		})
	}
}

func TestQueryDatesUseProcessLocalMidnight(t *testing.T) {
	for _, test := range []struct {
		zone string
		want int64
	}{
		{"UTC", 0}, {"Etc/GMT+5", 18000}, {"Etc/GMT-2", -7200},
	} {
		if os.Getenv("MAILCLI_DATE_TEST_CHILD") == test.zone {
			prepared, err := PrepareQuery(Query{After: "1970-01-01", Before: "1970-01-02"})
			if err != nil || prepared.AfterUnix == nil || *prepared.AfterUnix != test.want || prepared.BeforeUnix == nil || *prepared.BeforeUnix != test.want+86400 {
				t.Fatalf("local dates in %s: %+v, error=%v", test.zone, prepared, err)
			}
			explicit, err := PrepareQuery(Query{After: "1970-01-01T00:00:00Z", Before: "1970-01-01T01:00:01+01:00"})
			if err != nil || explicit.AfterUnix == nil || *explicit.AfterUnix != 0 || explicit.BeforeUnix == nil || *explicit.BeforeUnix != 1 {
				t.Fatalf("explicit instants changed with zone %s: %+v, error=%v", test.zone, explicit, err)
			}
			return
		}
	}
	for _, zone := range []string{"UTC", "Etc/GMT+5", "Etc/GMT-2"} {
		t.Run(zone, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestQueryDatesUseProcessLocalMidnight$")
			command.Env = append(os.Environ(), "TZ="+zone, "MAILCLI_DATE_TEST_CHILD="+zone)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("date parsing in %s failed: %v\n%s", zone, err, output)
			}
		})
	}
}

func TestPrepareQueryBindsExactCountToCursorFingerprint(t *testing.T) {
	defaultQuery, err := PrepareQuery(Query{Text: "needle"})
	if err != nil {
		t.Fatalf("PrepareQuery(default) error = %v", err)
	}
	exactQuery, err := PrepareQuery(Query{Text: "needle", ExactCount: true})
	if err != nil {
		t.Fatalf("PrepareQuery(exact) error = %v", err)
	}
	if defaultQuery.Fingerprint == exactQuery.Fingerprint {
		t.Fatal("exact-count mode must be bound to the search cursor fingerprint")
	}
}

func TestPrepareQueryCursorAllowsPageBoundsAndRetainsFilters(t *testing.T) {
	base, err := PrepareQuery(Query{Text: "needle", Limit: 1, MaxBytes: 128, MaxMessages: 3})
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := EncodeSearchCursorWithRevision(base.Fingerprint, "store", "revision", 100, false, 42)
	if err != nil {
		t.Fatal(err)
	}
	value := true
	for _, test := range []struct {
		name   string
		change func(*Query)
		valid  bool
	}{
		{name: "page size", change: func(q *Query) { q.Limit = 25 }, valid: true},
		{name: "byte budget", change: func(q *Query) { q.MaxBytes = 256 }, valid: true},
		{name: "candidate bound", change: func(q *Query) { q.MaxMessages = 5 }, valid: true},
		{name: "all page bounds", change: func(q *Query) { q.Limit, q.MaxBytes, q.MaxMessages = 25, 256, 5 }, valid: true},
		{name: "text", change: func(q *Query) { q.Text = "other" }},
		{name: "sender", change: func(q *Query) { q.Sender = "sender@example.com" }},
		{name: "recipient", change: func(q *Query) { q.Recipient = "recipient@example.com" }},
		{name: "subject", change: func(q *Query) { q.Subject = "subject" }},
		{name: "after", change: func(q *Query) { q.After = "2026-01-01" }},
		{name: "before", change: func(q *Query) { q.Before = "2026-02-01" }},
		{name: "read", change: func(q *Query) { q.Read = &value }},
		{name: "flagged", change: func(q *Query) { q.Flagged = &value }},
		{name: "attachment", change: func(q *Query) { q.HasAttachment = &value }},
		{name: "account", change: func(q *Query) { q.AccountRef = "account" }},
		{name: "mailbox", change: func(q *Query) { q.MailboxRef = "mailbox" }},
		{name: "exact count", change: func(q *Query) { q.ExactCount = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := base.Query
			query.Cursor = cursor
			test.change(&query)
			prepared, err := PrepareQuery(query)
			if !test.valid {
				var operation *OperationError
				if !errors.As(err, &operation) || operation.Code != "invalid_cursor" {
					t.Fatalf("changed filter accepted: query=%+v, error=%v", query, err)
				}
				return
			}
			if err != nil || prepared.Fingerprint != base.Fingerprint || prepared.Cursor == nil ||
				prepared.Cursor.StoreUUID != "store" || prepared.Cursor.IndexRevision != "revision" || prepared.Cursor.RowID != 42 ||
				prepared.Query.Limit != query.Limit || prepared.Query.MaxBytes != query.MaxBytes || prepared.Query.MaxMessages != query.MaxMessages {
				t.Fatalf("page bounds changed cursor identity or runtime budgets: prepared=%+v, error=%v", prepared, err)
			}
		})
	}
}
