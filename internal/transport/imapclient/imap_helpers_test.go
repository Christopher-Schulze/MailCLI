package imapclient

import (
	"bufio"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"mailcli/internal/transport"
)

func TestPickTrash(t *testing.T) {
	t.Parallel()

	if got := transport.PickTrashMailbox([]transport.MailboxInfo{
		{Name: "INBOX"},
		{Name: "Bin", Flags: []string{"\\Trash"}},
	}); got != "Bin" {
		t.Fatalf("flagged trash = %q, want Bin", got)
	}
	if got := transport.PickTrashMailbox([]transport.MailboxInfo{
		{Name: "INBOX"},
		{Name: "[Gmail]/Trash"},
	}); got != "[Gmail]/Trash" {
		t.Fatalf("named trash = %q", got)
	}
	if got := transport.PickTrashMailbox([]transport.MailboxInfo{{Name: "INBOX"}}); got != "" {
		t.Fatalf("missing trash = %q", got)
	}
}

func TestIsTimeout(t *testing.T) {
	t.Parallel()

	if !isTimeout(context.DeadlineExceeded) {
		t.Fatal("DeadlineExceeded should be timeout")
	}
	if !isTimeout(os.ErrDeadlineExceeded) {
		t.Fatal("os.ErrDeadlineExceeded should be timeout")
	}
	if isTimeout(errors.New("plain")) {
		t.Fatal("plain error should not be timeout")
	}
}

func TestWrapIOError(t *testing.T) {
	t.Parallel()

	if err := wrapIOError(context.Background(), nil, transport.CodeIMAPConnectFailed, "x"); err != nil {
		t.Fatalf("nil err wrapped = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := wrapIOError(ctx, errors.New("closed"), transport.CodeIMAPConnectFailed, "read")
	if transport.ErrorCode(err) != transport.CodeIMAPTimeout {
		t.Fatalf("canceled wrap code = %q", transport.ErrorCode(err))
	}
	err = wrapIOError(context.Background(), context.DeadlineExceeded, transport.CodeIMAPConnectFailed, "read")
	if transport.ErrorCode(err) != transport.CodeIMAPTimeout {
		t.Fatalf("deadline wrap code = %q", transport.ErrorCode(err))
	}
	err = wrapIOError(context.Background(), errors.New("boom"), transport.CodeIMAPConnectFailed, "read")
	if transport.ErrorCode(err) != transport.CodeIMAPConnectFailed {
		t.Fatalf("plain wrap code = %q", transport.ErrorCode(err))
	}
}

func TestReadLineRejectsOversizedResponseWithTypedCause(t *testing.T) {
	t.Parallel()

	sess := &session{
		br: bufio.NewReader(strings.NewReader(
			strings.Repeat("x", maxIMAPResponseLineBytes+1) + "\r\n",
		)),
	}
	_, err := (&Client{}).readLine(sess)
	var malformed *malformedResponseError
	if !errors.As(err, &malformed) {
		t.Fatalf("readLine() error = %v, want malformedResponseError", err)
	}
	if !strings.Contains(err.Error(), "malformed IMAP response") ||
		!strings.Contains(err.Error(), "exceeds") || errors.Unwrap(malformed) == nil {
		t.Fatalf("readLine() error = %v, want malformed response with preserved cause", err)
	}
	if !sess.dirty {
		t.Fatal("readLine() left oversized-response session reusable")
	}
}

func TestWrapDialErrorClassifiesAndPreservesCause(t *testing.T) {
	t.Parallel()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cause := errors.New("dial failed")
	tests := []struct {
		name     string
		ctx      context.Context
		err      error
		wantCode string
	}{
		{name: "canceled", ctx: canceled, err: cause, wantCode: transport.CodeIMAPTimeout},
		{name: "timeout", ctx: context.Background(), err: context.DeadlineExceeded, wantCode: transport.CodeIMAPTimeout},
		{name: "connect", ctx: context.Background(), err: cause, wantCode: transport.CodeIMAPConnectFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := wrapDialError(test.ctx, test.err)
			if transport.ErrorCode(err) != test.wantCode {
				t.Fatalf("wrapDialError() code = %q, want %q", transport.ErrorCode(err), test.wantCode)
			}
			if !errors.Is(err, test.err) {
				t.Fatalf("wrapDialError() error = %v, want cause %v", err, test.err)
			}
		})
	}
}
