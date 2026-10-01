package mail

import (
	"fmt"
	"os"
)

var handoffAttachmentReadHook = func(int) {}

func openHandoffAttachment(path string) (*os.File, os.FileInfo, error) {
	file, info, err := openRegularAttachment(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, handoffAttachmentMissing(path, err)
		}
		return nil, nil, handoffAttachmentUnreadable(path, err)
	}
	return file, info, nil
}

func handoffAttachmentMissing(path string, err error) error {
	return &OperationError{
		Code:    "handoff_attachment_missing",
		Message: fmt.Sprintf("attachment %s is missing: %v", path, err),
	}
}

func handoffAttachmentUnreadable(path string, err error) error {
	return &OperationError{
		Code:    "handoff_attachment_unreadable",
		Message: fmt.Sprintf("attachment %s is not safely readable: %v", path, err),
	}
}

func handoffAttachmentChanged(path string, err error) error {
	return &OperationError{
		Code:    "handoff_attachment_changed",
		Message: fmt.Sprintf("attachment %s changed while it was staged: %v", path, err),
	}
}
