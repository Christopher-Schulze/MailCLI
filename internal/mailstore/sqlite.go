package mailstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"path/filepath"

	"github.com/mattn/go-sqlite3"
)

type sqliteConnector struct {
	driver driver.Driver
	dsn    string
}

func (c *sqliteConnector) Connect(context.Context) (driver.Conn, error) {
	return c.driver.Open(c.dsn)
}

func (c *sqliteConnector) Driver() driver.Driver {
	return c.driver
}

func openReadOnlyDatabase(ctx context.Context, path string) (*sql.DB, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve Envelope Index path: %w", err)
	}
	uri := &url.URL{Scheme: "file", Path: absolutePath}
	query := uri.Query()
	query.Set("mode", "ro")
	query.Set("cache", "private")
	query.Set("_query_only", "1")
	query.Set("_busy_timeout", "1000")
	query.Set("_txlock", "deferred")
	uri.RawQuery = query.Encode()
	database := sql.OpenDB(&sqliteConnector{
		driver: &sqlite3.SQLiteDriver{}, dsn: uri.String(),
	})
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	if err := configureReadConnection(ctx, database, absolutePath); err != nil {
		resultErr := err
		joinCloseError(&resultErr, database, "Envelope Index database")
		return nil, resultErr
	}
	return database, nil
}

func configureReadConnection(
	ctx context.Context,
	database *sql.DB,
	expectedPath string,
) (resultErr error) {
	canonicalExpectedPath, err := filepath.EvalSymlinks(expectedPath)
	if err != nil {
		return fmt.Errorf("resolve Envelope Index identity: %w", err)
	}
	connection, err := database.Conn(ctx)
	if err != nil {
		return operationError("mail_store_unavailable", fmt.Sprintf("open Envelope Index read-only: %v", err))
	}
	defer joinCloseError(&resultErr, connection, "Envelope Index connection")
	for _, statement := range []string{
		"PRAGMA trusted_schema=OFF", "PRAGMA temp_store=MEMORY",
	} {
		if _, err := connection.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure read-only Envelope Index: %w", err)
		}
	}
	var queryOnly int
	if err := connection.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
		return fmt.Errorf("read Envelope Index query_only state: %w", err)
	}
	if queryOnly != 1 {
		return operationError("mail_store_not_read_only", "Envelope Index connection is not query-only")
	}
	var journalMode string
	if err := connection.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		return fmt.Errorf("read Envelope Index journal mode: %w", err)
	}
	if journalMode != "wal" {
		return operationError(
			"unsupported_mail_store_schema",
			fmt.Sprintf("Envelope Index journal mode is %q, want WAL", journalMode),
		)
	}
	rows, err := connection.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return fmt.Errorf("read Envelope Index database list: %w", err)
	}
	defer joinCloseError(&resultErr, rows, "Envelope Index database list rows")
	for rows.Next() {
		var sequence int
		var name string
		var actualPath string
		if err := rows.Scan(&sequence, &name, &actualPath); err != nil {
			return fmt.Errorf("scan Envelope Index database list: %w", err)
		}
		if name != "main" {
			continue
		}
		canonicalActualPath, err := filepath.EvalSymlinks(actualPath)
		if err != nil {
			return fmt.Errorf("resolve opened Envelope Index identity: %w", err)
		}
		if filepath.Clean(canonicalActualPath) != filepath.Clean(canonicalExpectedPath) {
			return operationError(
				"mail_store_path_mismatch",
				fmt.Sprintf("SQLite opened %q, expected Envelope Index %q", actualPath, expectedPath),
			)
		}
	}
	return rows.Err()
}
