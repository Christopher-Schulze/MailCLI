package imapclient

import (
	"context"
	"errors"
	"net"
	"os"

	"mailcli/internal/transport"
)

func wrapDialError(ctx context.Context, err error) error {
	if ctx.Err() == context.Canceled {
		return &transport.TransportError{
			Code:    transport.CodeIMAPTimeout,
			Message: "IMAP connection canceled",
			Err:     err,
		}
	}
	if isTimeout(err) {
		return &transport.TransportError{
			Code:    transport.CodeIMAPTimeout,
			Message: "IMAP connection timed out",
			Err:     err,
		}
	}
	return &transport.TransportError{
		Code:    transport.CodeIMAPConnectFailed,
		Message: "IMAP connection failed",
		Err:     err,
	}
}

func wrapIOError(ctx context.Context, err error, code, message string) error {
	if err == nil {
		return nil
	}
	if ctx.Err() == context.Canceled {
		return &transport.TransportError{
			Code:    transport.CodeIMAPTimeout,
			Message: message,
			Err:     err,
		}
	}
	if isTimeout(err) {
		return &transport.TransportError{
			Code:    transport.CodeIMAPTimeout,
			Message: message,
			Err:     err,
		}
	}
	return &transport.TransportError{
		Code:    code,
		Message: message,
		Err:     err,
	}
}

func wrapCommandIOError(ctx context.Context, err error, message string) error {
	if err == nil {
		return nil
	}
	if transport.ErrorCode(err) != "" {
		return err
	}
	var malformed *malformedResponseError
	if errors.As(err, &malformed) {
		return &transport.TransportError{
			Code:    transport.CodeIMAPResponseMalformed,
			Message: message,
			Err:     err,
		}
	}
	code := transport.CodeIMAPDisconnected
	switch {
	case errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled):
		code = transport.CodeIMAPCanceled
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || isTimeout(err):
		code = transport.CodeIMAPTimeout
	}
	return &transport.TransportError{Code: code, Message: message, Err: err}
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}
