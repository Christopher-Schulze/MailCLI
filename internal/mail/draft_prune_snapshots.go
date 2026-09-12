package mail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	if !current.IsDir() || !os.SameFile(d.identity, current) {
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
	check := func() error { return orphanDraftCleanupCheck(ctx, storage, ref) }
	if err := check(); err != nil {
		return err
	}
	name := ref + handoffSnapshotSuffix
	info, err := storage.lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect orphan handoff snapshots: %w", err)
	}
	directory, err := openOrphanSnapshotDirectory(storage.root, name, info, check)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.root.Close()) }()
	if err := removeOrphanSnapshotDirectory(directory, orphanSnapshotParentDepth); err != nil {
		return err
	}
	return storage.apply(draftStorageSync, "", "", 0)
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
	if !expected.Mode().IsRegular() || !current.Mode().IsRegular() || !os.SameFile(expected, current) {
		return draftLockChangedError("orphan snapshot file changed before cleanup")
	}
	return directory.root.Remove(name)
}

func removeOrphanDraftClaims(ctx context.Context, storage *draftStorage, ref string) error {
	for _, suffix := range []string{".send-claim", ".send-spool", ".save-claim", handoffClaimSuffix} {
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
