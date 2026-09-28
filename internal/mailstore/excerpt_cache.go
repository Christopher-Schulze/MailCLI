package mailstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// excerptCacheTTL bounds how long an IMAP excerpt stays reusable.
	excerptCacheTTL = 30 * 24 * time.Hour
	// maximumExcerptCacheEntryBytes bounds one entry: a 1,000-rune excerpt
	// is at most 4,000 bytes of UTF-8 before JSON escaping.
	maximumExcerptCacheEntryBytes = 32 * 1024
	excerptCachePruneMarker       = ".pruned"
)

// excerptCache keeps IMAP-fetched excerpts, cut to the maximum excerpt length,
// so later pages reuse them without contacting the server. An empty dir
// disables it. Every failure degrades to a cache miss.
type excerptCache struct {
	dir string
}

// excerptCacheVersion marks entries written by this layout; entries without it
// or with another value are ignored and overwritten.
const excerptCacheVersion = 1

type cachedExcerpt struct {
	Version  int    `json:"v"`
	Excerpt  string `json:"excerpt"`
	Complete bool   `json:"complete"`
}

func defaultExcerptCacheDirectory(home string) string {
	return filepath.Join(home, "Library", "Caches", "MailCLI", "excerpts")
}

// excerptCacheKey identifies a message by local store identity only, so a
// lookup needs no account, credential or server access. A changed server UID
// or store row yields a different key.
func (s *Store) excerptCacheKey(ctx context.Context, ref string) (string, error) {
	resolved, err := s.resolveMessage(ctx, ref)
	if err != nil {
		return "", err
	}
	record := resolved.Record
	if record.RemoteID <= 0 {
		return "", errors.New("message has no server UID")
	}
	return excerptCacheIdentity{
		storeUUID: s.storeUUID, accountID: resolved.Reference.AccountID, mailboxPath: resolved.Reference.MailboxPath,
		rowID: record.RowID, storeGlobalID: record.StoreGlobalID, remoteID: record.RemoteID,
		uidValidity: resolved.Reference.ExpectedIMAPUIDValidity,
	}.key(), nil
}

// excerptCacheIdentity is everything a cache key is derived from. The server
// UIDVALIDITY (0 while unknown) separates two generations of one mailbox.
type excerptCacheIdentity struct {
	storeUUID     string
	accountID     string
	mailboxPath   []string
	rowID         int64
	storeGlobalID int64
	remoteID      int64
	uidValidity   uint32
}

func (identity excerptCacheIdentity) key() string {
	text := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%d",
		identity.storeUUID, identity.accountID, strings.Join(identity.mailboxPath, "/"),
		identity.rowID, identity.storeGlobalID, identity.remoteID, identity.uidValidity)
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func (cache excerptCache) path(key string) string {
	return filepath.Join(cache.dir, key+".json")
}

func (cache excerptCache) load(key string) (cachedExcerpt, bool) {
	if cache.dir == "" || key == "" {
		return cachedExcerpt{}, false
	}
	file, err := os.Open(cache.path(key))
	if err != nil {
		return cachedExcerpt{}, false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximumExcerptCacheEntryBytes || time.Since(info.ModTime()) > excerptCacheTTL {
		return cachedExcerpt{}, false
	}
	var entry cachedExcerpt
	if err := json.NewDecoder(io.LimitReader(file, maximumExcerptCacheEntryBytes)).Decode(&entry); err != nil {
		return cachedExcerpt{}, false
	}
	if entry.Version != excerptCacheVersion {
		return cachedExcerpt{}, false
	}
	return entry, true
}

// store writes the entry atomically with owner-only permissions and prunes
// expired entries at most once a day.
func (cache excerptCache) store(key string, entry cachedExcerpt) error {
	if cache.dir == "" || key == "" {
		return nil
	}
	if err := os.MkdirAll(cache.dir, 0o700); err != nil {
		return err
	}
	entry.Version = excerptCacheVersion
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(cache.dir, "."+key+".*")
	if err != nil {
		return err
	}
	_, writeErr := temporary.Write(data)
	closeErr := temporary.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return errors.Join(err, os.Remove(temporary.Name()))
	}
	if err := os.Rename(temporary.Name(), cache.path(key)); err != nil {
		return errors.Join(err, os.Remove(temporary.Name()))
	}
	return cache.pruneDaily()
}

func (cache excerptCache) pruneDaily() error {
	marker := filepath.Join(cache.dir, excerptCachePruneMarker)
	if info, err := os.Lstat(marker); err == nil && info.Mode().IsRegular() && time.Since(info.ModTime()) < 24*time.Hour {
		return nil
	}
	entries, err := os.ReadDir(cache.dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err == nil && time.Since(info.ModTime()) > excerptCacheTTL {
			errs = append(errs, os.Remove(filepath.Join(cache.dir, entry.Name())))
		}
	}
	errs = append(errs, replaceFile(cache.dir, marker))
	return errors.Join(errs...)
}

// replaceFile publishes an empty regular file at path through a temporary file
// and a rename, so an existing symlink at path is replaced and never followed.
func replaceFile(directory string, path string) error {
	temporary, err := os.CreateTemp(directory, ".marker.*")
	if err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return errors.Join(err, os.Remove(temporary.Name()))
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return errors.Join(err, os.Remove(temporary.Name()))
	}
	return nil
}

// cutExcerpt shortens a cached maximum-length excerpt to length runes; it
// equals mail.BuildExcerpt at that length because both cut the same
// normalized text at a rune boundary.
func cutExcerpt(excerpt string, length int) string {
	for index := range excerpt {
		if length == 0 {
			return excerpt[:index]
		}
		length--
	}
	return excerpt
}
