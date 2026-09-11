package mail

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"mailcli/internal/mailref"
)

const (
	DefaultSearchMaxMessages    = 50_000
	DefaultSearchMaxBytes       = int64(4 * 1024 * 1024 * 1024)
	MaximumSearchMaxMessages    = 100_000
	MaximumSearchMaxBytes       = int64(8 * 1024 * 1024 * 1024)
	SearchConsistencyBestEffort = "best_effort"
	legacySearchCursorVersion   = 3
	searchCursorVersion         = 4
)

const (
	searchCursorReceivedAtNull = 1 << iota
	searchCursorInclusive
)

type Query struct {
	Text          string
	Sender        string
	Recipient     string
	Subject       string
	After         string
	Before        string
	Read          *bool
	Flagged       *bool
	HasAttachment *bool
	AccountRef    string
	MailboxRef    string
	Limit         int
	Cursor        string
	MaxMessages   int
	MaxBytes      int64
	ExactCount    bool
}

type SearchMessage struct {
	Summary MessageSummary `json:"summary"`
	Snippet string         `json:"snippet,omitempty"`
}

type SearchCoverage struct {
	Consistency            string `json:"consistency"`
	IndexRevision          string `json:"index_revision"`
	Backend                string `json:"backend"`
	CandidateMessages      int    `json:"candidate_messages"`
	CandidateMessagesExact bool   `json:"candidate_messages_exact"`
	ScannedMessages        int    `json:"scanned_messages"`
	ScannedBytes           int64  `json:"scanned_bytes"`
	FullSources            int    `json:"full_sources"`
	PartialSources         int    `json:"partial_sources"`
	MissingSources         int    `json:"missing_sources"`
	Complete               bool   `json:"complete"`
	CatalogProvenMessages  int    `json:"catalog_proven_messages,omitempty"`
}

type SearchPage struct {
	Messages   []SearchMessage `json:"messages"`
	NextCursor string          `json:"next_cursor,omitempty"`
	Coverage   SearchCoverage  `json:"coverage"`
}

type PreparedQuery struct {
	Query         Query
	AfterUnix     *int64
	BeforeUnix    *int64
	Fingerprint   string
	Cursor        *SearchCursor
	IndexRevision string
}

type SearchCursor struct {
	Version        int    `json:"version"`
	Fingerprint    string `json:"fingerprint"`
	StoreUUID      string `json:"store_uuid"`
	IndexRevision  string `json:"index_revision"`
	ReceivedAt     int64  `json:"received_at"`
	ReceivedAtNull bool   `json:"received_at_null"`
	RowID          int64  `json:"row_id"`
	Inclusive      bool   `json:"inclusive,omitempty"`
}

type Searcher interface {
	SearchMessages(ctx context.Context, query PreparedQuery) (SearchPage, error)
}

func (s *Service) SearchMessages(ctx context.Context, query Query) (SearchPage, error) {
	prepared, err := PrepareQuery(query)
	if err != nil {
		return SearchPage{}, err
	}
	searcher, ok := s.gateway.(Searcher)
	if !ok {
		return SearchPage{}, &OperationError{
			Code: "search_unavailable", Message: "the selected Mail backend does not support safe search",
		}
	}
	return searcher.SearchMessages(ctx, prepared)
}

func PrepareQuery(query Query) (PreparedQuery, error) {
	limit, err := normalizeLimit(query.Limit)
	if err != nil {
		return PreparedQuery{}, err
	}
	query.Limit = limit
	if query.MaxMessages == 0 {
		query.MaxMessages = DefaultSearchMaxMessages
	}
	if query.MaxMessages < 1 || query.MaxMessages > MaximumSearchMaxMessages {
		return PreparedQuery{}, validationError(fmt.Sprintf(
			"max messages must be between 1 and %d", MaximumSearchMaxMessages,
		))
	}
	if query.MaxBytes == 0 {
		query.MaxBytes = DefaultSearchMaxBytes
	}
	if query.MaxBytes < 1 || query.MaxBytes > MaximumSearchMaxBytes {
		return PreparedQuery{}, validationError(fmt.Sprintf(
			"max bytes must be between 1 and %d", MaximumSearchMaxBytes,
		))
	}
	after, err := parseQueryTime(query.After)
	if err != nil {
		return PreparedQuery{}, validationError(fmt.Sprintf("invalid after date: %v", err))
	}
	before, err := parseQueryTime(query.Before)
	if err != nil {
		return PreparedQuery{}, validationError(fmt.Sprintf("invalid before date: %v", err))
	}
	if after != nil && before != nil && *after >= *before {
		return PreparedQuery{}, validationError("after must be earlier than before")
	}
	fingerprint, err := queryFingerprint(query)
	if err != nil {
		return PreparedQuery{}, err
	}
	prepared := PreparedQuery{
		Query: query, AfterUnix: after, BeforeUnix: before, Fingerprint: fingerprint,
	}
	if query.Cursor != "" {
		prepared.Cursor, err = DecodeSearchCursor(query.Cursor, fingerprint)
		if err != nil {
			return PreparedQuery{}, &OperationError{Code: "invalid_cursor", Message: err.Error()}
		}
	}
	return prepared, nil
}

func EncodeSearchCursor(fingerprint string, storeUUID string, receivedAt int64, receivedAtNull bool, rowID int64) (string, error) {
	return EncodeSearchCursorWithRevision(fingerprint, storeUUID, "legacy", receivedAt, receivedAtNull, rowID)
}

func EncodeSearchCursorWithRevision(
	fingerprint string,
	storeUUID string,
	indexRevision string,
	receivedAt int64,
	receivedAtNull bool,
	rowID int64,
) (string, error) {
	return encodeSearchCursor(fingerprint, storeUUID, indexRevision, receivedAt, receivedAtNull, rowID, false)
}

func EncodeSearchCursorInclusive(
	fingerprint string,
	storeUUID string,
	receivedAt int64,
	receivedAtNull bool,
	rowID int64,
) (string, error) {
	return EncodeSearchCursorInclusiveWithRevision(
		fingerprint, storeUUID, "legacy", receivedAt, receivedAtNull, rowID,
	)
}

func EncodeSearchCursorInclusiveWithRevision(
	fingerprint string,
	storeUUID string,
	indexRevision string,
	receivedAt int64,
	receivedAtNull bool,
	rowID int64,
) (string, error) {
	return encodeSearchCursor(fingerprint, storeUUID, indexRevision, receivedAt, receivedAtNull, rowID, true)
}

func encodeSearchCursor(
	fingerprint string,
	storeUUID string,
	indexRevision string,
	receivedAt int64,
	receivedAtNull bool,
	rowID int64,
	inclusive bool,
) (string, error) {
	var flags uint8
	if receivedAtNull {
		flags |= searchCursorReceivedAtNull
	}
	if inclusive {
		flags |= searchCursorInclusive
	}
	token, err := mailref.EncodeCompactTokenPayload("scur_", &mailref.CompactPayload{
		Fingerprint: fingerprint, StoreUUID: storeUUID, IndexRevision: indexRevision,
		ReceivedAt: receivedAt, Flags: flags, RowID: rowID,
	}, searchCursorVersion)
	if err != nil {
		return "", fmt.Errorf("encode search cursor: %w", err)
	}
	return token, nil
}

func DecodeSearchCursor(value string, fingerprint string) (*SearchCursor, error) {
	payload, err := mailref.DecodeTokenPayload("scur_", value)
	if err != nil {
		return nil, err
	}
	var cursor SearchCursor
	if isLegacyJSONPayload(payload) {
		if err := json.Unmarshal(payload, &cursor); err != nil {
			return nil, fmt.Errorf("parse search cursor: %w", err)
		}
		if cursor.Version != legacySearchCursorVersion {
			return nil, fmt.Errorf("unsupported search cursor version %d (expected %d)", cursor.Version, searchCursorVersion)
		}
	} else {
		compact, err := mailref.DecodeCompactPayload(payload, searchCursorVersion)
		if err != nil {
			return nil, err
		}
		if compact.Flags > searchCursorReceivedAtNull|searchCursorInclusive {
			return nil, fmt.Errorf("unknown search cursor flags 0x%x", compact.Flags)
		}
		cursor = SearchCursor{
			Version: searchCursorVersion, Fingerprint: compact.Fingerprint, StoreUUID: compact.StoreUUID,
			IndexRevision: compact.IndexRevision, ReceivedAt: compact.ReceivedAt,
			ReceivedAtNull: compact.Flags&searchCursorReceivedAtNull != 0, RowID: compact.RowID,
			Inclusive: compact.Flags&searchCursorInclusive != 0,
		}
	}
	if cursor.StoreUUID == "" || cursor.IndexRevision == "" || cursor.RowID < 1 || cursor.Fingerprint != fingerprint {
		return nil, fmt.Errorf("search cursor does not match this query")
	}
	return &cursor, nil
}

func isLegacyJSONPayload(payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

func parseQueryTime(value string) (*int64, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		parsed, err = time.ParseInLocation(time.DateOnly, value, time.Local)
	}
	if err != nil {
		return nil, fmt.Errorf("use RFC 3339 or YYYY-MM-DD")
	}
	timestamp := parsed.Unix()
	return &timestamp, nil
}

func queryFingerprint(query Query) (string, error) {
	query.Cursor = ""
	query.Limit = 0
	payload, err := json.Marshal(query)
	if err != nil {
		return "", fmt.Errorf("encode search query fingerprint: %w", err)
	}
	digest := sha256.Sum256(payload)
	return base64.RawURLEncoding.EncodeToString(digest[:]), nil
}
