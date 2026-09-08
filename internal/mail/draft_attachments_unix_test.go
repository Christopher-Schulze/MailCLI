//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package mail

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestFingerprintAttachmentRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attachment.pipe")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("Mkfifo() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := fingerprintAttachmentContext(ctx, path, MaximumDraftAttachmentBytes)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("fingerprintAttachmentContext() error = nil, want FIFO rejection")
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("FIFO rejection waited for context timeout: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("fingerprintAttachmentContext() blocked on FIFO")
	}
}

func TestOpenRegularAttachmentRejectsFIFOReplacement(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "attachment")
	if err := os.WriteFile(path, []byte("attachment"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("Mkfifo() error = %v", err)
	}

	_, _, err := openRegularAttachment(path)
	if err == nil {
		t.Fatal("openRegularAttachment() error = nil, want FIFO rejection")
	}
}
