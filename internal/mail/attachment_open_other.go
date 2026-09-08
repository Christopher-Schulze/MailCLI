//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package mail

import "os"

func openRegularAttachmentPath(path string) (*os.File, error) {
	return os.Open(path)
}
