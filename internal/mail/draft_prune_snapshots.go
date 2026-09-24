package mail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

const (
	orphanSnapshotParentDepth = iota
	orphanSnapshotAttemptDepth
	orphanSnapshotIndexDepth
	orphanSnapshotDirectoryBatchSize = 128
)

// Each level owns a pinned root and verifies its name through the already
// pinned parent. Descendant operations never resolve a multi-component path.
type orphanSnapshotDirectory struct {
	root     *os.Root
	parent   *os.Root
	name     string
	identity os.FileInfo
	check    func() error
}

func openOrphanSnapshotDirectory(parent *os.Root, name string, expected os.FileInfo, check func() error) (*orphanSnapshotDirectory, error) {
	if !expected.IsDir() || expected.Mode()&os.ModeSymlink != 0 {
		return nil, draftLockUnsafeError("orphan snapshot path is not a real directory")
	}
	if err := check(); err != nil {
		return nil, err
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("open orphan snapshot directory: %w", err)
	}
	directory := &orphanSnapshotDirectory{root: root, parent: parent, name: name, identity: expected, check: check}
	opened, err := root.Stat(".")
	if err == nil && !os.SameFile(expected, opened) {
		err = draftLockChangedError("orphan snapshot directory changed while opening")
	}
	if err == nil {
		err = directory.verify()
	}
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	return directory, nil
}

func (d *orphanSnapshotDirectory) verify() error {
	if err := d.check(); err != nil {
		return err
	}
	current, err := d.parent.Lstat(d.name)
	if err != nil {
		return fmt.Errorf("recheck orphan snapshot directory: %w", err)
	}
	if !current.IsDir() || current.Mode().Perm() != d.identity.Mode().Perm() || !os.SameFile(d.identity, current) {
		return draftLockChangedError("orphan snapshot directory changed before cleanup")
	}
	return nil
}

func orphanDraftCleanupCheck(ctx context.Context, storage *draftStorage, ref string) error {
	if err := draftContextError(ctx, "prune"); err != nil {
		return err
	}
	_, err := storage.lstat(ref + ".json")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("recheck orphan draft: %w", err)
	}
	return &OperationError{Code: "prune_state_changed", Message: "draft reappeared during orphan cleanup; skipped"}
}

func removeOrphanHandoffSnapshotTree(ctx context.Context, ref string, storage *draftStorage) (resultErr error) {
	checkDraft := func() error { return orphanDraftCleanupCheck(ctx, storage, ref) }
	if err := checkDraft(); err != nil {
		return err
	}
	claimName := ref + handoffClaimSuffix
	claimIdentity, err := storage.lstat(claimName)
	if os.IsNotExist(err) {
		if _, snapshotErr := storage.lstat(ref + handoffSnapshotSuffix); os.IsNotExist(snapshotErr) {
			return nil
		} else if snapshotErr != nil {
			return fmt.Errorf("inspect unclaimed handoff snapshots: %w", snapshotErr)
		}
		return draftLockUnsafeError("orphan handoff snapshots have no owning claim; preserved")
	}
	if err != nil {
		return fmt.Errorf("inspect orphan handoff claim: %w", err)
	}
	if !claimIdentity.Mode().IsRegular() {
		return draftLockUnsafeError("orphan handoff claim is not a regular file")
	}
	attempt, err := readHandoffAttempt(ref, storage)
	if err != nil {
		return fmt.Errorf("read orphan handoff claim: %w", err)
	}
	if attempt == nil {
		return draftLockChangedError("orphan handoff claim disappeared before cleanup")
	}
	if attempt.DispatchStarted || attempt.Outcome != HandoffOutcomePrepared {
		return &OperationError{
			Code:    "prune_state_changed",
			Message: "orphan handoff reached external dispatch; its claim and snapshots were preserved",
		}
	}
	checkClaim := func() error {
		if err := checkDraft(); err != nil {
			return err
		}
		return preparedHandoffClaimCheck(ref, *attempt, claimIdentity, storage)
	}
	parentName := ref + handoffSnapshotSuffix
	parentIdentity, parentErr := storage.lstat(parentName)
	if parentErr != nil && !os.IsNotExist(parentErr) {
		return fmt.Errorf("inspect orphan handoff snapshot parent: %w", parentErr)
	}
	if os.IsNotExist(parentErr) {
		if len(attempt.Snapshots) > 0 {
			return draftLockChangedError("claimed orphan handoff snapshots are missing")
		}
	} else {
		parentDirectory, openErr := openOrphanSnapshotDirectory(storage.root, parentName, parentIdentity, checkClaim)
		if openErr != nil {
			return openErr
		}
		entries, readErr := readPinnedSnapshotEntries(parentDirectory.root, checkClaim, 2)
		closeErr := parentDirectory.root.Close()
		if readErr != nil {
			return errors.Join(fmt.Errorf("inspect orphan handoff snapshot attempts: %w", readErr), closeErr)
		}
		if closeErr != nil {
			return closeErr
		}
		if len(entries) > 1 || (len(entries) == 1 && entries[0].Name() != attempt.ID) {
			return draftLockUnsafeError("orphan handoff snapshot parent contains an unclaimed attempt")
		}
		if len(entries) == 0 && len(attempt.Snapshots) > 0 {
			return draftLockChangedError("claimed orphan handoff attempt directory is missing")
		}
	}
	if err := removeHandoffSnapshotAttempt(ref, *attempt, attempt.Snapshots, false, storage, checkClaim); err != nil {
		return err
	}
	if parentErr == nil {
		if err := removeEmptyOrphanSnapshotParent(storage, parentName, parentIdentity, checkClaim); err != nil {
			return err
		}
	}
	return removePreparedHandoffAttempt(ref, *attempt, claimIdentity, storage)
}

type handoffSnapshotIndex struct {
	index     int
	directory *orphanSnapshotDirectory
	fileName  string
	fileInfo  os.FileInfo
}

func sameHandoffSnapshots(left, right []HandoffSnapshot) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sameHandoffAttempt(left, right HandoffAttempt) bool {
	return left.ID == right.ID && left.DraftRef == right.DraftRef &&
		left.StartedAt.Equal(right.StartedAt) && left.UpdatedAt.Equal(right.UpdatedAt) &&
		left.Outcome == right.Outcome && left.DispatchStarted == right.DispatchStarted &&
		left.SnapshotsRetained == right.SnapshotsRetained && sameHandoffSnapshots(left.Snapshots, right.Snapshots)
}

func preparedHandoffClaimCheck(ref string, expected HandoffAttempt, identity os.FileInfo, storage *draftStorage) error {
	name := ref + handoffClaimSuffix
	current, err := storage.lstat(name)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(identity, current) {
		return draftLockChangedError("prepared handoff claim changed before snapshot cleanup")
	}
	attempt, err := readHandoffAttempt(ref, storage)
	if err != nil {
		return fmt.Errorf("recheck prepared handoff claim: %w", err)
	}
	if attempt == nil || !sameHandoffAttempt(expected, *attempt) || attempt.DispatchStarted || attempt.Outcome != HandoffOutcomePrepared {
		return draftLockChangedError("prepared handoff attempt changed before snapshot cleanup")
	}
	current, err = storage.lstat(name)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(identity, current) {
		return draftLockChangedError("prepared handoff claim changed while being checked")
	}
	return nil
}

func removePreparedHandoffAttempt(ref string, attempt HandoffAttempt, identity os.FileInfo, storage *draftStorage) error {
	if err := preparedHandoffClaimCheck(ref, attempt, identity, storage); err != nil {
		return err
	}
	return removeDraftStorageFile(storage, ref+handoffClaimSuffix, identity, "handoff claim")
}

func recoverPreparedHandoffStaging(ctx context.Context, ref string, attemptID string, attachments []DraftAttachment, storage *draftStorage) error {
	if !validHandoffAttemptID(attemptID) {
		return validationError("invalid handoff attempt id")
	}
	claimName := ref + handoffClaimSuffix
	claimIdentity, err := storage.lstat(claimName)
	if err != nil {
		return draftLockChangedError("prepared handoff claim is missing before snapshot recovery")
	}
	if !claimIdentity.Mode().IsRegular() {
		return draftLockUnsafeError("prepared handoff claim is not a regular file")
	}
	attempt, err := readHandoffAttempt(ref, storage)
	if err != nil {
		return fmt.Errorf("read prepared handoff claim: %w", err)
	}
	if attempt == nil || attempt.ID != attemptID || attempt.DispatchStarted || attempt.Outcome != HandoffOutcomePrepared {
		return draftLockChangedError("prepared handoff attempt changed before snapshot recovery")
	}
	expected := handoffSnapshotsFromAttachments(attachments)
	if len(attempt.Snapshots) > 0 && !sameHandoffSnapshots(attempt.Snapshots, expected) {
		return draftLockChangedError("prepared handoff snapshot list differs from the draft")
	}
	check := func() error {
		if err := draftContextError(ctx, "handoff"); err != nil {
			return err
		}
		return preparedHandoffClaimCheck(ref, *attempt, claimIdentity, storage)
	}
	allowPartial := len(attempt.Snapshots) == 0
	if err := removeHandoffSnapshotAttempt(ref, *attempt, expected, allowPartial, storage, check); err != nil {
		return err
	}
	return removePreparedHandoffAttempt(ref, *attempt, claimIdentity, storage)
}

func handoffSnapshotsFromAttachments(attachments []DraftAttachment) []HandoffSnapshot {
	snapshots := make([]HandoffSnapshot, 0, len(attachments))
	for _, attachment := range attachments {
		snapshots = append(snapshots, HandoffSnapshot{
			Name: filepath.Base(attachment.Path), Size: attachment.Size, SHA256: attachment.SHA256,
		})
	}
	return snapshots
}

func readPinnedSnapshotEntries(root *os.Root, check func() error, maximumEntries int) ([]os.DirEntry, error) {
	reader, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("open pinned snapshot directory: %w", err)
	}
	var entries []os.DirEntry
	for {
		if check != nil {
			if err := check(); err != nil {
				return nil, errors.Join(err, reader.Close())
			}
		}
		batch, readErr := reader.ReadDir(orphanSnapshotDirectoryBatchSize)
		entries = append(entries, batch...)
		if len(entries) > maximumEntries {
			return nil, errors.Join(draftLockUnsafeError("pinned snapshot directory has too many entries"), reader.Close())
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, errors.Join(fmt.Errorf("read pinned snapshot directory: %w", readErr), reader.Close())
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	if err := reader.Close(); err != nil {
		return nil, err
	}
	return entries, nil
}

func removeHandoffSnapshotAttempt(
	ref string,
	attempt HandoffAttempt,
	expected []HandoffSnapshot,
	allowPartial bool,
	storage *draftStorage,
	check func() error,
) (resultErr error) {
	if !validHandoffAttemptID(attempt.ID) || storage == nil || storage.root == nil {
		return draftLockUnsafeError("handoff snapshot cleanup lacks a pinned valid attempt")
	}
	if check == nil {
		check = func() error { return nil }
	}
	if check != nil {
		if err := check(); err != nil {
			return err
		}
	}
	parentName := ref + handoffSnapshotSuffix
	parentIdentity, err := storage.lstat(parentName)
	if os.IsNotExist(err) {
		if !allowPartial && len(expected) > 0 {
			return draftLockChangedError("handoff snapshot parent disappeared before cleanup")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect handoff snapshot parent: %w", err)
	}
	if parentIdentity.Mode().Perm() != 0o700 {
		return draftLockUnsafeError("handoff snapshot parent has an unsafe mode")
	}
	parentDirectory, err := openOrphanSnapshotDirectory(storage.root, parentName, parentIdentity, check)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, parentDirectory.root.Close()) }()
	attemptIdentity, err := parentDirectory.root.Lstat(attempt.ID)
	if os.IsNotExist(err) {
		if !allowPartial && len(expected) > 0 {
			return draftLockChangedError("handoff snapshot attempt disappeared before cleanup")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect handoff snapshot attempt: %w", err)
	}
	if attemptIdentity.Mode().Perm() != 0o700 {
		return draftLockUnsafeError("handoff snapshot attempt has an unsafe mode")
	}
	attemptDirectory, err := openOrphanSnapshotDirectory(parentDirectory.root, attempt.ID, attemptIdentity, parentDirectory.verify)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, attemptDirectory.root.Close()) }()
	entries, err := readPinnedSnapshotEntries(attemptDirectory.root, check, len(expected)+1)
	if err != nil {
		return err
	}
	if len(entries) > len(expected) {
		return draftLockUnsafeError("handoff snapshot attempt has unexpected index directories")
	}
	indexes := make([]handoffSnapshotIndex, 0, len(entries))
	defer func() {
		for _, index := range indexes {
			resultErr = errors.Join(resultErr, index.directory.root.Close())
		}
	}()
	for _, entry := range entries {
		index, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil || index < 0 || index >= len(expected) || strconv.Itoa(index) != entry.Name() {
			return draftLockUnsafeError("handoff snapshot attempt has an unexpected index")
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return fmt.Errorf("inspect handoff snapshot index: %w", infoErr)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
			return draftLockUnsafeError("handoff snapshot index is not a private directory")
		}
		current, statErr := attemptDirectory.root.Lstat(entry.Name())
		if statErr != nil || !current.IsDir() || !os.SameFile(info, current) {
			return draftLockChangedError("handoff snapshot index changed while being inspected")
		}
		indexDirectory, openErr := openOrphanSnapshotDirectory(attemptDirectory.root, entry.Name(), info, attemptDirectory.verify)
		if openErr != nil {
			return openErr
		}
		indexes = append(indexes, handoffSnapshotIndex{index: index, directory: indexDirectory})
	}
	sort.Slice(indexes, func(left, right int) bool { return indexes[left].index < indexes[right].index })
	for position := range indexes {
		index := &indexes[position]
		if allowPartial && index.index != position {
			return draftLockUnsafeError("partial handoff snapshot indexes are not a prefix")
		}
		files, readErr := readPinnedSnapshotEntries(index.directory.root, check, 2)
		if readErr != nil {
			return readErr
		}
		if len(files) > 1 {
			return draftLockUnsafeError("handoff snapshot index contains multiple files")
		}
		if len(files) == 0 {
			continue
		}
		file := files[0]
		expectation := expected[index.index]
		if file.Name() != expectation.Name || filepath.Base(expectation.Name) != expectation.Name {
			return draftLockUnsafeError("handoff snapshot file has an unexpected name")
		}
		info, infoErr := file.Info()
		if infoErr != nil {
			return fmt.Errorf("inspect handoff snapshot file: %w", infoErr)
		}
		mode := info.Mode().Perm()
		if !info.Mode().IsRegular() || (mode != 0o400 && (!allowPartial || mode != 0o600)) {
			return draftLockUnsafeError("handoff snapshot file is not a private regular file")
		}
		if (allowPartial && info.Size() > expectation.Size+1) || (!allowPartial && info.Size() != expectation.Size) {
			return draftLockChangedError("handoff snapshot file size changed before cleanup")
		}
		current, statErr := index.directory.root.Lstat(file.Name())
		if statErr != nil || !current.Mode().IsRegular() || current.Mode().Perm() != mode ||
			current.Size() != info.Size() || !os.SameFile(info, current) {
			return draftLockChangedError("handoff snapshot file changed while being inspected")
		}
		index.fileName = file.Name()
		index.fileInfo = info
	}
	if allowPartial {
		for position, index := range indexes {
			if index.fileInfo == nil && position != len(indexes)-1 {
				return draftLockUnsafeError("only the final partial handoff snapshot index may be empty")
			}
		}
	}
	if check != nil {
		if err := check(); err != nil {
			return err
		}
	}
	for _, index := range indexes {
		if index.fileInfo != nil {
			if err := removeOrphanSnapshotFile(index.directory, index.fileName, index.fileInfo); err != nil {
				return err
			}
		}
		if err := index.directory.verify(); err != nil {
			return err
		}
		remaining, err := readPinnedSnapshotEntries(index.directory.root, check, 1)
		if err != nil {
			return fmt.Errorf("recheck handoff snapshot index after cleanup: %w", err)
		}
		if len(remaining) != 0 {
			return draftLockChangedError(fmt.Sprintf("handoff snapshot index retained %q during cleanup", remaining[0].Name()))
		}
		if err := index.directory.parent.Remove(index.directory.name); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove handoff snapshot index: %w", err)
		}
	}
	if err := attemptDirectory.verify(); err != nil {
		return err
	}
	remaining, err := readPinnedSnapshotEntries(attemptDirectory.root, check, 1)
	if err != nil {
		return fmt.Errorf("recheck handoff snapshot attempt after cleanup: %w", err)
	}
	if len(remaining) != 0 {
		return draftLockChangedError(fmt.Sprintf("handoff snapshot attempt retained %d entries during cleanup", len(remaining)))
	}
	if err := attemptDirectory.parent.Remove(attemptDirectory.name); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove handoff snapshot attempt: %w", err)
	}
	return storage.apply(draftStorageSync, "", "", 0)
}

func removeEmptyOrphanSnapshotParent(storage *draftStorage, name string, identity os.FileInfo, check func() error) (resultErr error) {
	directory, err := openOrphanSnapshotDirectory(storage.root, name, identity, check)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.root.Close()) }()
	entries, err := readPinnedSnapshotEntries(directory.root, check, 1)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return draftLockUnsafeError("orphan handoff snapshot parent changed during cleanup")
	}
	if err := directory.verify(); err != nil {
		return err
	}
	return storage.root.Remove(name)
}

func removeOrphanSnapshotDirectory(directory *orphanSnapshotDirectory, depth int) (resultErr error) {
	if err := directory.verify(); err != nil {
		return err
	}
	reader, err := directory.root.Open(".")
	if err != nil {
		return fmt.Errorf("list orphan snapshot directory: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	for {
		entries, readErr := reader.ReadDir(orphanSnapshotDirectoryBatchSize)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("read orphan snapshot directory: %w", readErr)
		}
		if depth == orphanSnapshotIndexDepth && len(entries) > 1 {
			return draftLockUnsafeError("orphan snapshot index contains multiple files")
		}
		for _, entry := range entries {
			if err := removeOrphanSnapshotEntry(directory, entry, depth); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	if err := directory.verify(); err != nil {
		return err
	}
	return directory.parent.Remove(directory.name)
}

func removeOrphanSnapshotEntry(directory *orphanSnapshotDirectory, entry os.DirEntry, depth int) (resultErr error) {
	if err := directory.verify(); err != nil {
		return err
	}
	info, err := entry.Info()
	if err != nil {
		return fmt.Errorf("inspect orphan snapshot entry: %w", err)
	}
	if depth == orphanSnapshotIndexDepth {
		return removeOrphanSnapshotFile(directory, entry.Name(), info)
	}
	if !validOrphanSnapshotDirectoryName(entry.Name(), depth) {
		return draftLockUnsafeError("orphan snapshot directory has an unexpected name")
	}
	child, err := openOrphanSnapshotDirectory(directory.root, entry.Name(), info, directory.verify)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, child.root.Close()) }()
	return removeOrphanSnapshotDirectory(child, depth+1)
}

func validOrphanSnapshotDirectoryName(name string, depth int) bool {
	if depth == orphanSnapshotParentDepth {
		return validHandoffAttemptID(name)
	}
	index, err := strconv.Atoi(name)
	return depth == orphanSnapshotAttemptDepth && err == nil && index >= 0 && index < MaximumDraftAttachments && strconv.Itoa(index) == name
}

func removeOrphanSnapshotFile(directory *orphanSnapshotDirectory, name string, expected os.FileInfo) error {
	if err := directory.verify(); err != nil {
		return err
	}
	current, err := directory.root.Lstat(name)
	if err != nil {
		return fmt.Errorf("recheck orphan snapshot file: %w", err)
	}
	if !expected.Mode().IsRegular() || !current.Mode().IsRegular() ||
		current.Mode().Perm() != expected.Mode().Perm() || current.Size() != expected.Size() ||
		!current.ModTime().Equal(expected.ModTime()) || !os.SameFile(expected, current) {
		return draftLockChangedError("orphan snapshot file changed before cleanup")
	}
	if err := directory.root.Remove(name); err != nil {
		return fmt.Errorf("remove orphan snapshot file: %w", err)
	}
	if _, err := directory.root.Lstat(name); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("verify orphan snapshot file cleanup: %w", err)
	}
	return draftLockChangedError("orphan snapshot file remained after cleanup")
}

func removeOrphanDraftClaims(ctx context.Context, storage *draftStorage, ref string) error {
	for _, suffix := range []string{".send-claim", ".send-spool", ".save-claim"} {
		if err := orphanDraftCleanupCheck(ctx, storage, ref); err != nil {
			return err
		}
		name := ref + suffix
		identity, err := storage.lstat(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect orphan draft artifact: %w", err)
		}
		if !identity.Mode().IsRegular() {
			return draftLockUnsafeError("orphan draft artifact is not a regular file")
		}
		if err := removeDraftStorageFile(storage, name, identity, ""); err != nil {
			return err
		}
	}
	return storage.apply(draftStorageSync, "", "", 0)
}
