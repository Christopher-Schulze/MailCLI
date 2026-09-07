//go:build !darwin && !plan9 && !(js && wasm)

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func openPrivateKeyFile(path string) (openedFile *os.File, openedInfo os.FileInfo, resultErr error) {
	if !filepath.IsAbs(path) {
		return nil, nil, fmt.Errorf("private key path must be absolute")
	}
	cleanPath := filepath.Clean(path)
	parentPath := filepath.Dir(cleanPath)
	name := filepath.Base(cleanPath)
	parentInfo, err := os.Lstat(parentPath)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect private key directory: %w", err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("private key directory must be a real directory")
	}
	pinnedRoot, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open private key directory: %w", err)
	}
	defer func() {
		closeErr := pinnedRoot.Close()
		if resultErr != nil {
			resultErr = errors.Join(resultErr, closeErr)
			if openedFile != nil {
				resultErr = errors.Join(resultErr, openedFile.Close())
				openedFile = nil
				openedInfo = nil
			}
			return
		}
		if closeErr != nil {
			resultErr = closeErr
			if openedFile != nil {
				resultErr = errors.Join(resultErr, openedFile.Close())
				openedFile = nil
				openedInfo = nil
			}
		}
	}()

	openedParentInfo, err := pinnedRoot.Stat(".")
	if err != nil {
		return nil, nil, fmt.Errorf("inspect opened private key directory: %w", err)
	}
	if !openedParentInfo.IsDir() || !os.SameFile(parentInfo, openedParentInfo) {
		return nil, nil, fmt.Errorf("private key directory changed while opening")
	}
	pathInfo, err := pinnedRoot.Lstat(name)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect private key path: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() || pathInfo.Mode().Perm()&0o077 != 0 {
		return nil, nil, fmt.Errorf("private key must be a regular file accessible only by its owner")
	}
	openedFile, err = pinnedRoot.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open private key: %w", err)
	}
	openedInfo, err = openedFile.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("inspect opened private key: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || openedInfo.Mode().Perm()&0o077 != 0 || !os.SameFile(pathInfo, openedInfo) {
		return nil, nil, fmt.Errorf("private key changed while opening")
	}
	currentPathInfo, err := pinnedRoot.Lstat(name)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect private key path: %w", err)
	}
	if currentPathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, currentPathInfo) {
		return nil, nil, fmt.Errorf("private key changed while opening")
	}
	currentParentInfo, err := os.Lstat(parentPath)
	if err != nil || currentParentInfo.Mode()&os.ModeSymlink != 0 ||
		!currentParentInfo.IsDir() || !os.SameFile(parentInfo, currentParentInfo) {
		return nil, nil, fmt.Errorf("private key directory changed while opening")
	}
	return openedFile, openedInfo, nil
}
