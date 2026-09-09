package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

type directRecoverySubmitter struct {
	message []byte
}

func (s *directRecoverySubmitter) Submit(
	ctx context.Context,
	cfg transport.SubmitConfig,
	from string,
	rcpts []string,
	message []byte,
) (transport.SubmitEvidence, error) {
	_ = ctx
	_ = cfg
	_ = from
	_ = rcpts
	s.message = append(s.message[:0], message...)
	return transport.SubmitEvidence{}, &transport.SubmissionError{Stage: "final_reply"}
}

type directRecoveryCredentials struct{}

func (directRecoveryCredentials) Load(string) (string, error) { return "secret", nil }
func (directRecoveryCredentials) Store(string, string) error  { return nil }
func (directRecoveryCredentials) Delete(string) error         { return nil }

type directRecoveryIMAP struct {
	raw         []byte
	listCalls   int
	searchCalls int
	fetchCalls  int
}

func (i *directRecoveryIMAP) AppendToSent(
	context.Context, transport.ImapConfig, []byte, string,
) (transport.AppendEvidence, error) {
	return transport.AppendEvidence{}, errors.New("unexpected APPEND during direct recovery")
}

func (i *directRecoveryIMAP) ListMailboxes(context.Context, transport.ImapConfig) ([]transport.MailboxInfo, error) {
	i.listCalls++
	return []transport.MailboxInfo{{
		Name: "Sent", WireName: "Sent", DisplayName: "Sent", DisplayPath: []string{"Sent"}, Flags: []string{"\\Sent"},
	}}, nil
}

func (i *directRecoveryIMAP) SearchUID(
	context.Context, transport.ImapConfig, string, string,
) (uint32, uint32, int, error) {
	i.searchCalls++
	return 42, 7, 1, nil
}

func (i *directRecoveryIMAP) FetchMessage(
	context.Context, transport.ImapConfig, string, uint32, uint32, int64,
) ([]byte, error) {
	i.fetchCalls++
	return append([]byte(nil), i.raw...), nil
}

func (i *directRecoveryIMAP) SetFlags(
	context.Context, transport.ImapConfig, string, uint32, uint32, []string, []string,
) (transport.MutationEvidence, error) {
	return transport.MutationEvidence{}, errors.New("unexpected STORE during direct recovery")
}

func (i *directRecoveryIMAP) CopyMessage(
	context.Context, transport.ImapConfig, string, uint32, uint32, string,
) (transport.MutationEvidence, error) {
	return transport.MutationEvidence{}, errors.New("unexpected COPY during direct recovery")
}

func (i *directRecoveryIMAP) MoveMessage(
	context.Context, transport.ImapConfig, string, uint32, uint32, string,
) (transport.MutationEvidence, error) {
	return transport.MutationEvidence{}, errors.New("unexpected MOVE during direct recovery")
}

func (i *directRecoveryIMAP) DeleteMessage(
	context.Context, transport.ImapConfig, string, uint32, uint32,
) (transport.MutationEvidence, error) {
	return transport.MutationEvidence{}, errors.New("unexpected DELETE during direct recovery")
}

func (i *directRecoveryIMAP) CheckStatus(
	context.Context, transport.ImapConfig, string,
) (transport.MailboxStatus, error) {
	return transport.MailboxStatus{}, errors.New("unexpected STATUS during direct recovery")
}

func TestRunWithFactoriesDirectRecoverySkipsMailStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configRoot, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir() error = %v", err)
	}
	root := filepath.Join(configRoot, "MailCLI", "drafts")
	submitter := &directRecoverySubmitter{}
	imap := &directRecoveryIMAP{}
	service := mail.NewServiceWithTransport(nil, root, mail.SendTransport{
		Submitter:   submitter,
		Mirror:      imap,
		Credentials: directRecoveryCredentials{},
	})
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		From: "sender@icloud.com", To: []mail.Recipient{{Address: "recipient@example.com"}},
		Subject: "Direct recovery", Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	if _, err := service.SendDraft(context.Background(), draft.Ref); err == nil {
		t.Fatal("SendDraft() error = nil, want unknown submission")
	} else {
		var submissionErr *transport.SubmissionError
		if !errors.As(err, &submissionErr) {
			t.Fatalf("SendDraft() error = %v, want unknown submission", err)
		}
	}
	if len(submitter.message) == 0 {
		t.Fatal("submitter did not retain the sent MIME")
	}
	imap.raw = append([]byte(nil), submitter.message...)

	factoryCalls := 0
	stdout, stderr, code := runWithArgsAndStderrUsing(t, []string{
		"mailcli", "drafts", "reconcile", "--ref", draft.Ref, "--json",
	}, func() int {
		return runWithFactories(nil, func() *invocationTransport {
			factoryCalls++
			return &invocationTransport{
				SendTransport: mail.SendTransport{
					Submitter: submitter, Mirror: imap, Credentials: directRecoveryCredentials{}, Imap: imap,
				},
			}
		})
	})
	if code != 0 {
		t.Fatalf("runWithFactories() = %d, stderr = %q, stdout = %q", code, stderr, stdout)
	}
	if factoryCalls != 1 {
		t.Fatalf("transport factory calls = %d, want 1", factoryCalls)
	}
	if imap.listCalls != 1 || imap.searchCalls != 1 || imap.fetchCalls != 1 {
		t.Fatalf("IMAP calls = list:%d search:%d fetch:%d, want one each", imap.listCalls, imap.searchCalls, imap.fetchCalls)
	}
	var response struct {
		OK      bool   `json:"ok"`
		Command string `json:"command"`
		Data    struct {
			SendResult *mail.SendResult `json:"send_result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &response); err != nil {
		t.Fatalf("decode reconcile response: %v; stdout = %q", err, stdout)
	}
	if !response.OK || response.Command != "drafts.reconcile" || response.Data.SendResult == nil ||
		response.Data.SendResult.Outcome != mail.SendOutcomeSent || response.Data.SendResult.DraftRetained {
		t.Fatalf("reconcile response = %+v", response)
	}
	if _, err := service.GetDraft(draft.Ref); err == nil {
		t.Fatal("reconciled draft still exists")
	}
	if _, err := service.GetSendReceipt(draft.Ref); err != nil {
		t.Fatalf("GetSendReceipt() error = %v", err)
	}
}
