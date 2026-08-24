package mail

import (
	"testing"
	"time"
)

func TestParseQueryTimeTable(t *testing.T) {
	localDate := time.Date(2026, time.August, 23, 0, 0, 0, 0, time.Local).Unix()
	tests := []struct {
		name    string
		value   string
		want    int64
		wantErr bool
	}{
		{name: "empty", value: "", want: 0},
		{name: "local calendar day", value: "2026-08-23", want: localDate},
		{name: "explicit RFC3339 offset", value: "2026-08-23T00:00:00+02:00", want: 1787436000},
		{name: "invalid", value: "23.08.2026", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseQueryTime(test.value)
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("parseQueryTime(%q) = %d, %v; want %d, wantErr %t", test.value, got, err, test.want, test.wantErr)
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
