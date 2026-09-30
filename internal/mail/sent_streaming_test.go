package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	stdmail "net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/transport"
)

type sentStreamSubmitter struct {
	stubSubmitter
	streamCalls int
}

func (s *sentStreamSubmitter) SubmitReader(_ context.Context, _ transport.SubmitConfig, _ string, _ []string, messageID string, reader io.ReadSeeker, size int64) (transport.SubmitEvidence, error) {
	s.streamCalls++
	written, err := io.Copy(io.Discard, reader)
	if err != nil || written != size {
		return transport.SubmitEvidence{}, errors.Join(err, io.ErrUnexpectedEOF)
	}
	return transport.SubmitEvidence{MessageID: messageID, ServerResponse: "250 accepted"}, nil
}

type sentStreamMirror struct {
	stubMirror
	streamCalls int
}

func (s *sentStreamMirror) AppendToSentReader(_ context.Context, _ transport.ImapConfig, _ io.Reader, _ int64, _ string) (transport.AppendEvidence, error) {
	s.streamCalls++
	return transport.AppendEvidence{}, &transport.TransportError{Code: transport.CodeIMAPAppendIncomplete, Message: "fixture disconnected before APPEND"}
}

type sentStreamIMAP struct {
	reconcileImapStub
	message     *ComposedMessage
	streamCalls int
	closeCalls  int
	maximum     int64
}

type sentStreamReader struct {
	io.ReadSeekCloser
	owner *sentStreamIMAP
}

func (r *sentStreamReader) Close() error {
	r.owner.closeCalls++
	return r.ReadSeekCloser.Close()
}

func (s *sentStreamIMAP) FetchMessageReader(_ context.Context, _ transport.ImapConfig, mailbox string, uid uint32, validity uint32, maximum int64) (io.ReadSeekCloser, int64, error) {
	s.streamCalls++
	s.maximum = maximum
	if mailbox != "Sent" || uid != 42 || validity != 77 {
		return nil, 0, errors.New("wrong FETCH identity")
	}
	if s.message.Size() > maximum {
		return nil, 0, &transport.TransportError{Code: transport.CodeIMAPRawSourceTooLarge, Message: "fixture exceeds FETCH bound"}
	}
	reader, err := s.message.Open()
	if err != nil {
		return nil, 0, err
	}
	return &sentStreamReader{ReadSeekCloser: reader, owner: s}, s.message.Size(), nil
}

func (s *sentStreamIMAP) FetchMessage(_ context.Context, _ transport.ImapConfig, _ string, _ uint32, _ uint32, maximum int64) ([]byte, error) {
	s.fetchCalls++
	if s.message.Size() > maximum {
		return nil, &transport.TransportError{Code: transport.CodeIMAPRawSourceTooLarge, Message: "fixture exceeds byte FETCH bound"}
	}
	return nil, errors.New("streaming fixture must not use byte FETCH")
}

func TestReconcileLargeSentCandidateStreamsAttachments(t *testing.T) {
	for _, test := range []struct {
		name     string
		accepted bool
		changed  bool
	}{
		{name: "unknown"},
		{name: "unknown changed byte", changed: true},
		{name: "accepted mirror pending", accepted: true},
		{name: "accepted changed byte", accepted: true, changed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "large.bin")
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if err := errors.Join(file.Truncate(MaximumRawSourceBytes+1), file.Close()); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(directory, "drafts")
			submitter, mirror := &sentStreamSubmitter{}, &sentStreamMirror{}
			service := NewServiceWithTransport(nil, root, SendTransport{Submitter: submitter, Mirror: mirror, Credentials: &stubCredentials{password: "secret"}})
			draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
				From: "sender@icloud.com", To: []Recipient{{Address: "recipient@example.com"}},
				Subject: "Large send", Body: "Body", Attachments: []string{path},
			}})
			if err != nil {
				t.Fatal(err)
			}
			messageID := "<claim@example.com>"
			if test.accepted {
				result, err := service.SendDraft(context.Background(), SendDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision})
				if errorCode(err) != transport.CodeIMAPAppendIncomplete || result.Outcome != SendOutcomeMirrorPending || !result.DraftRetained {
					t.Fatalf("SendDraft() = %+v, %v", result, err)
				}
				retained, err := service.GetDraft(draft.Ref)
				if err != nil || retained.SendAttempt == nil {
					t.Fatalf("missing accepted claim: %+v, %v", retained, err)
				}
				messageID = retained.SendAttempt.MessageID
			} else {
				beginUnknownClaim(t, root, draft)
			}
			candidate := draft
			if test.changed {
				file, err := os.OpenFile(path, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.WriteAt([]byte{1}, 0); err != nil {
					t.Fatal(err)
				}
				hash := sha256.New()
				if _, err := io.Copy(hash, file); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
				candidate.Attachments = append([]DraftAttachment(nil), draft.Attachments...)
				candidate.Attachments[0].SHA256 = hex.EncodeToString(hash.Sum(nil))
			}
			message, err := composeDraftSpool(context.Background(), candidate, messageID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := message.Remove(); err != nil {
					t.Error(err)
				}
			})
			if message.Size() <= MaximumRawSourceBytes || draft.Attachments[0].Size <= MaximumRawSourceBytes {
				t.Fatal("fixture does not cross both former limits")
			}
			imap := &sentStreamIMAP{message: message, reconcileImapStub: reconcileImapStub{
				mailboxes: []transport.MailboxInfo{{Name: "Sent", Flags: []string{"\\Sent"}}}, uid: 42,
			}}
			service.send.Imap = imap
			result, err := service.ReconcileDraft(context.Background(), draft.Ref)
			if test.changed {
				if errorCode(err) != "send_identity_mismatch" || !result.DraftRetained {
					t.Fatalf("changed ReconcileDraft() = %+v, %v", result, err)
				}
				if retained, err := service.GetDraft(draft.Ref); err != nil || retained.SendAttempt == nil {
					t.Fatalf("claim lost: %+v, %v", retained, err)
				}
			} else if err != nil || result.Outcome != SendOutcomeSent || !result.Reconciled || result.DraftRetained || result.Receipt == nil {
				t.Fatalf("ReconcileDraft() = %+v, %v", result, err)
			}
			wantSends := 0
			if test.accepted {
				wantSends = 1
			}
			if submitter.streamCalls != wantSends || mirror.streamCalls != wantSends || submitter.calls != 0 || mirror.calls != 0 {
				t.Fatalf("submission/APPEND replay: streams %d/%d, bytes %d/%d", submitter.streamCalls, mirror.streamCalls, submitter.calls, mirror.calls)
			}
			if imap.streamCalls != 1 || imap.closeCalls != 1 || imap.fetchCalls != 0 || imap.maximum != maximumAcceptedMessageSpoolBytes {
				t.Fatalf("FETCH streams/closes/bytes/bound = %d/%d/%d/%d", imap.streamCalls, imap.closeCalls, imap.fetchCalls, imap.maximum)
			}
		})
	}
}

type sentIdentitySource struct {
	io.Reader
	closed   int
	closeErr error
}

func (s *sentIdentitySource) Close() error {
	s.closed++
	return s.closeErr
}

func TestFetchedSentIdentityBoundsAndOwnership(t *testing.T) {
	draft := Draft{From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}}, Subject: "Subject", Body: "Body"}
	messageID := "<identity@example.com>"
	raw, err := BuildMessage(draft, messageID)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := draftMIMEFingerprint(draft)
	if err != nil {
		t.Fatal(err)
	}
	closeErr := errors.New("close failed")
	for _, test := range []struct {
		name     string
		size     int64
		canceled bool
		closeErr error
		wantCode string
		wantErr  error
	}{
		{name: "exact", size: int64(len(raw))},
		{name: "understated", size: int64(len(raw) - 1), wantCode: "send_identity_unreadable"},
		{name: "overstated", size: int64(len(raw) + 1), wantCode: "send_identity_unreadable"},
		{name: "negative", size: -1, wantCode: "send_identity_unreadable"},
		{name: "oversize", size: maximumAcceptedMessageSpoolBytes + 1, wantCode: "send_identity_unreadable"},
		{name: "canceled", size: int64(len(raw)), canceled: true, wantCode: "send_identity_unreadable", wantErr: context.Canceled},
		{name: "close failure", size: int64(len(raw)), closeErr: closeErr, wantErr: closeErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.canceled {
				cancel()
			}
			source := &sentIdentitySource{Reader: strings.NewReader(string(raw)), closeErr: test.closeErr}
			err := verifyFetchedSentIdentity(ctx, source, test.size, draft, SendAttempt{MessageID: messageID, MIMEFingerprint: fingerprint})
			wantFailure := test.wantCode != "" || test.wantErr != nil
			if (err != nil) != wantFailure || errorCode(err) != test.wantCode || (test.wantErr != nil && !errors.Is(err, test.wantErr)) || source.closed != 1 {
				t.Fatalf("verify = %v, closes = %d; want code %q, cause %v", err, source.closed, test.wantCode, test.wantErr)
			}
		})
	}
	if err := verifyFetchedSentIdentity(context.Background(), nil, 0, draft, SendAttempt{}); errorCode(err) != "send_identity_unreadable" {
		t.Fatalf("nil source = %v", err)
	}
}

type legacySentIMAP struct {
	reconcileImapStub
	maximum int64
}

func (s *legacySentIMAP) FetchMessage(_ context.Context, _ transport.ImapConfig, _ string, _ uint32, _ uint32, maximum int64) ([]byte, error) {
	s.maximum = maximum
	return s.fetchRaw, s.fetchErr
}

func TestSentIdentityLegacyFetcherKeepsByteBound(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  []byte
		code string
	}{
		{name: "small", raw: []byte("source")},
		{name: "oversize", raw: make([]byte, MaximumRawSourceBytes+1), code: "send_identity_unreadable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			imap := &legacySentIMAP{reconcileImapStub: reconcileImapStub{fetchRaw: test.raw}}
			source, size, err := fetchSentMessageSource(context.Background(), imap, transport.ImapConfig{}, "Sent", 42, 77)
			if (err != nil) != (test.code != "") || errorCode(err) != test.code || imap.maximum != MaximumRawSourceBytes {
				t.Fatalf("legacy FETCH = %v, bound = %d", err, imap.maximum)
			}
			if err != nil {
				if source != nil {
					t.Fatal("oversized source was published")
				}
				return
			}
			if size != int64(len(test.raw)) {
				t.Fatalf("source size = %d", size)
			}
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSentIdentityAttachmentAggregateBounds(t *testing.T) {
	for _, test := range []struct {
		name  string
		count int
		size  int64
		code  string
	}{
		{name: "maximum bytes", count: 1, size: MaximumDraftAttachmentBytes},
		{name: "aggregate bytes", count: 1, size: MaximumDraftAttachmentBytes + 1, code: "send_identity_unreadable"},
		{name: "maximum count", count: MaximumDraftAttachments},
		{name: "attachment count", count: MaximumDraftAttachments + 1, code: "send_identity_unreadable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "sparse-")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := file.Close(); err != nil {
					t.Error(err)
				}
			}()
			if err := file.Truncate(test.size); err != nil {
				t.Fatal(err)
			}
			parts := []io.Reader{strings.NewReader("--fixture\r\nContent-Type: text/plain\r\n\r\nBody\r\n")}
			for i := 0; i < test.count; i++ {
				parts = append(parts, strings.NewReader("--fixture\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=test.bin\r\n\r\n"), io.NewSectionReader(file, 0, test.size), strings.NewReader("\r\n"))
			}
			parts = append(parts, strings.NewReader("--fixture--\r\n"))
			identity, err := parseMIMEIdentity(stdmail.Header{"Content-Type": {"multipart/mixed; boundary=fixture"}}, io.MultiReader(parts...))
			if (err != nil) != (test.code != "") || errorCode(err) != test.code {
				t.Fatalf("aggregate bound = %v", err)
			}
			if err == nil && (len(identity.attachments) != test.count || identity.attachmentBytes != test.size*int64(test.count)) {
				t.Fatalf("attachment identity = %+v", identity)
			}
		})
	}
}
