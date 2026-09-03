package imapclient

import (
	"context"
	"errors"
	"os"
	"testing"

	"mailcli/internal/transport"
)

func TestPickTrash(t *testing.T) {
	t.Parallel()

	if got := pickTrash([]transport.MailboxInfo{
		{Name: "INBOX"},
		{Name: "Bin", Flags: []string{"\\Trash"}},
	}); got != "Bin" {
		t.Fatalf("flagged trash = %q, want Bin", got)
	}
	if got := pickTrash([]transport.MailboxInfo{
		{Name: "INBOX"},
		{Name: "[Gmail]/Trash"},
	}); got != "[Gmail]/Trash" {
		t.Fatalf("named trash = %q", got)
	}
	if got := pickTrash([]transport.MailboxInfo{{Name: "INBOX"}}); got != "" {
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
