package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"mailcli/internal/mail"
)

const catalogListCursorVersion = 1

type catalogListCursor struct {
	Version int    `json:"v"`
	Command string `json:"c"`
	Scope   string `json:"s"`
	Digest  string `json:"d"`
	Offset  int    `json:"o"`
}

type catalogPageMetadata struct {
	Limit      int    `json:"limit"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func validatePageLimit(limit int) error {
	if limit < 1 || limit > mail.MaximumPageLimit {
		return &commandError{code: "invalid_argument", message: fmt.Sprintf(
			"--limit must be between 1 and %d", mail.MaximumPageLimit,
		)}
	}
	return nil
}

func validateCatalogCursorScope(raw, command, scope string) error {
	if raw == "" {
		return nil
	}
	cursor, err := decodeCatalogListCursor(raw)
	if err != nil || cursor.Command != command || cursor.Scope != scope {
		return invalidCatalogCursor(command)
	}
	return nil
}

func paginateCatalog[T any](
	items []T,
	limit int,
	rawCursor string,
	command string,
	scope string,
	identity func(T) string,
) ([]T, string, error) {
	if err := validatePageLimit(limit); err != nil {
		return nil, "", err
	}
	identities := make([]string, len(items))
	seen := make(map[string]struct{}, len(items))
	for index, item := range items {
		value := identity(item)
		if value == "" {
			return nil, "", &mail.OperationError{Code: "list_identity_invalid", Message: "list item is missing its stable identity"}
		}
		if _, exists := seen[value]; exists {
			return nil, "", &mail.OperationError{Code: "list_identity_invalid", Message: "list contains duplicate stable identities"}
		}
		seen[value] = struct{}{}
		identities[index] = value
	}
	digest, err := catalogIdentityDigest(identities)
	if err != nil {
		return nil, "", fmt.Errorf("digest catalog identities: %w", err)
	}
	offset := 0
	if rawCursor != "" {
		cursor, err := decodeCatalogListCursor(rawCursor)
		if err != nil || cursor.Command != command || cursor.Scope != scope || cursor.Digest != digest || cursor.Offset > len(items) {
			return nil, "", invalidCatalogCursor(command)
		}
		offset = cursor.Offset
	}
	end := len(items)
	if limit < len(items)-offset {
		end = offset + limit
	}
	nextCursor := ""
	if end < len(items) {
		nextCursor, err = encodeCatalogListCursor(catalogListCursor{
			Version: catalogListCursorVersion, Command: command, Scope: scope,
			Digest: digest, Offset: end,
		})
		if err != nil {
			return nil, "", fmt.Errorf("encode catalog cursor: %w", err)
		}
	}
	return items[offset:end], nextCursor, nil
}

func decodeCatalogListCursor(raw string) (catalogListCursor, error) {
	var cursor catalogListCursor
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || json.Unmarshal(payload, &cursor) != nil ||
		cursor.Version != catalogListCursorVersion || cursor.Command == "" || cursor.Digest == "" || cursor.Offset < 0 {
		return catalogListCursor{}, fmt.Errorf("invalid catalog cursor")
	}
	return cursor, nil
}

func encodeCatalogListCursor(cursor catalogListCursor) (string, error) {
	payload, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func catalogIdentityDigest(identities []string) (string, error) {
	hash := sha256.New()
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(identities)))
	if _, err := hash.Write(length[:]); err != nil {
		return "", err
	}
	for _, identity := range identities {
		binary.BigEndian.PutUint64(length[:], uint64(len(identity)))
		if _, err := hash.Write(length[:]); err != nil {
			return "", err
		}
		if _, err := io.WriteString(hash, identity); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func invalidCatalogCursor(command string) error {
	return &mail.ValidationError{
		Code: "invalid_cursor", Message: "list cursor is invalid for this command, scope, or current identity order; restart without --cursor",
	}
}

func catalogCursorRecovery(command string) mail.RecoveryGuidance {
	return mail.RecoveryGuidance{
		Action: mail.RecoveryCorrect, Command: command,
		Instruction: "Restart this list without --cursor.",
	}
}

func listOutputRecovery(command string) mail.RecoveryGuidance {
	return mail.RecoveryGuidance{
		Action: mail.RecoveryCorrect, Command: command,
		Instruction: fmt.Sprintf(
			"Reduce --limit and retry with the same incoming --cursor, or raise --max-bytes up to %d; no items or continuation cursor were returned.",
			maximumJSONOutputBytes,
		),
	}
}

func failCatalogCursor(
	command string,
	jsonOutput bool,
	stdout io.Writer,
	stderr io.Writer,
) int {
	recovery := catalogCursorRecovery(command)
	return failCommandWithData(command, jsonOutput, responseData{listRecovery: &recovery}, invalidCatalogCursor(command), stdout, stderr)
}

func writeBoundedListSuccess(
	stdout io.Writer,
	command string,
	data responseData,
	maxBytes int64,
	recovery mail.RecoveryGuidance,
) int {
	if data.StoreProfile == nil && invocationStoreProfile != nil {
		data.StoreProfile = invocationStoreProfile
	}
	payload, err := marshalEnvelope(envelope{
		SchemaVersion: schemaVersion, OK: true, Command: command, Data: data,
	})
	if err != nil {
		return 1
	}
	if int64(len(payload)) > maxBytes {
		limitErr := newOutputTooLargeError(int64(len(payload)), maxBytes, outputSizeExact, command)
		limitErr.recoveryRoute = "reduce --limit or raise --max-bytes"
		recovery.Command = command
		fallback := responseData{listRecovery: &recovery}
		return failCommandWithData(command, true, fallback, limitErr, stdout, io.Discard)
	}
	return writeEnvelopeBytes(stdout, payload)
}

func listCursorScope(values ...string) string {
	return strings.Join(values, "\x00")
}
