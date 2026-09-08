//go:build darwin

package mail

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func openRegularAttachmentPath(path string) (*os.File, error) {
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("resolve attachment parent: %w", err)
	}
	fileDescriptor, err := unix.Open(
		filepath.Join(resolvedParent, filepath.Base(path)),
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW_ANY|unix.O_NONBLOCK,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open attachment descriptor: %w", err)
	}
	file := os.NewFile(uintptr(fileDescriptor), path)
	if file == nil {
		return nil, fmt.Errorf("adopt attachment descriptor: %w", unix.Close(fileDescriptor))
	}
	return file, nil
}
