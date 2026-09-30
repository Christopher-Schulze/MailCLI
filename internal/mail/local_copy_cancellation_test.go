package mail

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/transport"
)

func TestRecoveryWorkStopsCanceledInvocation(t *testing.T) {
	for _, phase := range []string{"send", "reconcile"} {
		t.Run(phase, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "drafts")
			submitter, mirror := sendTransportStubs()
			credentials := &stubCredentials{password: "secret"}
			service := newTransportService(root, submitter, mirror, credentials)
			draft := createTransportDraft(t, service)
			if phase == "reconcile" {
				mirror.err = &transport.TransportError{Code: transport.CodeIMAPAppendFailed, Message: "NO mailbox"}
				if _, err := service.SendDraft(context.Background(), SendDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision}); errorCode(err) != transport.CodeIMAPAppendFailed {
					t.Fatal(err)
				}
			}
			before := make(map[string][]byte)
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				payload, err := os.ReadFile(filepath.Join(root, entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				before[entry.Name()] = payload
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			credentials.loadHook = cancel
			submissions, appends := submitter.calls, mirror.calls
			if phase == "send" {
				_, err = service.SendDraft(ctx, SendDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision})
			} else {
				_, err = service.ReconcileDraft(ctx, draft.Ref)
			}
			if !errors.Is(err, context.Canceled) || submitter.calls != submissions || mirror.calls != appends {
				t.Fatalf("canceled %s: err=%v, SMTP=%d/%d, APPEND=%d/%d", phase, err, submitter.calls, submissions, mirror.calls, appends)
			}
			entries, err = os.ReadDir(root)
			if err != nil || len(entries) != len(before) {
				t.Fatalf("retained state entries=%d/%d, err=%v", len(entries), len(before), err)
			}
			for _, entry := range entries {
				payload, err := os.ReadFile(filepath.Join(root, entry.Name()))
				if err != nil || !bytes.Equal(payload, before[entry.Name()]) {
					t.Fatalf("retained %s changed: %v", entry.Name(), err)
				}
			}
		})
	}
}

// Cancels a real context at a deterministic read-boundary check, without timing races.
type readBoundaryCancelContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *readBoundaryCancelContext) Err() error {
	c.checks++
	if c.checks == 3 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestRecoverySpoolCancellationPreservesOwnership(t *testing.T) {
	message, err := ComposeMessageSpool(Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Body: strings.Repeat("x", 1<<20),
	}, "<cancel@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := message.Remove(); err != nil {
			t.Error(err)
		}
	})
	root := t.TempDir()
	ref := "draft_123456789012345678901234"
	pinnedRoot, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pinnedRoot.Close(); err != nil {
			t.Error(err)
		}
	})
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := directory.Close(); err != nil {
			t.Error(err)
		}
	})
	state := &draftStorage{rootName: root, root: pinnedRoot, directory: directory}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &readBoundaryCancelContext{Context: base, cancel: cancel}
	if _, err := persistAcceptedMessageSpool(ctx, root, ref, message, state); !errors.Is(err, context.Canceled) || ctx.checks < 3 {
		t.Fatalf("persist err=%v checks=%d", err, ctx.checks)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unpublished temporary survived: %v, %v", entries, err)
	}
	spool, err := persistAcceptedMessageSpool(context.Background(), root, ref, message, state)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ref+".send-spool")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	base, cancel = context.WithCancel(context.Background())
	defer cancel()
	ctx = &readBoundaryCancelContext{Context: base, cancel: cancel}
	attempt := SendAttempt{MessageID: message.MessageID(), RecoverySpool: spool}
	if _, err := openAcceptedMessageSpool(ctx, ref, attempt, state); !errors.Is(err, context.Canceled) || errorCode(err) != "draft_operation_canceled" {
		t.Fatalf("verification err=%v checks=%d", err, ctx.checks)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("retained bytes changed: %v", err)
	}
	if err := removeAcceptedMessageSpool(ref, &attempt, state); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("terminal cleanup did not finish: %v", err)
	}
}
