package mailstore

import (
	"errors"
	"fmt"
)

type Error struct {
	Code    string
	Message string
	Err     error
}

func (e *Error) Error() string {
	return e.Message
}

func (e *Error) ErrorCode() string {
	return e.Code
}

func (e *Error) Unwrap() error {
	return e.Err
}

func operationError(code string, message string) error {
	return operationErrorWithCause(code, message, nil)
}

func operationErrorWithCause(code string, message string, cause error) error {
	return &Error{Code: code, Message: message, Err: cause}
}

type hydrationError struct {
	operation string
	local     error
	remote    error
}

func newHydrationError(operation string, local, remote error) error {
	if local == nil {
		return remote
	}
	if remote == nil {
		return local
	}
	return &hydrationError{operation: operation, local: local, remote: remote}
}

func (e *hydrationError) Error() string {
	return fmt.Sprintf(
		"%s failed: local source: %v; IMAP fallback: %v",
		e.operation, e.local, e.remote,
	)
}

func (e *hydrationError) Unwrap() []error {
	return []error{e.local, e.remote}
}

func (e *hydrationError) ErrorCode() string {
	if code := nestedErrorCode(e.remote); code != "" {
		return code
	}
	if code := nestedErrorCode(e.local); code != "" {
		return code
	}
	return "hydration_failed"
}

func nestedErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		return coded.ErrorCode()
	}
	return ""
}
