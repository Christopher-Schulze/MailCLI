//go:build !darwin && !plan9 && !(js && wasm)

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type exclusiveOutputOther struct {
	file     *os.File
	root     *os.Root
	name     string
	identity os.FileInfo
}

func openExclusiveOutput(path string, mode os.FileMode) (exclusiveOutput, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("release output path must be absolute")
	}
	cleanPath := filepath.Clean(path)
	parentPath := filepath.Dir(cleanPath)
	name := filepath.Base(cleanPath)
	parentInfo, err := os.Lstat(parentPath)
	if err != nil {
		return nil, fmt.Errorf("inspect release output directory: %w", err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("release output directory must be a real directory")
	}
	root, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, fmt.Errorf("open release output directory: %w", err)
	}
	opened := &exclusiveOutputOther{root: root, name: name}
	openedParentInfo, err := root.Stat(".")
	if err != nil || !openedParentInfo.IsDir() || !os.SameFile(parentInfo, openedParentInfo) {
		_ = opened.CloseParent()
		return nil, fmt.Errorf("release output directory changed while opening")
	}
	opened.file, err = root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		_ = opened.CloseParent()
		return nil, fmt.Errorf("create release output: %w", err)
	}
	opened.identity, err = opened.file.Stat()
	if err != nil {
		_ = opened.CloseFile()
		_ = opened.CloseParent()
		return nil, fmt.Errorf("inspect created release output: %w", err)
	}
	pathInfo, err := root.Lstat(name)
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() ||
		!opened.identity.Mode().IsRegular() || !os.SameFile(opened.identity, pathInfo) {
		_ = opened.CloseFile()
		_ = opened.Cleanup()
		_ = opened.CloseParent()
		return nil, fmt.Errorf("release output changed while opening")
	}
	return opened, nil
}

func (output *exclusiveOutputOther) Write(payload []byte) (int, error) {
	return output.file.Write(payload)
}

func (output *exclusiveOutputOther) Sync() error {
	return output.file.Sync()
}

func (output *exclusiveOutputOther) Validate(expectedSize int64) error {
	opened, err := output.file.Stat()
	if err != nil {
		return fmt.Errorf("inspect release output descriptor: %w", err)
	}
	if !opened.Mode().IsRegular() || opened.Size() != expectedSize || !os.SameFile(output.identity, opened) {
		return fmt.Errorf("release output changed before close")
	}
	current, err := output.root.Lstat(output.name)
	if err != nil {
		return fmt.Errorf("inspect release output path: %w", err)
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() ||
		current.Size() != expectedSize || !os.SameFile(output.identity, current) {
		return fmt.Errorf("release output path changed before close")
	}
	return nil
}

func (output *exclusiveOutputOther) CloseFile() error {
	if output.file == nil {
		return nil
	}
	file := output.file
	output.file = nil
	return file.Close()
}

func (output *exclusiveOutputOther) Cleanup() error {
	if output.root == nil {
		return fmt.Errorf("release output parent is closed")
	}
	current, err := output.root.Lstat(output.name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspect failed release output: %w", err)
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(output.identity, current) {
		return fmt.Errorf("release output changed; refusing cleanup")
	}
	if err := output.root.Remove(output.name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove failed release output: %w", err)
	}
	return nil
}

func (output *exclusiveOutputOther) CloseParent() error {
	if output.root == nil {
		return nil
	}
	root := output.root
	output.root = nil
	return root.Close()
}
