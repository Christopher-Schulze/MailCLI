package mailstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type Store struct {
	database          *sql.DB
	versionRoot       string
	versionDirectory  *os.File
	storeUUID         string
	activeAccounts    []mailboxLocation
	activeAccountKeys map[string]struct{}
	capability        schemaCapability

	// The mailbox catalog is memoized for the Store's read-only lifetime (one
	// CLI invocation). Freshness of individual message membership lives in the
	// per-message SQL checks, not here. The counter is a test hook.
	mailboxCatalogOnce    sync.Once
	mailboxCatalog        []mailboxRecord
	mailboxCatalogErr     error
	mailboxCatalogQueries int
}

func Open(ctx context.Context, config Config) (*Store, error) {
	versionRoot, err := discoverVersionRoot(config.MailRoot)
	if err != nil {
		return nil, err
	}
	if filepath.Base(versionRoot) != "V10" {
		return nil, operationError(
			"unsupported_mail_store_schema",
			fmt.Sprintf("Mail store layout %q is unsupported; supported layout is V10", filepath.Base(versionRoot)),
		)
	}
	accountURLs, err := loadActiveAccountURLs(ctx, config)
	if err != nil {
		return nil, err
	}
	activeKeys := make(map[string]struct{}, len(accountURLs))
	activeAccounts := make([]mailboxLocation, 0, len(accountURLs))
	for _, value := range accountURLs {
		location, err := parseAccountRoot(value)
		if err != nil {
			return nil, operationError(
				"mail_store_preferences_invalid",
				fmt.Sprintf("Mail AccountOrdering contains an unsafe account URL: %v", err),
			)
		}
		key := location.rootKey()
		if _, exists := activeKeys[key]; exists {
			continue
		}
		activeKeys[key] = struct{}{}
		activeAccounts = append(activeAccounts, location)
	}
	versionDirectory, err := openVersionDirectory(versionRoot)
	if err != nil {
		return nil, err
	}
	databasePath := filepath.Join(versionRoot, "MailData", envelopeIndexName)
	database, err := openReadOnlyDatabase(ctx, databasePath)
	if err != nil {
		resultErr := err
		joinCloseError(&resultErr, versionDirectory, "Mail store root")
		return nil, resultErr
	}
	capability, err := validateSchema(ctx, database)
	if err != nil {
		resultErr := err
		joinCloseError(&resultErr, database, "Envelope Index database")
		joinCloseError(&resultErr, versionDirectory, "Mail store root")
		return nil, resultErr
	}
	return &Store{
		database: database, versionRoot: versionRoot, storeUUID: capability.StoreUUID,
		versionDirectory: versionDirectory,
		activeAccounts:   activeAccounts, activeAccountKeys: activeKeys, capability: capability,
	}, nil
}

func (s *Store) Close() error {
	return errors.Join(s.database.Close(), s.versionDirectory.Close())
}

func (s *Store) SchemaFingerprint() string {
	return s.capability.Fingerprint
}

func openVersionDirectory(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect Mail store root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, operationError("unsafe_message_source", "Mail store root is not a regular directory")
	}
	directory, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open Mail store root: %w", err)
	}
	opened, err := directory.Stat()
	if err != nil || !os.SameFile(info, opened) {
		resultErr := error(operationError("store_changed", "Mail store root changed while opening"))
		if err != nil {
			resultErr = fmt.Errorf("inspect opened Mail store root: %w", err)
		}
		joinCloseError(&resultErr, directory, "Mail store root")
		return nil, resultErr
	}
	return directory, nil
}
