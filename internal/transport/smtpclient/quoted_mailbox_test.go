package smtpclient

import (
	"bytes"
	"context"
	stdmail "net/mail"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

func TestQuotedMailboxStoredDraftLoopback(t *testing.T) {
	for _, test := range []struct {
		name, sender, recipient, hidden string
		utf8, supported, unbound        bool
	}{
		{name: "ASCII without capability", sender: `"A B"@icloud.com`, recipient: `"C D"@example.com`},
		{name: "ASCII with capability", sender: `"A B"@icloud.com`, recipient: `"C D"@example.com`, supported: true},
		{name: "unbound quoted sender", sender: `"A B"@icloud.com`, recipient: `"C D"@example.com`, unbound: true},
		{name: "escaped quotes", sender: `"A\"B"@icloud.com`, recipient: `"C\"D"@example.com`},
		{name: "escaped slashes", sender: `"A\\B"@icloud.com`, recipient: `"C\\D"@example.com`},
		{name: "embedded at", sender: `"A@B"@icloud.com`, recipient: `"C@D"@example.com`},
		{name: "UTF8 sender rejected", sender: `"Jörg Smith"@icloud.com`, recipient: `"C D"@example.com`, utf8: true},
		{name: "UTF8 sender accepted", sender: `"Jörg Smith"@icloud.com`, recipient: `"C D"@example.com`, utf8: true, supported: true},
		{name: "UTF8 recipient rejected", sender: `"A B"@icloud.com`, recipient: `"用户 Name"@example.com`, utf8: true},
		{name: "UTF8 recipient accepted", sender: `"A B"@icloud.com`, recipient: `"用户 Name"@example.com`, utf8: true, supported: true},
		{name: "UTF8 BCC rejected", sender: `"A B"@icloud.com`, recipient: `"C D"@example.com`, hidden: `"用户 Hidden"@example.com`, utf8: true},
		{name: "UTF8 BCC accepted", sender: `"A B"@icloud.com`, recipient: `"C D"@example.com`, hidden: `"用户 Hidden"@example.com`, utf8: true, supported: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			credential := `"Login Box"@icloud.com`
			if test.unbound {
				credential = test.sender
			}
			server := newFakeSMTPServer(t, func(server *fakeSMTPServer) {
				server.smtpUTF8, server.eightBitMIME, server.authUser = test.supported, test.supported, credential
			})
			root := t.TempDir()
			var bindings mail.AccountBindingStore
			account := ""
			if !test.unbound {
				bindings = mail.NewAccountBindingStore(filepath.Join(root, "bindings.json"))
				if err := bindings.UpsertAccountBinding(mail.AccountBinding{AccountID: "QUOTED", SenderAliases: []string{test.sender}, CredentialAccount: credential}); err != nil {
					t.Fatal(err)
				}
				var err error
				account, err = mailref.EncodeAccount("QUOTED")
				if err != nil {
					t.Fatal(err)
				}
			}
			if test.hidden == "" {
				test.hidden = `"Hidden Box"@example.com`
			}
			submitter := &loopbackDraftSubmitter{Client: testClient(), server: server}
			mirror := &utf8TestMirror{}
			service := mail.NewServiceWithTransport(nil, filepath.Join(root, "drafts"), mail.SendTransport{Submitter: submitter, Mirror: mirror, Credentials: utf8TestCredentials{credential: server.authPass}, AccountBindings: bindings})
			input := mail.DraftInput{AccountRef: account, From: "Jörg <" + test.sender + ">", To: []mail.Recipient{{Name: "Recipient", Address: test.recipient}}, CC: []mail.Recipient{{Name: "Copy", Address: `"Copy Box"@example.com`}}, BCC: []mail.Recipient{{Name: "Hidden", Address: test.hidden}}, Body: "Body"}
			draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: input})
			if err != nil {
				t.Fatal(err)
			}
			stored, err := service.GetDraft(draft.Ref)
			if err != nil || stored.Revision != draft.Revision || stored.From != input.From || !reflect.DeepEqual(stored.To, input.To) {
				t.Fatalf("stored draft changed identity: %+v, %v", stored, err)
			}
			if _, err := service.SendDraft(context.Background(), mail.SendDraftRequest{Ref: draft.Ref, ExpectedRevision: "different-review"}); transport.ErrorCode(err) != "draft_revision_conflict" || submitter.readerCalls != 0 {
				t.Fatalf("review guard: %v; calls=%d", err, submitter.readerCalls)
			}
			request := mail.SendDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision}
			result, err := service.SendDraft(context.Background(), request)
			if submitter.readerCalls != 1 {
				t.Fatalf("streaming calls = %d, error=%v", submitter.readerCalls, err)
			}
			if test.utf8 && !test.supported {
				retained, getErr := service.GetDraft(draft.Ref)
				server.mu.Lock()
				defer server.mu.Unlock()
				if transport.ErrorCode(err) != transport.CodeSMTPUTF8Unsupported || result.SubmissionAccepted || server.authCalls != 0 || server.mailCommand != "" || len(server.rcpts) != 0 || mirror.payload != nil || getErr != nil || retained.Revision != draft.Revision || retained.SendAttempt != nil {
					t.Fatalf("blocked UTF8: result=%+v error=%v retained=%+v get=%v MAIL=%q", result, err, retained, getErr, server.mailCommand)
				}
				return
			}
			if err != nil || !result.SubmissionAccepted {
				t.Fatalf("send = %+v, %v", result, err)
			}
			if _, err := service.SendDraft(context.Background(), request); err != nil || submitter.readerCalls != 1 {
				t.Fatalf("send replay: %v, calls=%d", err, submitter.readerCalls)
			}
			server.mu.Lock()
			defer server.mu.Unlock()
			if server.mailFrom != test.sender || !reflect.DeepEqual(server.rcpts, []string{test.recipient, `"Copy Box"@example.com`, test.hidden}) || !bytes.Equal(server.data, mirror.payload) {
				t.Fatalf("envelope/mirror: MAIL=%q RCPT=%q mirrorEqual=%t", server.mailFrom, server.rcpts, bytes.Equal(server.data, mirror.payload))
			}
			if strings.Contains(server.mailCommand, " SMTPUTF8") != test.utf8 || server.authCalls != 1 {
				t.Fatalf("SMTP negotiation: MAIL=%q AUTH=%d", server.mailCommand, server.authCalls)
			}
			message, err := stdmail.ReadMessage(bytes.NewReader(server.data))
			if err != nil {
				t.Fatal(err)
			}
			for _, header := range []struct{ field, name, address string }{{"From", "Jörg", test.sender}, {"To", "Recipient", test.recipient}, {"Cc", "Copy", `"Copy Box"@example.com`}} {
				actual, err := message.Header.AddressList(header.field)
				want, parseErr := stdmail.ParseAddress(header.address)
				if err != nil || parseErr != nil || len(actual) != 1 || actual[0].Address != want.Address || actual[0].Name != header.name {
					t.Errorf("%s = %+v, %v", header.field, actual, err)
				}
			}
			if message.Header.Get("Bcc") != "" {
				t.Fatal("BCC header disclosed")
			}
		})
	}
}
