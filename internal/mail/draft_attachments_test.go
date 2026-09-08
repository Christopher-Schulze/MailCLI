package mail

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFingerprintAttachmentBoundsGrowingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "growing-attachment.txt")
	initial := "base"
	maximumSize := int64(8)
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	readBytes := 0
	grown := false
	var appendErr error
	attachment, err := fingerprintAttachmentContextWithObserver(
		context.Background(), path, maximumSize, func(read int) {
			readBytes += read
			if grown || appendErr != nil {
				return
			}
			grown = true
			file, openErr := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if openErr != nil {
				appendErr = openErr
				return
			}
			_, appendErr = file.WriteString(strings.Repeat("g", int(maximumSize)))
			if closeErr := file.Close(); appendErr == nil {
				appendErr = closeErr
			}
		},
	)
	if appendErr != nil {
		t.Fatalf("grow attachment = %v", appendErr)
	}
	if err == nil {
		t.Fatal("fingerprintAttachmentContextWithObserver() error = nil, want growth rejection")
	}
	if readBytes > int(maximumSize)+1 {
		t.Fatalf("fingerprint read %d bytes, want at most %d", readBytes, maximumSize+1)
	}
	if attachment != (DraftAttachment{}) {
		t.Fatalf("fingerprintAttachmentContextWithObserver() attachment = %+v, want empty result", attachment)
	}
}

func TestFingerprintAttachmentContextStopsCanceledWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attachment.txt")
	if err := os.WriteFile(path, []byte("attachment"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fingerprintAttachmentContext(ctx, path, MaximumDraftAttachmentBytes)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("fingerprintAttachmentContext() error = %v, want context.Canceled", err)
	}
}

func TestFingerprintAttachmentRejectsDirectoryBeforeOpening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attachment-directory")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}

	_, err := fingerprintAttachment(path, MaximumDraftAttachmentBytes)
	if err == nil {
		t.Fatal("fingerprintAttachment() error = nil, want regular-file validation")
	}
	if errorCode(err) != "invalid_argument" {
		t.Fatalf("fingerprintAttachment() error = %v, want invalid argument", err)
	}
}

func TestPrepareDraftContextStopsCanceledFingerprinting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attachment.txt")
	if err := os.WriteFile(path, []byte("attachment"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := prepareDraftWithAttachmentsObserverContext(ctx, CreateDraftRequest{
		Kind: DraftKindNew,
		Input: DraftInput{
			To:          []Recipient{{Address: "recipient@example.com"}},
			Body:        "Body",
			Attachments: []string{path},
		},
	}, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("prepareDraftWithAttachmentsObserverContext() error = %v, want context.Canceled", err)
	}
}
