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
	"fmt"
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
}

type nativeResponse struct {
	OK              bool   `json:"ok"`
	Code            string `json:"code,omitempty"`
	Message         string `json:"message,omitempty"`
	Opened          bool   `json:"opened,omitempty"`
	MailApplication string `json:"mail_application,omitempty"`
}

type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string {
	return e.Message
}

func (e *Error) ErrorCode() string {
	return e.Code
}

var invokeNativeCompose = nativeComposeEmail

func nativeComposeEmail(payload string) (string, error) {
	input := C.CString(payload)
	defer C.free(unsafe.Pointer(input))
	output := C.mailcli_compose_email(input)
	if output == nil {
		return "", fmt.Errorf("compose handoff returned no result")
	}
	defer C.free(unsafe.Pointer(output))
	return C.GoString(output), nil
}

func Handoff(ctx context.Context, request Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return Result{}, fmt.Errorf("encode compose handoff: %w", err)
	}
	raw, err := invokeNativeCompose(string(payload))
	if err != nil {
		return Result{}, err
	}
	return parseHandoffResponse(raw)
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
		return Result{}, &Error{Code: response.Code, Message: response.Message}
	}
	return Result{Opened: response.Opened, MailApplication: response.MailApplication}, nil
}
