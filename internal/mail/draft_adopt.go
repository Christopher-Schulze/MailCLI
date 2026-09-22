package mail

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AdoptStoreDraft copies a Mail.app store draft into a new local draft. The
// source message is only read — adoption is a copy, never a move — and the
// result is a normal local draft_* ref with the full update/send/discard
// lifecycle. Attachment bytes are saved into a draft-owned directory inside
// the draft root because a local draft pins attachments by absolute path and
// re-fingerprints them before every send; caller-managed paths cannot serve
// that role for store bytes.
func (s *Service) AdoptStoreDraft(ctx context.Context, ref string) (Draft, error) {
	if strings.TrimSpace(ref) == "" {
		return Draft{}, validationError("draft message ref is required")
	}
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
	draftRef, err := newDraftReference()
	if err != nil {
		return Draft{}, err
	}
	paths := make([]string, 0, len(message.Attachments))
	attachmentsWritten := false
	if len(message.Attachments) > 0 {
		directory := filepath.Join(root, adoptedAttachmentDirName(draftRef))
		if err := os.Mkdir(directory, 0o700); err != nil {
			return Draft{}, fmt.Errorf("create adopted attachment directory: %w", err)
		}
		defer func() {
			if !attachmentsWritten {
				_ = os.RemoveAll(directory)
			}
		}()
		for index, attachment := range message.Attachments {
			if err := ctx.Err(); err != nil {
				return Draft{}, draftContextError(ctx, "adopt")
			}
			destination := filepath.Join(directory, adoptedAttachmentFileName(index, attachment.Name))
			if err := s.gateway.SaveAttachmentTo(ctx, ref, attachment.ID, destination); err != nil {
				return Draft{}, err
			}
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
	if err := refreshDraftRevision(&draft); err != nil {
		return Draft{}, err
	}
	if err := writeDraftFile(root, draft); err != nil {
		return Draft{}, err
	}
	attachmentsWritten = true
	return draft, nil
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
