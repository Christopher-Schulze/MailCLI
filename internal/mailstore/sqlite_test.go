package mailstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattn/go-sqlite3"
)

func TestOpenReadOnlyDatabaseSeesWALAndRejectsWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "Envelope Index")
	writer := openTestWriter(t, path)
	closeTestResource(t, writer, "fixture writer")
	if _, err := writer.ExecContext(ctx, "PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatalf("disable WAL autocheckpoint: %v", err)
	}
	if _, err := writer.ExecContext(ctx, "CREATE TABLE values_table(value TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := writer.ExecContext(ctx, "INSERT INTO values_table(value) VALUES ('visible-in-wal')"); err != nil {
		t.Fatalf("insert WAL row: %v", err)
	}

	reader, err := openReadOnlyDatabase(ctx, path)
	if err != nil {
		t.Fatalf("openReadOnlyDatabase() error = %v", err)
	}
	closeTestResource(t, reader, "read-only database")
	var value string
	if err := reader.QueryRowContext(ctx, "SELECT value FROM values_table").Scan(&value); err != nil {
		t.Fatalf("read WAL row: %v", err)
	}
	if value != "visible-in-wal" {
		t.Fatalf("read value = %q", value)
	}
	if _, err := reader.ExecContext(ctx, "INSERT INTO values_table(value) VALUES ('forbidden')"); err == nil {
		t.Fatal("read-only insert error = nil")
	}
}

func TestValidateSchemaAcceptsCapabilitiesAndRejectsMissingColumn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		omit      string
		wantError string
	}{
		{name: "supported"},
		{name: "missing attachment id", omit: "attachment_id", wantError: "attachments.attachment_id"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "Envelope Index")
			writer := openTestWriter(t, path)
			createTestSchema(t, writer, test.omit)
			if err := writer.Close(); err != nil {
				t.Fatalf("close writer: %v", err)
			}
			reader, err := openReadOnlyDatabase(ctx, path)
			if err != nil {
				t.Fatalf("openReadOnlyDatabase() error = %v", err)
			}
			closeTestResource(t, reader, "read-only database")
			capability, err := validateSchema(ctx, reader)
			if test.wantError == "" {
				if err != nil || capability.Fingerprint == "" {
					t.Fatalf("validateSchema() = %#v, %v", capability, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("validateSchema() error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestValidateSchemaRejectsUnknownSemanticProfile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "future minor", key: "minor_version", value: "99999"},
		{name: "future framework", key: "last_write_framework_version", value: "9999.1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "Envelope Index")
			writer := openTestWriter(t, path)
			createTestSchema(t, writer, "")
			if _, err := writer.ExecContext(
				ctx, "UPDATE properties SET value = ? WHERE key = ?", test.value, test.key,
			); err != nil {
				t.Fatalf("update schema profile: %v", err)
			}
			if err := writer.Close(); err != nil {
				t.Fatalf("close writer: %v", err)
			}
			reader, err := openReadOnlyDatabase(ctx, path)
			if err != nil {
				t.Fatalf("openReadOnlyDatabase() error = %v", err)
			}
			closeTestResource(t, reader, "read-only database")
			_, err = validateSchema(ctx, reader)
			if err == nil || !strings.Contains(err.Error(), "profile") {
				t.Fatalf("validateSchema() error = %v, want unsupported profile", err)
			}
		})
	}
}

func TestValidateSchemaPropertyPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		statements []string
		wantError  string
	}{
		{
			name:       "missing required property",
			statements: []string{`DELETE FROM properties WHERE key = 'minor_version'`},
			wantError:  "unsupported_mail_store_schema",
		},
		{
			name:       "identical duplicate",
			statements: []string{`INSERT INTO properties(key, value) VALUES ('UUID', 'AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE')`},
			wantError:  "unsupported_mail_store_schema",
		},
		{
			name:       "conflicting duplicate",
			statements: []string{`INSERT INTO properties(key, value) VALUES ('UUID', 'FFFFFFFF-EEEE-4DDD-8CCC-BBBBBBBBBBBB')`},
			wantError:  "unsupported_mail_store_schema",
		},
		{
			name: "conflicting duplicate reversed",
			statements: []string{
				`DELETE FROM properties WHERE key = 'UUID'`,
				`INSERT INTO properties(key, value) VALUES ('UUID', 'FFFFFFFF-EEEE-4DDD-8CCC-BBBBBBBBBBBB')`,
				`INSERT INTO properties(key, value) VALUES ('UUID', 'AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE')`,
			},
			wantError: "unsupported_mail_store_schema",
		},
		{
			name:       "malformed required value",
			statements: []string{`UPDATE properties SET value = NULL WHERE key = 'UUID'`},
			wantError:  "unsupported_mail_store_schema",
		},
		{
			name:       "unknown property ignored",
			statements: []string{`INSERT INTO properties(key, value) VALUES ('future_property', 'ignored')`},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "Envelope Index")
			writer := openTestWriter(t, path)
			createTestSchema(t, writer, "")
			for _, statement := range test.statements {
				if _, err := writer.ExecContext(ctx, statement); err != nil {
					closeTestResourceNow(t, writer, "fixture writer")
					t.Fatalf("execute property mutation %q: %v", statement, err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatalf("close writer: %v", err)
			}
			reader, err := openReadOnlyDatabase(ctx, path)
			if err != nil {
				t.Fatalf("openReadOnlyDatabase() error = %v", err)
			}
			closeTestResource(t, reader, "read-only database")
			_, err = validateSchema(ctx, reader)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("validateSchema() error = %v, want success", err)
				}
				return
			}
			if errorCodeForTest(err) != test.wantError {
				t.Fatalf("validateSchema() error = %v, want %s", err, test.wantError)
			}
			if test.name != "missing required property" && !strings.Contains(err.Error(), "property") {
				t.Fatalf("validateSchema() error = %v, want property evidence", err)
			}
		})
	}
}

func openTestWriter(t *testing.T, path string) *sql.DB {
	t.Helper()
	database := sql.OpenDB(&sqliteConnector{
		driver: &sqlite3.SQLiteDriver{}, dsn: path,
	})
	if _, err := database.Exec("PRAGMA journal_mode=WAL"); err != nil {
		closeTestResourceNow(t, database, "test database")
		t.Fatalf("enable WAL: %v", err)
	}
	return database
}

func createTestSchema(t *testing.T, database *sql.DB, omit string) {
	t.Helper()
	attachmentID := ", attachment_id TEXT"
	if omit == "attachment_id" {
		attachmentID = ""
	}
	statements := []string{
		`CREATE TABLE properties (ROWID INTEGER PRIMARY KEY, key, value)`,
		`CREATE TABLE mailboxes (ROWID INTEGER PRIMARY KEY, url TEXT NOT NULL, total_count INTEGER, unread_count INTEGER, deleted_count INTEGER, source INTEGER)`,
		`CREATE TABLE messages (ROWID INTEGER PRIMARY KEY, message_id INTEGER, global_message_id INTEGER, sender INTEGER, subject INTEGER, summary INTEGER, date_sent INTEGER, date_received INTEGER, mailbox INTEGER, flags INTEGER, read INTEGER, flagged INTEGER, deleted INTEGER, size INTEGER, conversation_id INTEGER, type INTEGER, display_date INTEGER, flag_color INTEGER)`,
		`CREATE TABLE addresses (ROWID INTEGER PRIMARY KEY, address TEXT, comment TEXT)`,
		`CREATE TABLE subjects (ROWID INTEGER PRIMARY KEY, subject TEXT)`,
		`CREATE TABLE summaries (ROWID INTEGER PRIMARY KEY, summary TEXT)`,
		`CREATE TABLE recipients (ROWID INTEGER PRIMARY KEY, message INTEGER, address INTEGER, type INTEGER, position INTEGER)`,
		`CREATE TABLE attachments (ROWID INTEGER PRIMARY KEY, message INTEGER` + attachmentID + `, name TEXT)`,
		`CREATE TABLE labels (message_id INTEGER, mailbox_id INTEGER)`,
		`CREATE TABLE server_messages (message INTEGER, mailbox INTEGER, junk_level INTEGER, draft INTEGER, replied INTEGER, forwarded INTEGER)`,
		`CREATE INDEX messages_mailbox_date_received ON messages(mailbox, date_received)`,
		`CREATE INDEX messages_deleted_date_received ON messages(deleted, date_received)`,
		`CREATE INDEX labels_mailbox ON labels(mailbox_id)`,
		`CREATE INDEX recipients_message ON recipients(message, position, type, address)`,
	}
	if omit != "attachment_id" {
		statements = append(statements, `CREATE INDEX attachments_message ON attachments(message, attachment_id)`)
	}
	statements = append(statements,
		`INSERT INTO properties(key, value) VALUES ('UUID', 'AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE')`,
		`INSERT INTO properties(key, value) VALUES ('version', '4')`,
		`INSERT INTO properties(key, value) VALUES ('minor_version', '74003')`,
		`INSERT INTO properties(key, value) VALUES ('last_write_framework_version', '3826.700.81')`,
	)
	for _, statement := range statements {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("execute test schema statement %q: %v", statement, err)
		}
	}
}
