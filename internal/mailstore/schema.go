package mailstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

const (
	supportedStoreVersion     = "4"
	supportedMinorVersion     = "74003"
	supportedFrameworkVersion = "3826.700.81"
)

type schemaCapability struct {
	StoreUUID        string
	StoreVersion     string
	MinorVersion     string
	FrameworkVersion string
	Fingerprint      string
}

func validateSchema(ctx context.Context, database *sql.DB) (schemaCapability, error) {
	connection, err := database.Conn(ctx)
	if err != nil {
		return schemaCapability{}, fmt.Errorf("acquire Envelope Index schema connection: %w", err)
	}
	defer connection.Close()
	var facts []string
	requiredColumns := requiredColumnCapabilities()
	requiredIndexes := requiredIndexCapabilities()
	tables := sortedKeys(requiredColumns)
	for _, table := range tables {
		columns, err := tableColumns(ctx, connection, table)
		if err != nil {
			return schemaCapability{}, err
		}
		for name, expectedType := range requiredColumns[table] {
			actualType, exists := columns[name]
			if !exists || (expectedType != "" && actualType != expectedType) {
				return schemaCapability{}, operationError(
					"unsupported_mail_store_schema",
					fmt.Sprintf("Envelope Index lacks required capability %s.%s", table, name),
				)
			}
			facts = append(facts, table+"."+name+":"+actualType)
		}
	}
	for _, table := range sortedIndexKeys(requiredIndexes) {
		indexes, err := tableIndexes(ctx, connection, table)
		if err != nil {
			return schemaCapability{}, err
		}
		for _, required := range requiredIndexes[table] {
			if !hasIndexPrefix(indexes, required) {
				return schemaCapability{}, operationError(
					"unsupported_mail_store_schema",
					fmt.Sprintf("Envelope Index lacks required %s index on %s", table, strings.Join(required, ",")),
				)
			}
			facts = append(facts, table+"#"+strings.Join(required, ","))
		}
	}
	capability, err := readSchemaProperties(ctx, connection)
	if err != nil {
		return schemaCapability{}, err
	}
	if capability.StoreVersion != supportedStoreVersion ||
		capability.MinorVersion != supportedMinorVersion ||
		capability.FrameworkVersion != supportedFrameworkVersion ||
		!validUUID(capability.StoreUUID) {
		return schemaCapability{}, operationError(
			"unsupported_mail_store_schema",
			fmt.Sprintf(
				"Envelope Index profile %s/%s/%s is unsupported; supported profile is %s/%s/%s",
				capability.StoreVersion, capability.MinorVersion, capability.FrameworkVersion,
				supportedStoreVersion, supportedMinorVersion, supportedFrameworkVersion,
			),
		)
	}
	facts = append(facts,
		"store_version:"+capability.StoreVersion,
		"minor_version:"+capability.MinorVersion,
		"framework_version:"+capability.FrameworkVersion,
	)
	sort.Strings(facts)
	digest := sha256.Sum256([]byte(strings.Join(facts, "\n")))
	capability.Fingerprint = hex.EncodeToString(digest[:])
	return capability, nil
}

func requiredColumnCapabilities() map[string]map[string]string {
	return map[string]map[string]string{
		"properties": {
			"ROWID": "INTEGER", "key": "", "value": "",
		},
		"mailboxes": {
			"ROWID": "INTEGER", "url": "TEXT", "total_count": "INTEGER",
			"unread_count": "INTEGER", "deleted_count": "INTEGER", "source": "INTEGER",
		},
		"messages": {
			"ROWID": "INTEGER", "message_id": "INTEGER", "global_message_id": "INTEGER",
			"sender": "INTEGER", "subject": "INTEGER", "summary": "INTEGER",
			"date_sent": "INTEGER", "date_received": "INTEGER", "mailbox": "INTEGER",
			"flags": "INTEGER", "read": "INTEGER", "flagged": "INTEGER",
			"deleted": "INTEGER", "size": "INTEGER", "conversation_id": "INTEGER",
			"type": "INTEGER", "display_date": "INTEGER", "flag_color": "INTEGER",
		},
		"addresses": {
			"ROWID": "INTEGER", "address": "TEXT", "comment": "TEXT",
		},
		"subjects": {
			"ROWID": "INTEGER", "subject": "TEXT",
		},
		"summaries": {
			"ROWID": "INTEGER", "summary": "TEXT",
		},
		"recipients": {
			"ROWID": "INTEGER", "message": "INTEGER", "address": "INTEGER",
			"type": "INTEGER", "position": "INTEGER",
		},
		"attachments": {
			"ROWID": "INTEGER", "message": "INTEGER", "attachment_id": "TEXT", "name": "TEXT",
		},
		"labels": {
			"message_id": "INTEGER", "mailbox_id": "INTEGER",
		},
		"server_messages": {
			"message": "INTEGER", "mailbox": "INTEGER", "junk_level": "INTEGER",
			"draft": "INTEGER", "replied": "INTEGER", "forwarded": "INTEGER",
		},
	}
}

func requiredIndexCapabilities() map[string][][]string {
	return map[string][][]string{
		"messages":    {{"mailbox", "date_received"}, {"deleted", "date_received"}},
		"labels":      {{"mailbox_id"}},
		"recipients":  {{"message", "position", "type", "address"}},
		"attachments": {{"message", "attachment_id"}},
	}
}

func tableColumns(ctx context.Context, connection *sql.Conn, table string) (map[string]string, error) {
	rows, err := connection.QueryContext(ctx, "PRAGMA table_xinfo("+quoteIdentifier(table)+")")
	if err != nil {
		return nil, fmt.Errorf("read Envelope Index %s columns: %w", table, err)
	}
	defer rows.Close()
	columns := make(map[string]string)
	for rows.Next() {
		var identifier int
		var name string
		var columnType string
		var notNull int
		var defaultValue sql.NullString
		var primaryKey int
		var hidden int
		if err := rows.Scan(
			&identifier, &name, &columnType, &notNull, &defaultValue, &primaryKey, &hidden,
		); err != nil {
			return nil, fmt.Errorf("scan Envelope Index %s column: %w", table, err)
		}
		columns[name] = strings.ToUpper(columnType)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Envelope Index %s columns: %w", table, err)
	}
	return columns, nil
}

func tableIndexes(ctx context.Context, connection *sql.Conn, table string) ([][]string, error) {
	rows, err := connection.QueryContext(ctx, "PRAGMA index_list("+quoteIdentifier(table)+")")
	if err != nil {
		return nil, fmt.Errorf("read Envelope Index %s indexes: %w", table, err)
	}
	var names []string
	for rows.Next() {
		var sequence int
		var name string
		var unique int
		var origin string
		var partial int
		if err := rows.Scan(&sequence, &name, &unique, &origin, &partial); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan Envelope Index %s index: %w", table, err)
		}
		names = append(names, name)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close Envelope Index %s index list: %w", table, err)
	}
	var indexes [][]string
	for _, name := range names {
		indexRows, err := connection.QueryContext(ctx, "PRAGMA index_info("+quoteIdentifier(name)+")")
		if err != nil {
			return nil, fmt.Errorf("read Envelope Index index %s: %w", name, err)
		}
		var columns []string
		for indexRows.Next() {
			var sequence int
			var identifier int
			var column string
			if err := indexRows.Scan(&sequence, &identifier, &column); err != nil {
				indexRows.Close()
				return nil, fmt.Errorf("scan Envelope Index index %s: %w", name, err)
			}
			columns = append(columns, column)
		}
		if err := indexRows.Close(); err != nil {
			return nil, fmt.Errorf("close Envelope Index index %s: %w", name, err)
		}
		indexes = append(indexes, columns)
	}
	return indexes, nil
}

func readSchemaProperties(ctx context.Context, connection *sql.Conn) (schemaCapability, error) {
	rows, err := connection.QueryContext(ctx, `
		SELECT key, value FROM properties
		WHERE key IN ('UUID', 'version', 'minor_version', 'last_write_framework_version')
	`)
	if err != nil {
		return schemaCapability{}, fmt.Errorf("read Envelope Index properties: %w", err)
	}
	defer rows.Close()
	values := make(map[string]string, 4)
	for rows.Next() {
		var key string
		var value string
		if err := rows.Scan(&key, &value); err != nil {
			return schemaCapability{}, fmt.Errorf("scan Envelope Index property: %w", err)
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return schemaCapability{}, fmt.Errorf("iterate Envelope Index properties: %w", err)
	}
	return schemaCapability{
		StoreUUID: values["UUID"], StoreVersion: values["version"],
		MinorVersion:     values["minor_version"],
		FrameworkVersion: values["last_write_framework_version"],
	}, nil
}

func hasIndexPrefix(indexes [][]string, required []string) bool {
	for _, columns := range indexes {
		if len(columns) < len(required) {
			continue
		}
		matches := true
		for index, column := range required {
			if columns[index] != column {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func sortedKeys(values map[string]map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedIndexKeys(values map[string][][]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
