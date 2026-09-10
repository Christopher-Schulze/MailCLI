package compose

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework AppKit -framework Foundation
#include <stdlib.h>
#include "handoff.h"
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"unsafe"
)

type Request struct {
	Recipients  []string `json:"recipients"`
	Subject     string   `json:"subject"`
	PlainBody   string   `json:"plain_body"`
	HTMLBody    string   `json:"html_body,omitempty"`
	Attachments []string `json:"attachments"`
}

type Result struct {
	Opened          bool   `json:"opened"`
	MailApplication string `json:"mail_application,omitempty"`
	State           State  `json:"state"`
}

// State identifies the external boundary reached by a compose handoff.
// Completion means that the sharing-service delegate confirmed the request;
// it never means that Mail saved or sent the draft.
type State string

const (
	StateConfirmedCompletion    State = "confirmed_completion"
	StateConfirmedFailure       State = "confirmed_failure"
	StateCanceledBeforeDispatch State = "canceled_before_dispatch"
	StateOutcomeUnknown         State = "outcome_unknown"
)

type nativeResponse struct {
	OK              bool   `json:"ok"`
	Code            string `json:"code,omitempty"`
	Message         string `json:"message,omitempty"`
	Opened          bool   `json:"opened,omitempty"`
	MailApplication string `json:"mail_application,omitempty"`
	Dispatched      bool   `json:"dispatched,omitempty"`
}

type Error struct {
	Code       string
	Message    string
	State      State
	Dispatched bool
}

func (e *Error) Error() string {
	return e.Message
}

func (e *Error) ErrorCode() string {
	return e.Code
}

type nativeComposeInvoker func(string, <-chan struct{}) (string, error)

// DispatchObserver is called immediately before the native sharing-service
// invocation. The observer must persist its own lifecycle marker and return;
// it cannot cancel or close the external Mail compose window.
type DispatchObserver func() error

var invokeNativeCompose nativeComposeInvoker = nativeComposeEmail

func nativeComposeEmail(payload string, cancelRequested <-chan struct{}) (result string, resultErr error) {
	// json.Marshal escapes embedded NUL bytes as \u0000 before this C string
	// boundary, so request data cannot truncate the native payload.
	cancellationRoot, err := os.MkdirTemp("", "mailcli-compose-cancellation-")
	if err != nil {
		return "", fmt.Errorf("create compose cancellation directory: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, removeComposeCancellationRoot(cancellationRoot))
	}()
	cancellationPath := filepath.Join(cancellationRoot, "requested")
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	writeCancellationMarker := func() {
		marker, createErr := os.OpenFile(cancellationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr == nil {
			if closeErr := marker.Close(); closeErr != nil {
				return
			}
		} else if !errors.Is(createErr, os.ErrExist) {
			// The native boundary will still return its bounded outcome. The
			// marker is advisory and must never turn into a false success.
			return
		}
	}
	go func() {
		defer close(watchDone)
		select {
		case <-cancelRequested:
			writeCancellationMarker()
		case <-stopWatch:
		}
	}()
	select {
	case <-cancelRequested:
		writeCancellationMarker()
	default:
	}
	defer func() {
		close(stopWatch)
		<-watchDone
	}()
	input := C.CString(payload)
	defer C.free(unsafe.Pointer(input))
	cancelInput := C.CString(cancellationPath)
	defer C.free(unsafe.Pointer(cancelInput))
	output := C.mailcli_compose_email(input, cancelInput)
	if output == nil {
		return "", fmt.Errorf("compose handoff returned no result")
	}
	defer C.free(unsafe.Pointer(output))
	return C.GoString(output), nil
}

func Handoff(ctx context.Context, request Request) (Result, error) {
	return HandoffWithDispatch(ctx, request, nil)
}

// HandoffWithDispatch performs a visible compose handoff and reports the
// native-dispatch boundary to observer. A callback after dispatch can only
// suppress a late result and retain evidence; AppKit exposes no supported
// cancellation or compose-window handle.
func HandoffWithDispatch(ctx context.Context, request Request, observer DispatchObserver) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if request.Recipients == nil {
		request.Recipients = []string{}
	}
	if request.Attachments == nil {
		request.Attachments = []string{}
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return Result{}, fmt.Errorf("encode compose handoff: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	cancelRequested := make(chan struct{})
	callDone := make(chan struct{})
	var cancellationObserved atomic.Bool
	cancelWatcherDone := make(chan struct{})
	go func() {
		defer close(cancelWatcherDone)
		select {
		case <-ctx.Done():
			cancellationObserved.Store(true)
			close(cancelRequested)
		case <-callDone:
			if ctx.Err() != nil {
				cancellationObserved.Store(true)
				close(cancelRequested)
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		close(callDone)
		<-cancelWatcherDone
		return Result{State: StateCanceledBeforeDispatch}, errors.Join(err, canceledBeforeDispatchError())
	}
	if observer != nil {
		if err := observer(); err != nil {
			close(callDone)
			<-cancelWatcherDone
			return Result{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		close(callDone)
		<-cancelWatcherDone
		return Result{State: StateCanceledBeforeDispatch}, errors.Join(err, canceledBeforeDispatchError())
	}
	raw, invokeErr := invokeNativeCompose(string(payload), cancelRequested)
	close(callDone)
	<-cancelWatcherDone
	if raw != "" {
		result, parseErr := parseHandoffResponse(raw)
		if cancellationObserved.Load() {
			var handoffErr *Error
			if errors.As(parseErr, &handoffErr) && !handoffErr.Dispatched {
				return Result{State: StateCanceledBeforeDispatch}, errors.Join(ctx.Err(), parseErr, invokeErr)
			}
			return Result{State: StateOutcomeUnknown}, errors.Join(outcomeUnknownError(), parseErr, invokeErr)
		}
		if parseErr != nil && observer != nil {
			return Result{State: StateOutcomeUnknown}, errors.Join(parseErr, outcomeUnknownError(), invokeErr)
		}
		if parseErr != nil && invokeErr != nil {
			return result, errors.Join(parseErr, invokeErr)
		}
		if parseErr != nil {
			return result, parseErr
		}
		return result, invokeErr
	}
	if cancellationObserved.Load() {
		return Result{State: StateOutcomeUnknown}, outcomeUnknownError()
	}
	if invokeErr != nil {
		if observer != nil {
			return Result{State: StateOutcomeUnknown}, errors.Join(invokeErr, outcomeUnknownError())
		}
		return Result{}, invokeErr
	}
	return Result{}, fmt.Errorf("compose handoff returned an empty result")
}

// parseHandoffResponse decodes the JSON response from the native compose
// handoff into a typed Result or Error. It is extracted from Handoff so the
// parsing logic is unit-testable without invoking AppKit.
func parseHandoffResponse(raw string) (Result, error) {
	var response nativeResponse
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		return Result{}, fmt.Errorf("decode compose handoff: %w", err)
	}
	if !response.OK {
		state := StateConfirmedFailure
		switch response.Code {
		case "handoff_outcome_unknown":
			state = StateOutcomeUnknown
		case "handoff_canceled_before_dispatch":
			state = StateCanceledBeforeDispatch
		}
		return Result{State: state}, &Error{Code: response.Code, Message: response.Message, State: state, Dispatched: response.Dispatched}
	}
	if !response.Dispatched {
		return Result{State: StateConfirmedFailure}, &Error{
			Code: "handoff_response_invalid", Message: "native handoff reported success without a dispatch boundary",
			State: StateConfirmedFailure, Dispatched: false,
		}
	}
	return Result{Opened: response.Opened, MailApplication: response.MailApplication, State: StateConfirmedCompletion}, nil
}

func (e *Error) DispatchedToNative() bool {
	return e.Dispatched
}

func (e *Error) OutcomeUnknown() bool {
	return e.State == StateOutcomeUnknown || e.Code == "handoff_outcome_unknown"
}

func outcomeUnknownError() error {
	return &Error{
		Code:       "handoff_outcome_unknown",
		Message:    "compose handoff cancellation interrupted the wait; Mail.app may have opened the compose and no window-close operation is available",
		State:      StateOutcomeUnknown,
		Dispatched: true,
	}
}

func canceledBeforeDispatchError() error {
	return &Error{
		Code:       "handoff_canceled_before_dispatch",
		Message:    "compose handoff was canceled before native dispatch; no external compose was initiated",
		State:      StateCanceledBeforeDispatch,
		Dispatched: false,
	}
}

func removeComposeCancellationRoot(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove compose cancellation directory: %w", err)
	}
	return nil
}
