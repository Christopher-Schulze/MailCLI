package mailstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const writeTransactionGenerationKey = "WriteTransactionGeneration"

// searchIndexRevision is a compact best-effort change token. Mail's own
// transaction generation is preferred when present; bounded database, WAL,
// and shared-memory metadata also detects fixture writes and representation
// changes such as WAL checkpoints without scanning the message corpus.
func (s *Store) searchIndexRevision(ctx context.Context) (string, error) {
	generation, err := s.writeTransactionGeneration(ctx)
	if err != nil {
		return "", err
	}
	var material strings.Builder
	writeRevisionPart(&material, "generation", generation)
	if generation != "absent" {
		digest := sha256.Sum256([]byte(material.String()))
		return hex.EncodeToString(digest[:]), nil
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := s.databasePath + suffix
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				writeRevisionPart(&material, suffix, "absent")
				continue
			}
			return "", fmt.Errorf("inspect Envelope Index revision source %q: %w", path, err)
		}
		writeRevisionPart(&material, suffix, strconv.FormatInt(info.Size(), 10))
		writeRevisionPart(&material, suffix+":mtime", strconv.FormatInt(info.ModTime().UnixNano(), 10))
	}
	digest := sha256.Sum256([]byte(material.String()))
	return hex.EncodeToString(digest[:]), nil
}

func (s *Store) writeTransactionGeneration(ctx context.Context) (result string, resultErr error) {
	rows, err := s.database.QueryContext(
		ctx,
		`SELECT value FROM properties WHERE key = ? ORDER BY ROWID LIMIT 2`,
		writeTransactionGenerationKey,
	)
	if err != nil {
		return "", fmt.Errorf("read Envelope Index write generation: %w", err)
	}
	defer joinCloseError(&resultErr, rows, "Envelope Index write generation rows")
	var values []sql.NullString
	for rows.Next() {
		var value sql.NullString
		if err := rows.Scan(&value); err != nil {
			return "", fmt.Errorf("scan Envelope Index write generation: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate Envelope Index write generation: %w", err)
	}
	if len(values) > 1 {
		return "", operationError(
			"unsupported_mail_store_schema",
			"Envelope Index contains duplicate WriteTransactionGeneration properties",
		)
	}
	if len(values) == 0 || !values[0].Valid {
		return "absent", nil
	}
	return values[0].String, nil
}

func writeRevisionPart(builder *strings.Builder, name string, value string) {
	builder.WriteString(name)
	builder.WriteByte(0)
	builder.WriteString(value)
	builder.WriteByte(0)
}
