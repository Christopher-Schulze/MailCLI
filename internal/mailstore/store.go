package mailstore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
)

type Store struct {
	database          *sql.DB
	versionRoot       string
	storeUUID         string
	activeAccounts    []mailboxLocation
	activeAccountKeys map[string]struct{}
	capability        schemaCapability
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
	databasePath := filepath.Join(versionRoot, "MailData", envelopeIndexName)
	database, err := openReadOnlyDatabase(ctx, databasePath)
	if err != nil {
		return nil, err
	}
	capability, err := validateSchema(ctx, database)
	if err != nil {
		resultErr := err
		joinCloseError(&resultErr, database, "Envelope Index database")
		return nil, resultErr
	}
	return &Store{
		database: database, versionRoot: versionRoot, storeUUID: capability.StoreUUID,
		activeAccounts: activeAccounts, activeAccountKeys: activeKeys, capability: capability,
	}, nil
}

func (s *Store) Close() error {
	return s.database.Close()
}

func (s *Store) SchemaFingerprint() string {
	return s.capability.Fingerprint
}
