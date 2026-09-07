package mail

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	draftLockPoll = 25 * time.Millisecond
	draftLockWait = 2 * time.Second
)

func draftContextError(ctx context.Context, operation string) error {
	if ctx.Err() == nil {
		return nil
	}
	return &OperationError{
		Code:    "draft_operation_canceled",
		Message: fmt.Sprintf("draft %s canceled before completion", operation),
	}
}

func draftLockContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, draftLockWait)
}

func classifyDraftContextError(ctx context.Context, err error, operation string) error {
	if err == nil || ctx.Err() == nil {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return draftContextError(ctx, operation)
	}
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		if coded.ErrorCode() == "draft_busy" {
			return draftContextError(ctx, operation)
		}
		if coded.ErrorCode() != "" {
			return err
		}
	}
	return err
}
