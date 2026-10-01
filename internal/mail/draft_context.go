package mail

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"mailcli/internal/transport"
)

const (
	draftLockPoll = 25 * time.Millisecond
	draftLockWait = 2 * time.Second
)

func draftContextError(ctx context.Context, operation string) error {
	cause := ctx.Err()
	if cause == nil {
		return nil
	}
	code, status := "draft_operation_canceled", "canceled"
	if errors.Is(cause, context.DeadlineExceeded) {
		code, status = "draft_operation_timeout", "timed out"
	}
	return &OperationError{
		Code:    code,
		Message: fmt.Sprintf("draft %s %s before completion", operation, status),
		Err:     cause,
	}
}

func draftLockContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, draftLockWait)
}

func classifyDraftContextError(ctx context.Context, err error, operation string) error {
	if err == nil || ctx.Err() == nil {
		return err
	}
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		if coded.ErrorCode() == "draft_busy" {
			return errors.Join(draftContextError(ctx, operation), err)
		}
		if coded.ErrorCode() != "" {
			return err
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(draftContextError(ctx, operation), err)
	}
	return err
}

// draftOperationBudget bounds a draft operation whose work scales with
// attachment bytes (fingerprinting, staging copies) using the same model as
// encoded message transfers: 30 seconds plus one second per MiB at a 1 MiB/s
// floor, capped at 15 minutes.
func draftOperationBudget(attachmentBytes int64) time.Duration {
	return transport.TransferBudgetForSize(attachmentBytes)
}

// draftAttachmentPathBytes sums the current sizes of attachment input paths.
// Paths that cannot be statted contribute zero; the fingerprint pass reports
// the authoritative error for them.
func draftAttachmentPathBytes(paths []string) int64 {
	var total int64
	for _, path := range paths {
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() && info.Size() > 0 {
			total += info.Size()
		}
	}
	return total
}
