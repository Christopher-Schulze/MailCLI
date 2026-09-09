package mail

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteExclusiveContentReturnsVerifiedProof(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "message.txt")
	payload := []byte("complete message body\n")
	result, err := WriteExclusiveContent(path, func(writer io.Writer) error {
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
	_, err := WriteExclusiveContent(path, func(io.Writer) error { return nil })
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
	_, err := WriteExclusiveContent(path, func(writer io.Writer) error {
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
	writer := &exportCountingWriter{writer: io.Discard, written: MaximumRawSourceBytes}
	_, err := writer.Write([]byte("overflow"))
	var operation *OperationError
	if err == nil || !errors.As(err, &operation) || operation.Code != "content_export_too_large" {
		t.Fatalf("Write() error = %v, want content_export_too_large", err)
	}
}
