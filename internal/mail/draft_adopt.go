package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// AdoptStoreDraft copies a Mail.app store draft into a new local draft. The
// source message is only read — adoption is a copy, never a move — and the
// result is a normal local draft_* ref with the full update/send/discard
// lifecycle. Downloads are staged outside the swept root; verified attachments
// and JSON are published together under the new draft's exclusive ref lease.
func (s *Service) AdoptStoreDraft(ctx context.Context, ref string) (result Draft, resultErr error) {
	if strings.TrimSpace(ref) == "" {
		return Draft{}, validationError("draft message ref is required")
	}
	var draftRef string
	var stage *adoptionStaging
	publicationStarted := false
	defer func() {
		resultErr = finishDraftAdoption(stage, draftRef, publicationStarted, resultErr)
	}()
	if err := draftContextError(ctx, "adopt"); err != nil {
		return Draft{}, err
	}
	message, err := s.OpenDraft(ctx, ref)
	if err != nil {
		return Draft{}, err
	}
	if !message.ContentComplete {
		return Draft{}, &OperationError{
			Code:    "adopt_source_incomplete",
			Message: "the store draft is not fully materialized locally; open it in Mail.app first",
		}
	}
	if len(message.Attachments) > MaximumDraftAttachments {
		return Draft{}, validationError("draft exceeds 100 attachments")
	}
	// Reject a draft whose known attachment sizes already exceed the bound
	// before any bytes are copied; fingerprinting at create stays authoritative.
	var knownAttachmentBytes int64
	for _, attachment := range message.Attachments {
		if attachment.SizeKnown && attachment.Size > 0 {
			knownAttachmentBytes += attachment.Size
		}
	}
	if knownAttachmentBytes > MaximumDraftAttachmentBytes {
		return Draft{}, validationError("draft attachments exceed 512 MiB total")
	}
	root, err := s.resolveDraftRoot()
	if err != nil {
		return Draft{}, err
	}
	draftRef, err = newDraftReference()
	if err != nil {
		return Draft{}, err
	}
	rootIdentity, err := os.Lstat(root)
	if err != nil || !rootIdentity.IsDir() {
		return Draft{}, errors.Join(errors.New("inspect adoption draft directory"), err)
	}
	paths := make([]string, 0, len(message.Attachments))
	if len(message.Attachments) > 0 {
		stage, err = newAdoptionStaging(root, draftRef, rootIdentity)
		if err != nil {
			return Draft{}, err
		}
		for index, attachment := range message.Attachments {
			if err := ctx.Err(); err != nil {
				return Draft{}, draftContextError(ctx, "adopt")
			}
			if err := stage.verify(); err != nil {
				return Draft{}, err
			}
			name := adoptedAttachmentFileName(index, attachment.Name)
			destination := filepath.Join(stage.path, name)
			if err := s.gateway.SaveAttachmentTo(ctx, ref, attachment.ID, destination); err != nil {
				return Draft{}, err
			}
			if err := stage.verify(); err != nil {
				return Draft{}, err
			}
			identity, err := stage.root.Lstat(name)
			if err != nil || !adoptionPrivateFile(identity) {
				return Draft{}, errors.Join(errors.New("downloaded adoption attachment is not a private regular file"), err)
			}
			stage.files = append(stage.files, adoptionStagedFile{name: name, identity: identity})
			paths = append(paths, destination)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, draftOperationBudget(draftAttachmentPathBytes(paths)))
	defer cancel()
	draft, err := prepareDraftWithAttachmentsObserverContext(ctx, CreateDraftRequest{
		Kind:                 DraftKindNew,
		preassignedRef:       draftRef,
		allowEmptyRecipients: true,
		Input: DraftInput{
			From:        message.Summary.Sender,
			To:          message.To,
			CC:          message.CC,
			BCC:         message.BCC,
			Subject:     message.Summary.Subject,
			Body:        message.Content,
			BodyFormat:  DraftBodyPlain,
			Attachments: paths,
		},
	}, nil, s.contentObserver)
	if err != nil {
		return Draft{}, classifyDraftContextError(ctx, err, "adopt")
	}
	if err := draftContextError(ctx, "adopt"); err != nil {
		return Draft{}, err
	}
	lockContext, lockCancel := draftLockContext(ctx)
	defer lockCancel()
	lease, err := acquireDraftLease(lockContext, root, draftRef)
	if err != nil {
		return Draft{}, classifyDraftContextError(ctx, err, "adopt")
	}
	defer func() { resultErr = errors.Join(resultErr, lease.release()) }()
	if err := verifyAdoptionDirectory(lease.storage.root, ".", root, rootIdentity); err != nil {
		return Draft{}, err
	}
	if _, err := lease.storage.lstat(draftRef + ".json"); !os.IsNotExist(err) {
		return Draft{}, errors.Join(errors.New("adopted draft destination already exists"), err)
	}
	if stage != nil {
		if err := stage.verify(); err != nil {
			return Draft{}, err
		}
		if err := lease.storage.root.Mkdir(adoptedAttachmentDirName(draftRef), 0o700); err != nil {
			return Draft{}, err
		}
		publicationStarted = true
		if err := publishAdoptionAttachments(ctx, stage, lease.storage, &draft); err != nil {
			return Draft{}, err
		}
	}
	if err := verifyAdoptionDirectory(lease.storage.root, ".", root, rootIdentity); err != nil {
		return Draft{}, err
	}
	if err := refreshDraftRevision(&draft); err != nil {
		return Draft{}, err
	}
	publicationStarted = true
	if err := writeDraftFileContext(ctx, root, draft, lease.storage); err != nil {
		return Draft{}, err
	}
	return draft, nil
}

// DraftAdoptionError retains source-bound evidence for safe retry guidance.
// PublicationStarted is conservative: JSON rename may have succeeded even if
// its directory sync failed. StagingRetained identifies failed owned cleanup.
type DraftAdoptionError struct {
	Ref                string
	StagingPath        string
	PublicationStarted bool
	StagingRetained    bool
	Err                error
}

func (e *DraftAdoptionError) Error() string {
	if e.PublicationStarted || e.StagingRetained {
		return fmt.Sprintf("adopt %s failed (publication_started=%t, staging_retained=%t, staging=%s): %v; inspect retained artifacts before retrying", e.Ref, e.PublicationStarted, e.StagingRetained, e.StagingPath, e.Err)
	}
	if e.Ref == "" {
		return fmt.Sprintf("adopt store draft failed before publication: %v", e.Err)
	}
	return fmt.Sprintf("adopt %s failed before publication: %v", e.Ref, e.Err)
}

func (e *DraftAdoptionError) Unwrap() error { return e.Err }

func (e *DraftAdoptionError) ErrorCode() string {
	var coded interface{ ErrorCode() string }
	if errors.As(e.Err, &coded) {
		return coded.ErrorCode()
	}
	return "draft_adopt_failed"
}

func finishDraftAdoption(stage *adoptionStaging, ref string, publicationStarted bool, resultErr error) error {
	retained, path := false, ""
	if stage != nil {
		path = stage.path
		if resultErr == nil || !publicationStarted {
			cleanupErr := stage.cleanup()
			retained = cleanupErr != nil
			resultErr = errors.Join(resultErr, cleanupErr)
		} else {
			retained = stage.owned
		}
		resultErr = errors.Join(resultErr, stage.close())
	}
	if resultErr == nil {
		return nil
	}
	return &DraftAdoptionError{Ref: ref, StagingPath: path, PublicationStarted: publicationStarted, StagingRetained: retained, Err: resultErr}
}

type adoptionStagedFile struct {
	name     string
	identity os.FileInfo
}

type adoptionStaging struct {
	parent   *os.Root
	root     *os.Root
	path     string
	identity os.FileInfo
	files    []adoptionStagedFile
	owned    bool
}

func newAdoptionStaging(root, ref string, rootIdentity os.FileInfo) (*adoptionStaging, error) {
	parent, err := os.OpenRoot(filepath.Dir(root))
	if err != nil {
		return nil, err
	}
	stage := &adoptionStaging{parent: parent, path: filepath.Join(filepath.Dir(root), ".mailcli-adopt-"+ref)}
	name := filepath.Base(stage.path)
	if err := parent.Mkdir(name, 0o700); err != nil {
		return stage, err
	}
	stage.owned = true
	stage.identity, err = parent.Lstat(name)
	if err != nil {
		return stage, err
	}
	stage.root, err = parent.OpenRoot(name)
	if err != nil {
		return stage, err
	}
	if err := stage.verify(); err != nil {
		return stage, err
	}
	stageStat, stageOK := stage.identity.Sys().(*syscall.Stat_t)
	rootStat, rootOK := rootIdentity.Sys().(*syscall.Stat_t)
	if !stageOK || !rootOK || stageStat.Dev != rootStat.Dev {
		return stage, errors.New("adoption staging and draft directory are not on the same filesystem")
	}
	return stage, nil
}

func verifyAdoptionDirectory(root *os.Root, name, path string, expected os.FileInfo) error {
	current, err := root.Lstat(name)
	if err != nil || expected == nil || !current.IsDir() || current.Mode() != expected.Mode() || !os.SameFile(expected, current) {
		return errors.Join(errors.New("adoption directory identity changed"), err)
	}
	named, err := os.Lstat(path)
	if err != nil || !named.IsDir() || named.Mode() != expected.Mode() || !os.SameFile(expected, named) {
		return errors.Join(errors.New("adoption directory path changed"), err)
	}
	return nil
}

func (s *adoptionStaging) verify() error {
	if s.identity == nil || s.identity.Mode() != os.ModeDir|0o700 {
		return errors.New("adoption staging is not a private directory")
	}
	metadata, ok := s.identity.Sys().(*syscall.Stat_t)
	if !ok || metadata.Uid != uint32(os.Geteuid()) {
		return errors.New("adoption staging has a foreign owner")
	}
	if err := verifyAdoptionDirectory(s.parent, filepath.Base(s.path), s.path, s.identity); err != nil {
		return err
	}
	if s.root == nil {
		return errors.New("adoption staging descriptor is unavailable")
	}
	pinned, err := s.root.Stat(".")
	if err != nil || !os.SameFile(s.identity, pinned) {
		return errors.Join(errors.New("adoption staging descriptor changed"), err)
	}
	return nil
}

func adoptionPrivateFile(info os.FileInfo) bool {
	if info == nil || info.Mode() != 0o600 {
		return false
	}
	metadata, ok := info.Sys().(*syscall.Stat_t)
	return ok && metadata.Uid == uint32(os.Geteuid()) && metadata.Nlink == 1
}

func (s *adoptionStaging) cleanup() error {
	if !s.owned {
		return nil
	}
	if err := s.verify(); err != nil {
		return err
	}
	directory, err := s.root.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := directory.ReadDir(-1)
	if err := errors.Join(readErr, directory.Close()); err != nil {
		return err
	}
	if len(entries) != len(s.files) {
		return errors.New("adoption staging contains unverified entries; preserved")
	}
	for _, file := range s.files {
		current, err := s.root.Lstat(file.name)
		if err != nil || !adoptionPrivateFile(current) || !os.SameFile(file.identity, current) {
			return errors.Join(errors.New("adoption staging file changed before cleanup"), err)
		}
	}
	for _, file := range s.files {
		current, err := s.root.Lstat(file.name)
		if err != nil || !os.SameFile(file.identity, current) {
			return errors.Join(errors.New("adoption staging file changed during cleanup"), err)
		}
		if err := s.root.Remove(file.name); err != nil {
			return err
		}
	}
	if err := s.verify(); err != nil {
		return err
	}
	return s.parent.Remove(filepath.Base(s.path))
}

func (s *adoptionStaging) close() error {
	var result error
	if s.root != nil {
		result = s.root.Close()
	}
	return errors.Join(result, s.parent.Close())
}

func publishAdoptionAttachments(ctx context.Context, stage *adoptionStaging, state *draftStorage, draft *Draft) (resultErr error) {
	name := adoptedAttachmentDirName(draft.Ref)
	identity, err := state.lstat(name)
	if err != nil {
		return err
	}
	target, err := state.root.OpenRoot(name)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, target.Close()) }()
	pinned, err := target.Stat(".")
	if err != nil || !os.SameFile(identity, pinned) {
		return errors.Join(errors.New("adoption attachment directory changed while opening"), err)
	}
	for index, expected := range draft.Attachments {
		if err := stage.verify(); err != nil {
			return err
		}
		if err := verifyAdoptionDirectory(state.root, name, state.absolute(name), identity); err != nil {
			return err
		}
		published, err := copyAdoptionAttachment(ctx, stage, target, index, expected)
		if err != nil {
			return err
		}
		published.Path = filepath.Join(state.absolute(name), stage.files[index].name)
		draft.Attachments[index] = published
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := verifyAdoptionDirectory(state.root, name, state.absolute(name), identity); err != nil {
		return err
	}
	directory, err := target.Open(".")
	if err != nil {
		return err
	}
	if err := errors.Join(directory.Sync(), directory.Close()); err != nil {
		return err
	}
	return state.apply(draftStorageSync, "", "", 0)
}

func copyAdoptionAttachment(ctx context.Context, stage *adoptionStaging, target *os.Root, index int, expected DraftAttachment) (result DraftAttachment, resultErr error) {
	owned := stage.files[index]
	current, err := stage.root.Lstat(owned.name)
	if err != nil || !adoptionPrivateFile(current) || !os.SameFile(owned.identity, current) || current.Size() != expected.Size || current.ModTime().UnixNano() != expected.ModTimeNanos {
		return DraftAttachment{}, errors.Join(errors.New("adoption source attachment changed"), err)
	}
	source, err := stage.root.Open(owned.name)
	if err != nil {
		return DraftAttachment{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, source.Close()) }()
	pinned, err := source.Stat()
	if err != nil || !os.SameFile(current, pinned) {
		return DraftAttachment{}, errors.Join(errors.New("adoption source identity changed while opening"), err)
	}
	file, err := target.OpenFile(owned.name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return DraftAttachment{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	identity, err := file.Stat()
	if err != nil {
		return DraftAttachment{}, err
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(attachmentFingerprintReader{ctx: ctx, reader: source}, expected.Size+1))
	if err != nil {
		return DraftAttachment{}, err
	}
	if written != expected.Size || hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 {
		return DraftAttachment{}, errors.New("adoption source attachment fingerprint changed")
	}
	if err := file.Sync(); err != nil {
		return DraftAttachment{}, err
	}
	current, err = target.Lstat(owned.name)
	if err != nil || !adoptionPrivateFile(current) || !os.SameFile(identity, current) || current.Size() != written {
		return DraftAttachment{}, errors.Join(errors.New("published adoption attachment identity changed"), err)
	}
	sourceCurrent, err := stage.root.Lstat(owned.name)
	if err != nil || !os.SameFile(owned.identity, sourceCurrent) || sourceCurrent.Size() != expected.Size || sourceCurrent.ModTime().UnixNano() != expected.ModTimeNanos {
		return DraftAttachment{}, errors.Join(errors.New("adoption source changed while copying"), err)
	}
	return DraftAttachment{Size: written, SHA256: expected.SHA256, ModTimeNanos: current.ModTime().UnixNano()}, nil
}

func adoptedAttachmentDirName(ref string) string { return ref + ".attachments" }

func adoptedAttachmentFileName(index int, name string) string {
	base := filepath.Base(strings.TrimSpace(name))
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = "attachment"
	}
	return fmt.Sprintf("%d-%s", index, base)
}

// removeDraftAttachmentDir deletes the draft-owned adopted-attachment
// directory and the files inside it. Drafts created from caller-supplied
// paths never own those files; only the ref-derived directory is removed.
// Callers must have already removed or verified absent the draft's .json.
func removeDraftAttachmentDir(storage *draftStorage, ref string) error {
	name := adoptedAttachmentDirName(ref)
	info, err := storage.lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect adopted attachment directory: %w", err)
	}
	if !info.IsDir() {
		return draftLockUnsafeError("adopted attachment path is not a directory")
	}
	entries, err := draftStorageReadDir(storage, name)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			return draftLockUnsafeError("adopted attachment directory holds a non-regular entry")
		}
		if err := removeDraftStorageFile(storage, name+"/"+entry.Name(), nil, ""); err != nil {
			return err
		}
	}
	if err := removeDraftStorageFile(storage, name, nil, ""); err != nil {
		return fmt.Errorf("remove adopted attachment directory: %w", err)
	}
	if storage.root == nil {
		return syncDirectory(storage.rootName)
	}
	return storage.apply(draftStorageSync, "", "", 0)
}

func draftStorageReadDir(storage *draftStorage, name string) ([]os.DirEntry, error) {
	if storage.root == nil {
		entries, err := os.ReadDir(storage.absolute(name))
		if err != nil {
			return nil, fmt.Errorf("list adopted attachment directory: %w", err)
		}
		return entries, nil
	}
	directory, err := storage.root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open adopted attachment directory: %w", err)
	}
	defer func() { _ = directory.Close() }()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("list adopted attachment directory: %w", err)
	}
	return entries, nil
}
