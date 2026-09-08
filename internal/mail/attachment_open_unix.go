//go:build dragonfly || freebsd || linux || netbsd || openbsd || solaris

package mail

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openRegularAttachmentPath(path string) (*os.File, error) {
	fileDescriptor, err := unix.Open(
		path,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
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
