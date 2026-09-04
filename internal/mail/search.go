package mail

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	DefaultSearchMaxMessages = 50_000
	DefaultSearchMaxBytes    = int64(4 * 1024 * 1024 * 1024)
	MaximumSearchMaxMessages = 100_000
	MaximumSearchMaxBytes    = int64(8 * 1024 * 1024 * 1024)
	searchCursorVersion      = 1
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
}

type SearchMessage struct {
	Summary MessageSummary `json:"summary"`
	Snippet string         `json:"snippet,omitempty"`
}

type SearchCoverage struct {
	Backend               string `json:"backend"`
	CandidateMessages     int    `json:"candidate_messages"`
	ScannedMessages       int    `json:"scanned_messages"`
	ScannedBytes          int64  `json:"scanned_bytes"`
	FullSources           int    `json:"full_sources"`
	PartialSources        int    `json:"partial_sources"`
	MissingSources        int    `json:"missing_sources"`
	Complete              bool   `json:"complete"`
	CatalogProvenMessages int    `json:"catalog_proven_messages,omitempty"`
}

type SearchPage struct {
	Messages   []SearchMessage `json:"messages"`
	NextCursor string          `json:"next_cursor,omitempty"`
	Coverage   SearchCoverage  `json:"coverage"`
}

type PreparedQuery struct {
	Query       Query
	AfterUnix   int64
	BeforeUnix  int64
	Fingerprint string
	Cursor      *SearchCursor
}

type SearchCursor struct {
	Version     int    `json:"version"`
	Fingerprint string `json:"fingerprint"`
	StoreUUID   string `json:"store_uuid"`
	ReceivedAt  int64  `json:"received_at"`
	RowID       int64  `json:"row_id"`
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
	if after != 0 && before != 0 && after >= before {
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

func EncodeSearchCursor(fingerprint string, storeUUID string, receivedAt int64, rowID int64) (string, error) {
	payload, err := json.Marshal(SearchCursor{
		Version: searchCursorVersion, Fingerprint: fingerprint, StoreUUID: storeUUID,
		ReceivedAt: receivedAt, RowID: rowID,
	})
	if err != nil {
		return "", fmt.Errorf("encode search cursor: %w", err)
	}
	return "scur_" + base64.RawURLEncoding.EncodeToString(payload), nil
}

func DecodeSearchCursor(value string, fingerprint string) (*SearchCursor, error) {
	if !strings.HasPrefix(value, "scur_") {
		return nil, fmt.Errorf("invalid search cursor prefix")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, "scur_"))
	if err != nil {
		return nil, fmt.Errorf("decode search cursor: %w", err)
	}
	var cursor SearchCursor
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return nil, fmt.Errorf("parse search cursor: %w", err)
	}
	if cursor.Version != searchCursorVersion || cursor.StoreUUID == "" || cursor.RowID < 1 || cursor.Fingerprint != fingerprint {
		return nil, fmt.Errorf("search cursor does not match this query")
	}
	return &cursor, nil
}

func parseQueryTime(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.Unix(), nil
	}
	if parsed, err := time.ParseInLocation(time.DateOnly, value, time.Local); err == nil {
		return parsed.Unix(), nil
	}
	return 0, fmt.Errorf("use RFC 3339 or YYYY-MM-DD")
}

func queryFingerprint(query Query) (string, error) {
	query.Cursor = ""
	payload, err := json.Marshal(query)
	if err != nil {
		return "", fmt.Errorf("encode search query fingerprint: %w", err)
	}
	digest := sha256.Sum256(payload)
	return base64.RawURLEncoding.EncodeToString(digest[:]), nil
}
