package mail

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteExclusiveContentReturnsVerifiedProof(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "message.txt")
	payload := []byte("complete message body\n")
	result, err := WriteExclusiveContent(context.Background(), path, func(writer io.Writer) error {
		_, err := writer.Write(payload)
		return err
	})
	if err != nil {
		t.Fatalf("WriteExclusiveContent() error = %v", err)
	}
	digest := sha256.Sum256(payload)
	if result.Path != path || result.Size != int64(len(payload)) || result.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("result = %+v", result)
	}
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.Equal(actual, payload) {
		t.Fatalf("content = %q, want %q", actual, payload)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestWriteExclusiveContentRefusesExistingPath(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "existing.txt")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	_, err := WriteExclusiveContent(context.Background(), path, func(io.Writer) error { return nil })
	var validation *ValidationError
	if err == nil || !errors.As(err, &validation) {
		t.Fatalf("error = %v, want validation error", err)
	}
	actual, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile() error = %v", readErr)
	}
	if string(actual) != "original" {
		t.Fatalf("existing content changed to %q", actual)
	}
}

func TestWriteExclusiveContentRemovesOwnedPartialFileOnWriteFailure(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "partial.txt")
	wantErr := errors.New("write failed")
	_, err := WriteExclusiveContent(context.Background(), path, func(writer io.Writer) error {
		if _, writeErr := writer.Write([]byte("partial")); writeErr != nil {
			return writeErr
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("partial path stat error = %v, want not-exist", statErr)
	}
}

func TestExportCountingWriterRejectsBytesAboveLimit(t *testing.T) {
	writer := &exportCountingWriter{ctx: context.Background(), writer: io.Discard, written: MaximumRawSourceBytes}
	_, err := writer.Write([]byte("overflow"))
	var operation *OperationError
	if err == nil || !errors.As(err, &operation) || operation.Code != "content_export_too_large" {
		t.Fatalf("Write() error = %v, want content_export_too_large", err)
	}
}

func TestContentExportCancellationRemovesOnlyOwnedFile(t *testing.T) {
	for _, beforeWrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("before_write=%t", beforeWrite), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := filepath.Join(t.TempDir(), "canceled.txt")
			if beforeWrite {
				cancel()
			}
			called := false
			_, err := WriteExclusiveContent(ctx, path, func(writer io.Writer) error {
				called = true
				if _, err := writer.Write([]byte("first chunk")); err != nil {
					return err
				}
				cancel()
				_, err := writer.Write([]byte("must not be written"))
				return err
			})
			if !errors.Is(err, context.Canceled) || called == beforeWrite {
				t.Fatalf("error=%v callback=%t", err, called)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("canceled export remains: %v", err)
			}
		})
	}
}

func TestContentExportChangedParentPreservesUnknownEffect(t *testing.T) {
	base := t.TempDir()
	parent, moved := filepath.Join(base, "parent"), filepath.Join(base, "moved")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "export.txt")
	_, err := WriteExclusiveContent(context.Background(), path, func(writer io.Writer) error {
		if _, err := writer.Write([]byte("owned")); err != nil {
			return err
		}
		if err := os.Rename(parent, moved); err != nil {
			return err
		}
		if err := os.Mkdir(parent, 0700); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("replacement"), 0600)
	})
	var outcome *ContentExportOutcomeError
	if !errors.As(err, &outcome) || outcome.EffectCertainty != EffectUnknown {
		t.Fatalf("outcome=%+v error=%v", outcome, err)
	}
	guidance := GuidanceForError("messages.raw", err)
	if guidance.EffectCertainty != EffectUnknown || guidance.ReplayAllowed || guidance.Retryability != RetryObserveRequired {
		t.Fatalf("guidance=%+v", guidance)
	}
	for _, file := range []struct{ path, want string }{{path, "replacement"}, {filepath.Join(moved, "export.txt"), "owned"}} {
		actual, err := os.ReadFile(file.path)
		if err != nil || string(actual) != file.want {
			t.Fatalf("preserved file %s=%q error=%v", file.path, actual, err)
		}
	}
}

type cancelDuringExportHash struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *cancelDuringExportHash) Err() error {
	c.checks++
	if c.checks == 3 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestExportDigestObservesCancellationBetweenRealFileReads(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "hash.txt")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 128<<10), 0600); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := &cancelDuringExportHash{Context: ctx, cancel: cancel}
	size, digest, err := verifyExportBytes(observed, root, "hash.txt", identity)
	if !errors.Is(err, context.Canceled) || size != 0 || digest != "" || observed.checks < 3 {
		t.Fatalf("size=%d digest=%q checks=%d error=%v", size, digest, observed.checks, err)
	}
}
