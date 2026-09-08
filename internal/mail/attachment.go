package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

var (
	attachmentLink = func(root *os.Root, oldName string, newName string) error {
		return root.Link(oldName, newName)
	}
	attachmentChmod = func(file *os.File, mode os.FileMode) error {
		return file.Chmod(mode)
	}
	attachmentRemove = func(root *os.Root, name string) error {
		return root.Remove(name)
	}
	attachmentClose = func(file *os.File) error {
		return file.Close()
	}
	attachmentPublicationHook = func(string, string) error { return nil }
)

type attachmentPublication struct {
	root              *os.Root
	parentPath        string
	parentIdentity    os.FileInfo
	temporaryPath     string
	temporaryName     string
	temporaryIdentity os.FileInfo
	temporary         *os.File
	outputPath        string
	outputName        string
	outputIdentity    os.FileInfo
	output            *os.File
	outputOwned       bool
	retainOutput      bool
}

func (s *Service) SaveAttachment(
	ctx context.Context,
	request SaveAttachmentRequest,
) (saved SavedAttachment, resultErr error) {
	if err := validateAttachmentRequest(request); err != nil {
		return SavedAttachment{}, err
	}
	temporaryPath, err := attachmentTemporaryPath(request.OutputPath)
	if err != nil {
		return SavedAttachment{}, err
	}
	publication, err := openAttachmentPublication(temporaryPath, request.OutputPath)
	if err != nil {
		return SavedAttachment{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, publication.cleanup())
		resultErr = errors.Join(resultErr, publication.close())
	}()
	if err := s.gateway.SaveAttachmentTo(ctx, request.MessageRef, request.AttachmentID, temporaryPath); err != nil {
		return SavedAttachment{}, err
	}
	if err := publication.captureTemporary(); err != nil {
		return SavedAttachment{}, err
	}
	if err := publication.publish(); err != nil {
		return SavedAttachment{}, err
	}
	saved, err = publication.inspect(request.AttachmentID)
	if err != nil {
		return SavedAttachment{}, err
	}
	publication.retainOutput = true
	return saved, nil
}

func validateAttachmentRequest(request SaveAttachmentRequest) error {
	if request.MessageRef == "" || request.AttachmentID == "" || request.OutputPath == "" {
		return validationError("message ref, attachment id, and output path are required")
	}
	if !filepath.IsAbs(request.OutputPath) {
		return validationError("attachment output path must be absolute")
	}
	if _, err := os.Lstat(request.OutputPath); err == nil {
		return validationError("attachment output path already exists")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect attachment output path: %w", err)
	}
	info, err := os.Lstat(filepath.Dir(request.OutputPath))
	if err != nil {
		return fmt.Errorf("inspect attachment output directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return validationError("attachment output parent is not a directory")
	}
	return nil
}

func attachmentTemporaryPath(outputPath string) (string, error) {
	var suffix [12]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generate attachment temporary path: %w", err)
	}
	return filepath.Join(
		filepath.Dir(outputPath),
		"."+filepath.Base(outputPath)+".mailcli-"+hex.EncodeToString(suffix[:]),
	), nil
}

func openAttachmentPublication(temporaryPath string, outputPath string) (*attachmentPublication, error) {
	parentPath := filepath.Dir(outputPath)
	parentIdentity, err := os.Lstat(parentPath)
	if err != nil {
		return nil, fmt.Errorf("inspect attachment output directory: %w", err)
	}
	if !parentIdentity.IsDir() || parentIdentity.Mode()&os.ModeSymlink != 0 {
		return nil, validationError("attachment output parent is not a directory")
	}
	root, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, fmt.Errorf("open attachment output directory: %w", err)
	}
	publication := &attachmentPublication{
		root:           root,
		parentPath:     parentPath,
		parentIdentity: parentIdentity,
		temporaryPath:  temporaryPath,
		temporaryName:  filepath.Base(temporaryPath),
		outputPath:     outputPath,
		outputName:     filepath.Base(outputPath),
	}
	if err := publication.verifyParent(); err != nil {
		return nil, errors.Join(err, publication.close())
	}
	if _, err := root.Lstat(publication.temporaryName); err == nil {
		return nil, errors.Join(
			validationError("attachment temporary path already exists"),
			publication.close(),
		)
	} else if !os.IsNotExist(err) {
		return nil, errors.Join(fmt.Errorf("inspect attachment temporary path: %w", err), publication.close())
	}
	return publication, nil
}

func (p *attachmentPublication) verifyParent() error {
	if p.root == nil {
		return attachmentChangedError("attachment output directory is closed")
	}
	current, err := os.Lstat(p.parentPath)
	if err != nil {
		return attachmentChangedError(fmt.Sprintf("attachment output directory changed: %v", err))
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(p.parentIdentity, current) {
		return attachmentChangedError("attachment output directory changed")
	}
	opened, err := p.root.Stat(".")
	if err != nil {
		return fmt.Errorf("inspect pinned attachment output directory: %w", err)
	}
	if !opened.IsDir() || !os.SameFile(p.parentIdentity, opened) {
		return attachmentChangedError("pinned attachment output directory changed")
	}
	return nil
}

func (p *attachmentPublication) captureTemporary() error {
	if err := p.verifyParent(); err != nil {
		return err
	}
	pathInfo, err := p.root.Lstat(p.temporaryName)
	if err != nil {
		return fmt.Errorf("inspect saved attachment: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return attachmentChangedError("temporary attachment is not a regular file")
	}
	file, err := p.root.Open(p.temporaryName)
	if err != nil {
		return fmt.Errorf("open saved attachment: %w", err)
	}
	identity, err := file.Stat()
	if err != nil {
		return errors.Join(fmt.Errorf("inspect saved attachment: %w", err), attachmentClose(file))
	}
	current, err := p.root.Lstat(p.temporaryName)
	if err != nil || current.Mode()&os.ModeSymlink != 0 ||
		!current.Mode().IsRegular() || !identity.Mode().IsRegular() ||
		!os.SameFile(pathInfo, identity) || !os.SameFile(identity, current) {
		closeErr := attachmentClose(file)
		return errors.Join(attachmentChangedError("temporary attachment changed while opening"), closeErr)
	}
	p.temporary = file
	p.temporaryIdentity = identity
	return nil
}

func (p *attachmentPublication) verifyTemporary() error {
	if p.temporary == nil || p.temporaryIdentity == nil {
		return attachmentChangedError("temporary attachment ownership is unavailable")
	}
	if err := p.verifyParent(); err != nil {
		return err
	}
	pathInfo, err := p.root.Lstat(p.temporaryName)
	if err != nil {
		if os.IsNotExist(err) {
			return attachmentChangedError("temporary attachment disappeared")
		}
		return fmt.Errorf("inspect temporary attachment: %w", err)
	}
	identity, err := p.temporary.Stat()
	if err != nil {
		return fmt.Errorf("inspect temporary attachment descriptor: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() ||
		!identity.Mode().IsRegular() || !os.SameFile(p.temporaryIdentity, pathInfo) ||
		!os.SameFile(p.temporaryIdentity, identity) {
		return attachmentChangedError("temporary attachment changed")
	}
	return nil
}

func (p *attachmentPublication) publish() error {
	if err := p.verifyTemporary(); err != nil {
		return err
	}
	if err := attachmentLink(p.root, p.temporaryName, p.outputName); err != nil {
		if !errors.Is(err, syscall.EXDEV) {
			return fmt.Errorf("publish attachment without overwrite: %w", err)
		}
		return p.copyAcrossFilesystem()
	}
	p.outputOwned = true
	if err := p.openPublished(); err != nil {
		return err
	}
	if err := attachmentPublicationHook("after-link", p.outputPath); err != nil {
		return err
	}
	if err := attachmentPublicationHook("after-link-temporary", p.temporaryPath); err != nil {
		return err
	}
	if err := p.restrictPublished(); err != nil {
		return err
	}
	return p.verifyPublishedAndTemporary()
}

func (p *attachmentPublication) openPublished() error {
	pathInfo, err := p.root.Lstat(p.outputName)
	if err != nil {
		return fmt.Errorf("inspect published attachment: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() ||
		!os.SameFile(p.temporaryIdentity, pathInfo) {
		p.outputOwned = false
		return attachmentChangedError("published attachment is not the linked temporary file")
	}
	p.outputIdentity = pathInfo
	file, err := p.root.Open(p.outputName)
	if err != nil {
		return fmt.Errorf("open published attachment: %w", err)
	}
	identity, err := file.Stat()
	if err != nil {
		return errors.Join(fmt.Errorf("inspect published attachment: %w", err), attachmentClose(file))
	}
	if !identity.Mode().IsRegular() || !os.SameFile(p.outputIdentity, identity) {
		closeErr := attachmentClose(file)
		p.outputOwned = false
		return errors.Join(attachmentChangedError("published attachment changed while opening"), closeErr)
	}
	p.output = file
	p.outputIdentity = identity
	return nil
}

func (p *attachmentPublication) restrictPublished() error {
	if err := p.verifyPublished(); err != nil {
		p.disownOutputOnChange(err)
		return err
	}
	if err := attachmentPublicationHook("before-chmod", p.outputPath); err != nil {
		return err
	}
	if err := attachmentChmod(p.output, 0o600); err != nil {
		return fmt.Errorf("restrict saved attachment: %w", err)
	}
	if err := attachmentPublicationHook("after-chmod", p.outputPath); err != nil {
		return err
	}
	if err := p.verifyPublishedMode(); err != nil {
		p.disownOutputOnChange(err)
		return err
	}
	return nil
}

func (p *attachmentPublication) copyAcrossFilesystem() error {
	if err := p.verifyTemporary(); err != nil {
		return err
	}
	if err := p.createPublishedCopy(); err != nil {
		return err
	}
	if err := p.copyTemporaryBytes(); err != nil {
		return err
	}
	return p.restrictPublished()
}

func (p *attachmentPublication) createPublishedCopy() error {
	file, err := p.root.OpenFile(p.outputName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create published attachment copy: %w", err)
	}
	identity, err := file.Stat()
	if err != nil {
		return errors.Join(fmt.Errorf("inspect published attachment copy: %w", err), attachmentClose(file))
	}
	if !identity.Mode().IsRegular() {
		return errors.Join(
			fmt.Errorf("published attachment copy is not a regular file"),
			attachmentClose(file),
		)
	}
	p.output = file
	p.outputIdentity = identity
	p.outputOwned = true
	if err := p.verifyPublished(); err != nil {
		p.disownOutputOnChange(err)
		return err
	}
	return nil
}

func (p *attachmentPublication) copyTemporaryBytes() error {
	if _, err := p.temporary.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind temporary attachment: %w", err)
	}
	sourceHash := sha256.New()
	written, err := io.Copy(p.output, io.TeeReader(p.temporary, sourceHash))
	if err != nil {
		return fmt.Errorf("copy attachment across filesystems: %w", err)
	}
	if written != p.temporaryIdentity.Size() {
		return attachmentChangedError("temporary attachment changed while copying")
	}
	if err := p.output.Sync(); err != nil {
		return fmt.Errorf("sync published attachment copy: %w", err)
	}
	if err := p.verifyTemporary(); err != nil {
		return err
	}
	if _, err := p.output.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind published attachment copy: %w", err)
	}
	outputHash := sha256.New()
	copied, err := io.Copy(outputHash, p.output)
	if err != nil {
		return fmt.Errorf("verify published attachment copy: %w", err)
	}
	if copied != written || !bytes.Equal(sourceHash.Sum(nil), outputHash.Sum(nil)) {
		return attachmentChangedError("attachment bytes changed while copying")
	}
	return nil
}

func (p *attachmentPublication) verifyPublishedAndTemporary() error {
	if err := p.verifyPublished(); err != nil {
		p.disownOutputOnChange(err)
		return err
	}
	if err := p.verifyTemporary(); err != nil {
		return err
	}
	return nil
}

func (p *attachmentPublication) verifyPublished() error {
	if p.output == nil || p.outputIdentity == nil {
		return attachmentChangedError("published attachment ownership is unavailable")
	}
	if err := p.verifyParent(); err != nil {
		return err
	}
	pathInfo, err := p.root.Lstat(p.outputName)
	if err != nil {
		if os.IsNotExist(err) {
			return attachmentChangedError("published attachment disappeared")
		}
		return fmt.Errorf("inspect published attachment path: %w", err)
	}
	identity, err := p.output.Stat()
	if err != nil {
		return fmt.Errorf("inspect published attachment descriptor: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() ||
		!identity.Mode().IsRegular() || !os.SameFile(p.outputIdentity, pathInfo) ||
		!os.SameFile(p.outputIdentity, identity) {
		return attachmentChangedError("published attachment changed")
	}
	return nil
}

func (p *attachmentPublication) verifyPublishedMode() error {
	if err := p.verifyPublished(); err != nil {
		return err
	}
	identity, err := p.output.Stat()
	if err != nil {
		return fmt.Errorf("inspect saved attachment permissions: %w", err)
	}
	if identity.Mode().Perm() != 0o600 {
		return fmt.Errorf("saved attachment permissions are not private")
	}
	return nil
}

func (p *attachmentPublication) inspect(attachmentID string) (SavedAttachment, error) {
	if err := p.verifyPublishedMode(); err != nil {
		p.disownOutputOnChange(err)
		return SavedAttachment{}, err
	}
	if err := attachmentPublicationHook("before-inspect", p.outputPath); err != nil {
		return SavedAttachment{}, err
	}
	if _, err := p.output.Seek(0, io.SeekStart); err != nil {
		return SavedAttachment{}, fmt.Errorf("rewind saved attachment: %w", err)
	}
	hash := sha256.New()
	size, err := io.Copy(hash, p.output)
	if err != nil {
		return SavedAttachment{}, fmt.Errorf("hash saved attachment: %w", err)
	}
	if err := attachmentPublicationHook("after-inspect", p.outputPath); err != nil {
		return SavedAttachment{}, err
	}
	identity, err := p.output.Stat()
	if err != nil {
		return SavedAttachment{}, fmt.Errorf("inspect hashed attachment: %w", err)
	}
	if identity.Size() != size || !os.SameFile(p.outputIdentity, identity) {
		err := attachmentChangedError("published attachment changed while hashing")
		p.disownOutputOnChange(err)
		return SavedAttachment{}, err
	}
	if err := p.verifyPublishedMode(); err != nil {
		p.disownOutputOnChange(err)
		return SavedAttachment{}, err
	}
	return SavedAttachment{
		AttachmentID: attachmentID,
		Path:         p.outputPath,
		Size:         size,
		SHA256:       hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

func (p *attachmentPublication) disownOutputOnChange(err error) {
	var changed *OperationError
	if errors.As(err, &changed) && changed.Code == "attachment_changed" {
		p.outputOwned = false
	}
}

func (p *attachmentPublication) cleanup() error {
	var resultErr error
	if p.outputOwned && p.outputIdentity != nil {
		if err := attachmentPublicationHook("before-cleanup", p.outputPath); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
		var outputErr error
		if p.retainOutput {
			outputErr = p.verifyPublishedMode()
		} else {
			outputErr = p.removeOwned(p.outputName, p.outputIdentity, "published attachment")
		}
		if outputErr != nil {
			if p.retainOutput {
				p.disownOutputOnChange(outputErr)
			}
			resultErr = errors.Join(resultErr, outputErr)
		}
	}
	if p.temporary != nil && p.temporaryIdentity != nil {
		if err := attachmentPublicationHook("before-cleanup", p.temporaryPath); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
		if err := p.removeOwned(p.temporaryName, p.temporaryIdentity, "temporary attachment"); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}
	return resultErr
}

func (p *attachmentPublication) removeOwned(name string, identity os.FileInfo, label string) error {
	if err := p.verifyParent(); err != nil {
		return err
	}
	current, err := p.root.Lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s for cleanup: %w", label, err)
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() ||
		!os.SameFile(identity, current) {
		return attachmentChangedError(fmt.Sprintf("%s changed; refusing cleanup", label))
	}
	if err := attachmentRemove(p.root, name); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", label, err)
	}
	return nil
}

func (p *attachmentPublication) close() error {
	var resultErr error
	if p.output != nil {
		resultErr = errors.Join(resultErr, attachmentClose(p.output))
		p.output = nil
	}
	if p.temporary != nil {
		resultErr = errors.Join(resultErr, attachmentClose(p.temporary))
		p.temporary = nil
	}
	if p.root != nil {
		resultErr = errors.Join(resultErr, p.root.Close())
		p.root = nil
	}
	return resultErr
}

func attachmentChangedError(message string) error {
	return &OperationError{Code: "attachment_changed", Message: message}
}

func removeIfPresent(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
