package mailstore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestSchemaIndexCompatibility(t *testing.T) {
	for _, profile := range []struct {
		name, minor, framework string
		verified               bool
	}{
		{"legacy", "74003", "3826.700.81", true},
		{"compatible", "90006", "3901.200.71.1.16", false},
	} {
		for _, test := range []struct {
			name       string
			statements []string
			failure    string
		}{
			{"ordinary indexes", nil, ""},
			{"optional expression", []string{`CREATE INDEX optional_expression ON messages(lower(subject))`}, ""},
			{"optional partial expression", []string{`CREATE INDEX optional_expression ON messages(lower(subject)) WHERE deleted=0`}, ""},
			{"expression after required prefix", []string{
				`DROP INDEX messages_mailbox_date_received`,
				`CREATE INDEX extended_prefix ON messages(mailbox,date_received,lower(subject))`,
			}, ""},
			{"expression interrupts required prefix", []string{
				`DROP INDEX messages_mailbox_date_received`,
				`CREATE INDEX interrupted_prefix ON messages(mailbox,abs(subject),date_received)`,
			}, "index on mailbox,date_received"},
			{"expression precedes required prefix", []string{
				`DROP INDEX labels_mailbox`,
				`CREATE INDEX interrupted_prefix ON labels(abs(message_id),mailbox_id)`,
			}, "index on mailbox_id"},
			{"partial index alone", []string{
				`DROP INDEX labels_mailbox`,
				`CREATE INDEX filtered_labels ON labels(mailbox_id) WHERE message_id>0`,
			}, "index on mailbox_id"},
			{"partial plus full index", []string{
				`CREATE INDEX filtered_labels ON labels(mailbox_id) WHERE message_id>0`,
			}, ""},
			{"unknown profile with optional expression", []string{
				`CREATE INDEX optional_expression ON messages(lower(subject))`,
				`UPDATE properties SET value='99999' WHERE key='minor_version'`,
			}, "profile"},
		} {
			t.Run(profile.name+"/"+test.name, func(t *testing.T) {
				ctx := context.Background()
				path := filepath.Join(t.TempDir(), envelopeIndexName)
				writer := openTestWriter(t, path)
				closeTestResource(t, writer, "index fixture writer")
				createTestSchema(t, writer, "")
				for key, value := range map[string]string{
					"minor_version": profile.minor, "last_write_framework_version": profile.framework,
				} {
					if _, err := writer.ExecContext(ctx, `UPDATE properties SET value=? WHERE key=?`, value, key); err != nil {
						t.Fatal(err)
					}
				}
				for _, statement := range test.statements {
					if _, err := writer.ExecContext(ctx, statement); err != nil {
						t.Fatal(err)
					}
				}
				reader, err := openReadOnlyDatabase(ctx, path)
				if err != nil {
					t.Fatal(err)
				}
				closeTestResource(t, reader, "index fixture reader")
				capability, err := validateSchema(ctx, reader)
				if test.failure != "" {
					if nestedErrorCode(err) != "unsupported_mail_store_schema" || !strings.Contains(err.Error(), test.failure) {
						t.Fatalf("error=%v, want a typed refusal naming %q", err, test.failure)
					}
					return
				}
				if err != nil || capability.Fingerprint == "" || capability.ProfileVerified != profile.verified {
					t.Fatalf("capability=%+v error=%v", capability, err)
				}
				if _, err := reader.ExecContext(ctx, `UPDATE properties SET value='5' WHERE key='version'`); err == nil {
					t.Fatal("compatible indexes permitted a database write")
				}
			})
		}
	}
}
