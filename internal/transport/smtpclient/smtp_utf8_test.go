package smtpclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	stdmail "net/mail"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

func TestSubmitSMTPUTF8Negotiation(t *testing.T) {
	for _, test := range []struct {
		name, from, recipient, message string
		supported, eightBit, beforeTLS bool
		wantUTF8, wantUnsupported      bool
	}{
		{name: "ASCII without extension"},
		{name: "ASCII with extension", supported: true, eightBit: true},
		{name: "sender rejected", from: "Jörg@example.com", wantUnsupported: true},
		{name: "recipient rejected", recipient: "用户@example.com", wantUnsupported: true},
		{name: "sender supported", from: "Jörg@example.com", supported: true, eightBit: true, wantUTF8: true},
		{name: "recipient supported", recipient: "用户@example.com", supported: true, eightBit: true, wantUTF8: true},
		{name: "domain preserved", recipient: "name@bücher.example", supported: true, eightBit: true, wantUTF8: true},
		{name: "quoted local preserved", from: `"Jörg Smith"@example.com`, supported: true, eightBit: true, wantUTF8: true},
		{name: "raw header rejected", message: "Subject: Grüße\r\n\r\nBody\r\n", wantUnsupported: true},
		{name: "raw header supported", message: "Subject: Grüße\r\n\r\nBody\r\n", supported: true, eightBit: true, wantUTF8: true},
		{name: "body is not a header", message: "Content-Type: text/plain; charset=utf-8\r\n\r\nGrüße\r\n", eightBit: true},
		{name: "nested header rejected", message: "Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Description: Grüße\r\n\r\nBody\r\n--x--\r\n", wantUnsupported: true},
		{name: "post TLS capabilities", from: "Jörg@example.com", beforeTLS: true, eightBit: true, wantUnsupported: true},
		{name: "8BITMIME also required", from: "Jörg@example.com", supported: true, wantUnsupported: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.from == "" {
				test.from = "sender@example.com"
			}
			if test.recipient == "" {
				test.recipient = "recipient@example.com"
			}
			if test.message == "" {
				test.message = testMessage
			}
			server := newFakeSMTPServer(t, func(server *fakeSMTPServer) {
				server.smtpUTF8, server.eightBitMIME, server.utf8BeforeTLSOnly = test.supported, test.eightBit, test.beforeTLS
			})
			cfg := transport.SubmitConfig{Host: server.host(), Port: server.port(), Username: server.authUser, Password: server.authPass}
			evidence, err := testClient().Submit(context.Background(), cfg, test.from, []string{test.recipient}, []byte(test.message))
			server.mu.Lock()
			defer server.mu.Unlock()
			if test.wantUnsupported {
				if transport.ErrorCode(err) != transport.CodeSMTPUTF8Unsupported || evidence != (transport.SubmitEvidence{}) || server.mailCommand != "" || server.authCalls != 0 || len(server.rcpts) != 0 || len(server.data) != 0 {
					t.Fatalf("unsupported submission: evidence=%+v error=%v MAIL=%q recipients=%v DATA=%q", evidence, err, server.mailCommand, server.rcpts, server.data)
				}
				guidance := mail.GuidanceForError("drafts.send", err)
				if guidance.EffectCertainty != mail.EffectNone || guidance.Retryability != mail.RetryUserInputRequired || guidance.ReplayAllowed {
					t.Fatalf("unsupported guidance = %+v", guidance)
				}
				return
			}
			if err != nil || !strings.HasPrefix(evidence.ServerResponse, "250 ") || server.mailFrom != test.from || !reflect.DeepEqual(server.rcpts, []string{test.recipient}) || string(server.data) != test.message {
				t.Fatalf("submission: evidence=%+v error=%v MAIL=%q recipients=%v DATA=%q", evidence, err, server.mailCommand, server.rcpts, server.data)
			}
			if strings.Contains(server.mailCommand, " SMTPUTF8") != test.wantUTF8 || strings.Contains(server.mailCommand, " BODY=8BITMIME") != test.eightBit {
				t.Fatalf("MAIL parameters = %q", server.mailCommand)
			}
		})
	}
}

func TestSubmitRejectsInvalidEnvelopeBeforeMAIL(t *testing.T) {
	for _, value := range []string{"bad@@example.com", "missing-domain", "Name <name@example.com>", "<name@example.com>", "name@example.com (comment)", "name@example.com\r\nRCPT TO:<other@example.com>", "name\x00@example.com", "name\xff@example.com", "@example.com", "name@", "name name@example.com"} {
		t.Run(value, func(t *testing.T) {
			server := newFakeSMTPServer(t)
			cfg := transport.SubmitConfig{Host: server.host(), Port: server.port(), Username: server.authUser, Password: server.authPass}
			for _, from := range []string{value, "sender@example.com"} {
				_, err := testClient().Submit(context.Background(), cfg, from, []string{value}, []byte(testMessage))
				if transport.ErrorCode(err) != transport.CodeInvalidAddress {
					t.Fatalf("invalid envelope %q: %v", value, err)
				}
			}
			server.mu.Lock()
			defer server.mu.Unlock()
			if server.mailCommand != "" || server.authCalls != 0 {
				t.Fatalf("invalid envelope reached SMTP: MAIL=%q AUTH=%d", server.mailCommand, server.authCalls)
			}
		})
	}
}

func TestSubmissionScansMIMEHeadersAndRewinds(t *testing.T) {
	embedded := "Subject: Grüße\r\n\r\nBody\r\n"
	for _, test := range []struct {
		name, message string
		want          bool
	}{
		{name: "encoded display", message: "From: =?UTF-8?b?SsO2cmc=?= <sender@example.com>\r\n\r\nBody\r\n"},
		{name: "folded raw value", message: "Subject: Greeting\r\n Grüße\r\n\r\nBody\r\n", want: true},
		{name: "raw embedded", message: "Content-Type: message/rfc822\r\n\r\n" + embedded, want: true},
		{name: "raw global", message: "Content-Type: message/global\r\n\r\n" + embedded, want: true},
		{name: "encoded embedded", message: "Content-Type: message/global\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte(embedded)) + "\r\n"},
		{name: "quoted printable embedded", message: "Content-Type: message/global\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nSubject: Gr=C3=BC=C3=9Fe\r\n\r\nBody\r\n"},
		{name: "digest defaults", message: "Content-Type: multipart/digest; boundary=x\r\n\r\n--x\r\n\r\n" + embedded + "\r\n--x--\r\n", want: true},
		{name: "later sibling", message: "Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n\r\nGrüße\r\n--x\r\nContent-Type: multipart/alternative; boundary=y\r\n\r\n--y\r\nContent-Description: Grüße\r\n\r\nBody\r\n--y--\r\n--x--\r\n", want: true},
		{name: "attachment bytes", message: "Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: application/octet-stream\r\n\r\n\xff\x00\xfe\r\n--x--\r\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := strings.NewReader("prefix" + test.message)
			if _, err := reader.Seek(6, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			got, err := submissionRequiresUTF8(context.Background(), "sender@example.com", []string{"recipient@example.com"}, reader, int64(len(test.message)))
			if err != nil || got != test.want {
				t.Fatalf("UTF8 requirement = %t, %v; want %t", got, err, test.want)
			}
			replayed, err := io.ReadAll(reader)
			if err != nil || string(replayed) != test.message {
				t.Fatalf("preflight altered replay: %q, %v", replayed, err)
			}
		})
	}
}

func TestSubmissionPreflightRejectsUnclassifiableMIME(t *testing.T) {
	for _, message := range []string{
		"Subject: incomplete", "Subject: \xff\r\n\r\n", "Content-Type: multipart/mixed\r\n\r\n",
		"Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n\r\nunterminated body",
		"Content-Type: invalid; type\r\n\r\n", "Subject: " + strings.Repeat("x", smtpHeaderBudget+1) + "\r\n\r\n",
		"Content-Type: multipart/mixed; boundary=x\r\nContent-Transfer-Encoding: base64\r\n\r\n--x\r\nContent-Description: Grüße\r\n\r\nBody\r\n--x--\r\n",
		strings.Repeat("Content-Type: message/rfc822\r\n\r\n", smtpDepthLimit+2) + testMessage,
		"Content-Type: multipart/mixed; boundary=x\r\n\r\n" + strings.Repeat("--x\r\n\r\nbody\r\n", smtpPartLimit+1) + "--x--\r\n",
	} {
		reader := strings.NewReader(message)
		_, err := submissionRequiresUTF8(context.Background(), "sender@example.com", []string{"recipient@example.com"}, reader, int64(len(message)))
		if transport.ErrorCode(err) != transport.CodeSMTPRejected {
			t.Fatalf("unclassifiable MIME (%d bytes) error = %v", len(message), err)
		}
		position, seekErr := reader.Seek(0, io.SeekCurrent)
		if seekErr != nil || position != 0 {
			t.Fatalf("failed preflight source position = %d, %v", position, seekErr)
		}
	}
}

type rewindFailureSource struct{ *strings.Reader }

func (r rewindFailureSource) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekStart {
		return 0, errors.New("rewind unavailable")
	}
	return r.Reader.Seek(offset, whence)
}

func TestSubmissionPreflightCancellationAndRewindFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, test := range []struct {
		name   string
		ctx    context.Context
		reader io.ReadSeeker
		code   string
	}{
		{name: "canceled", ctx: ctx, reader: strings.NewReader(testMessage), code: transport.CodeSMTPTimeout},
		{name: "rewind failure", ctx: context.Background(), reader: rewindFailureSource{strings.NewReader(testMessage)}, code: transport.CodeSMTPRejected},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newFakeSMTPServer(t)
			cfg := transport.SubmitConfig{Host: server.host(), Port: server.port()}
			_, err := testClient().SubmitReader(test.ctx, cfg, "sender@example.com", []string{"recipient@example.com"}, "", test.reader, int64(len(testMessage)))
			if transport.ErrorCode(err) != test.code {
				t.Fatalf("preflight = %v, want %s", err, test.code)
			}
			server.mu.Lock()
			defer server.mu.Unlock()
			if server.mailCommand != "" || server.authCalls != 0 {
				t.Fatalf("failed preflight reached SMTP: %q", server.mailCommand)
			}
		})
	}
}

func TestComposedMessageSeekPreservesIndependentPinnedViews(t *testing.T) {
	message, err := mail.ComposeMessageSpool(mail.Draft{From: "Jörg <sender@example.com>", To: []mail.Recipient{{Address: "recipient@example.com"}}, Body: "Grüße"}, "<seek@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := message.Remove(); err != nil {
			t.Error(err)
		}
	})
	first, err := message.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := first.Close(); err != nil {
			t.Error(err)
		}
	}()
	second, err := message.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := message.Remove(); err != nil {
		t.Fatal(err)
	}
	expected, err := io.ReadAll(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, seek := range []struct {
		offset int64
		whence int
		want   int64
	}{
		{offset: -5, whence: io.SeekEnd, want: message.Size() - 5},
		{offset: -1, whence: io.SeekCurrent, want: message.Size() - 6},
		{offset: message.Size() + 1, whence: io.SeekStart, want: message.Size() + 1},
		{offset: 0, whence: io.SeekStart, want: 0},
	} {
		if position, err := first.Seek(seek.offset, seek.whence); err != nil || position != seek.want {
			t.Fatalf("seek %+v = %d, %v", seek, position, err)
		}
	}
	for _, reader := range []io.Reader{first, second} {
		actual, err := io.ReadAll(reader)
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatalf("independent seek replay differs: %v", err)
		}
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Seek(0, io.SeekStart); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed seek error = %v", err)
	}
}

// Only endpoint routing is injected; draft persistence, MIME, TLS, AUTH,
// SMTP negotiation, DATA, and send-state transitions use production code.
type loopbackDraftSubmitter struct {
	*Client
	server      *fakeSMTPServer
	readerCalls int
}

func (s *loopbackDraftSubmitter) SubmitReader(ctx context.Context, cfg transport.SubmitConfig, from string, recipients []string, messageID string, source io.ReadSeeker, size int64) (transport.SubmitEvidence, error) {
	s.readerCalls++
	cfg.Host, cfg.Port = s.server.host(), s.server.port()
	return s.Client.SubmitReader(ctx, cfg, from, recipients, messageID, source, size)
}

type utf8TestCredentials map[string]string

func (s utf8TestCredentials) Load(account string) (string, error) { return s[account], nil }
func (s utf8TestCredentials) Store(account, password string) error {
	s[account] = password
	return nil
}
func (s utf8TestCredentials) Delete(account string) error {
	delete(s, account)
	return nil
}

type utf8TestMirror struct{ payload []byte }

func (s *utf8TestMirror) AppendToSent(_ context.Context, _ transport.ImapConfig, message []byte, _ string) (transport.AppendEvidence, error) {
	s.payload = append([]byte(nil), message...)
	return transport.AppendEvidence{Mailbox: "Sent", Appended: true}, nil
}

func TestStoredDraftSMTPUTF8AndAliasIdentity(t *testing.T) {
	for _, test := range []struct {
		name, sender, recipient string
		supported, wantBlocked  bool
	}{
		{name: "display name only", sender: "alias@icloud.com", recipient: "recipient@example.com"},
		{name: "international alias rejected", sender: "Jörg@icloud.com", recipient: "recipient@example.com", wantBlocked: true},
		{name: "international alias supported", sender: "Jörg@icloud.com", recipient: "recipient@example.com", supported: true},
		{name: "international BCC rejected", sender: "alias@icloud.com", recipient: "用户@example.com", wantBlocked: true},
		{name: "international BCC supported", sender: "alias@icloud.com", recipient: "用户@example.com", supported: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newFakeSMTPServer(t, func(server *fakeSMTPServer) {
				server.smtpUTF8, server.eightBitMIME = test.supported, test.supported
				server.authUser = "login@icloud.com"
			})
			root := t.TempDir()
			bindings := mail.NewAccountBindingStore(filepath.Join(root, "bindings.json"))
			if err := bindings.UpsertAccountBinding(mail.AccountBinding{AccountID: "ACCOUNT-UTF8", SenderAliases: []string{test.sender}, CredentialAccount: server.authUser}); err != nil {
				t.Fatal(err)
			}
			account, err := mailref.EncodeAccount("ACCOUNT-UTF8")
			if err != nil {
				t.Fatal(err)
			}
			submitter := &loopbackDraftSubmitter{Client: testClient(), server: server}
			mirror := &utf8TestMirror{}
			service := mail.NewServiceWithTransport(nil, filepath.Join(root, "drafts"), mail.SendTransport{Submitter: submitter, Mirror: mirror, Credentials: utf8TestCredentials{server.authUser: server.authPass}, AccountBindings: bindings})
			draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{AccountRef: account, From: "Jörg <" + test.sender + ">", BCC: []mail.Recipient{{Address: test.recipient}}, Subject: "UTF8", Body: "Grüße"}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := service.SendDraft(context.Background(), mail.SendDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision})
			if submitter.readerCalls != 1 {
				t.Fatalf("streaming calls = %d", submitter.readerCalls)
			}
			server.mu.Lock()
			defer server.mu.Unlock()
			if test.wantBlocked {
				retained, getErr := service.GetDraft(draft.Ref)
				if transport.ErrorCode(err) != transport.CodeSMTPUTF8Unsupported || result.SubmissionAccepted || server.mailCommand != "" || mirror.payload != nil || getErr != nil || retained.Revision != draft.Revision || retained.SendAttempt != nil {
					t.Fatalf("blocked draft: result=%+v error=%v retained=%+v get=%v MAIL=%q", result, err, retained, getErr, server.mailCommand)
				}
				return
			}
			if err != nil || !result.SubmissionAccepted || server.mailFrom != test.sender || !reflect.DeepEqual(server.rcpts, []string{test.recipient}) || !bytes.Equal(mirror.payload, server.data) {
				t.Fatalf("stored draft: result=%+v error=%v MAIL=%q recipients=%v", result, err, server.mailCommand, server.rcpts)
			}
			message, err := stdmail.ReadMessage(bytes.NewReader(server.data))
			if err != nil {
				t.Fatal(err)
			}
			from, err := message.Header.AddressList("From")
			if err != nil || len(from) != 1 || from[0].Name != "Jörg" || from[0].Address != test.sender || !strings.HasPrefix(message.Header.Get("From"), "=?UTF-8?b?SsO2cmc=?= <") || message.Header.Get("Bcc") != "" {
				t.Fatalf("From=%q parsed=%v error=%v BCC=%q", message.Header.Get("From"), from, err, message.Header.Get("Bcc"))
			}
			if strings.Contains(server.mailCommand, " SMTPUTF8") != test.supported {
				t.Fatalf("MAIL = %q", server.mailCommand)
			}
		})
	}
}
