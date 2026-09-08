package mail

import (
	"errors"
	"fmt"
	"os"
)

var errAttachmentNotRegular = errors.New("attachment is not a regular file")

// openRegularAttachment pins a regular-file descriptor to the path identity
// observed before opening. Callers must still recheck the path after reading
// because a replacement can occur while the descriptor is open.
func openRegularAttachment(path string) (*os.File, os.FileInfo, error) {
	initialInfo, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !initialInfo.Mode().IsRegular() {
		return nil, nil, errAttachmentNotRegular
	}
	file, err := openRegularAttachmentPath(path)
	if err != nil {
		return nil, nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("stat opened attachment: %w", err), file.Close())
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(initialInfo, openedInfo) {
		return nil, nil, errors.Join(
			errors.New("attachment path changed while it was opened"),
			file.Close(),
		)
	}
	return file, openedInfo, nil
}
