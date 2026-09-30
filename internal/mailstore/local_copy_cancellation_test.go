package mailstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type cancelingLocalReader struct {
	reader *bytes.Reader
	cancel context.CancelFunc
	reads  int
	bytes  int
}

func (r *cancelingLocalReader) Read(p []byte) (int, error) {
	r.reads++
	n, err := r.reader.Read(p)
	r.bytes += n
	r.cancel()
	return n, err
}

func TestAttachmentCopyCancellationRemovesOnlyOwnedOutput(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &cancelingLocalReader{reader: bytes.NewReader(payload), cancel: cancel}
	output := filepath.Join(t.TempDir(), "attachment.bin")
	_, err := writeVerifiedExclusiveFileWithEvidence(ctx, output, reader, int64(len(payload)), sha256.Sum256(payload))
	if !errors.Is(err, context.Canceled) || reader.reads != 1 || reader.bytes <= 0 || reader.bytes > 32<<10 {
		t.Fatalf("copy err=%v reads=%d bytes=%d", err, reader.reads, reader.bytes)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("canceled output retained: %v", err)
	}
}

func TestAttachmentHashReaderStopsBetweenBoundedReads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &cancelingLocalReader{reader: bytes.NewReader(bytes.Repeat([]byte("x"), 1<<20)), cancel: cancel}
	n, err := io.Copy(sha256.New(), mimeContextReader{ctx: ctx, reader: reader})
	if !errors.Is(err, context.Canceled) || n != int64(reader.bytes) || reader.reads != 1 || n > 32<<10 {
		t.Fatalf("hash err=%v bytes=%d reads=%d", err, n, reader.reads)
	}
}

func TestExternalDiscoveryStopsBeforeOpeningCanceledPaths(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &Store{}
	if _, _, err := store.findExternalAttachment(ctx, resolvedMessage{}, attachmentRecord{ID: "2"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("discovery err=%v", err)
	}
	if _, err := store.hashStoreFile(ctx, externalAttachment{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("hash err=%v", err)
	}
}
