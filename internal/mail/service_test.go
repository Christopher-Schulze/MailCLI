package mail

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

type gatewayStub struct {
	listRequest ListMessagesRequest
	mailboxes   []Mailbox
	accounts    []Account
}

func (g *gatewayStub) Probe(context.Context, bool) DiagnosticReport {
	return DiagnosticReport{}
}

func (g *gatewayStub) ListAccounts(context.Context) ([]Account, error) {
	return g.accounts, nil
}

func (g *gatewayStub) ListMailboxes(context.Context, ListMailboxesRequest) ([]Mailbox, error) {
	return g.mailboxes, nil
}

func TestResolveMailboxUsesExactPath(t *testing.T) {
	gateway := &gatewayStub{mailboxes: []Mailbox{
		{Ref: "parent", Path: []string{"Projects"}},
		{Ref: "child", Path: []string{"Projects", "Sent"}},
	}}
	mailbox, err := NewService(gateway).ResolveMailbox(
		context.Background(), "acct_ref", []string{"Projects", "Sent"},
	)
	if err != nil || mailbox.Ref != "child" {
		t.Fatalf("ResolveMailbox() = %+v, error = %v", mailbox, err)
	}
	if _, err := NewService(gateway).ResolveMailbox(context.Background(), "acct_ref", []string{"sent"}); err == nil {
		t.Fatal("ResolveMailbox() case-insensitive match error = nil")
	}
}

func (g *gatewayStub) ListMessages(_ context.Context, request ListMessagesRequest) (MessagePage, error) {
	g.listRequest = request
	return MessagePage{}, nil
}

func (g *gatewayStub) GetMessage(context.Context, string) (Message, error) {
	return Message{}, nil
}

func (g *gatewayStub) OpenDraft(context.Context, string) (Message, error) {
	return Message{}, nil
}

func (g *gatewayStub) GetRawSource(context.Context, string) (string, error) {
	return "", nil
}

func (g *gatewayStub) SaveAttachmentTo(context.Context, string, string, string) error {
	return nil
}

func (g *gatewayStub) SaveDraft(context.Context, Draft) (MessageSummary, error) {
	return MessageSummary{Ref: "msg_saved"}, nil
}

func (g *gatewayStub) SendDraft(context.Context, Draft) (SendEvidence, error) {
	return SendEvidence{InvocationStarted: true, AcceptedByMail: true}, nil
}

func (g *gatewayStub) MarkMessage(context.Context, MarkMessageRequest) (MessageSummary, error) {
	return MessageSummary{}, nil
}

func (g *gatewayStub) TransferMessage(context.Context, TransferMessageRequest) (MessageSummary, error) {
	return MessageSummary{}, nil
}

func (g *gatewayStub) DeleteMessage(_ context.Context, request DeleteMessageRequest) (DeleteResult, error) {
	return DeleteResult{MessageRef: request.Ref, Deleted: true}, nil
}

func (g *gatewayStub) Sync(context.Context, string) error {
	return nil
}

func TestServiceValidationTable(t *testing.T) {
	tests := []struct {
		name    string
		run     func(*Service) error
		wantErr bool
	}{
		{name: "missing mailbox", run: func(service *Service) error {
			_, err := service.ListMessages(context.Background(), ListMessagesRequest{})
			return err
		}, wantErr: true},
		{name: "missing message detail ref", run: func(service *Service) error {
			_, err := service.GetMessage(context.Background(), "")
			return err
		}, wantErr: true},
		{name: "missing raw source ref", run: func(service *Service) error {
			_, err := service.GetRawSource(context.Background(), "")
			return err
		}, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := NewService(&gatewayStub{})
			err := test.run(service)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestServiceNormalizesLimits(t *testing.T) {
	gateway := &gatewayStub{}
	service := NewService(gateway)

	if _, err := service.ListMessages(context.Background(), ListMessagesRequest{MailboxRef: "mailbox", Limit: 0}); err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	if gateway.listRequest.Limit != DefaultPageLimit {
		t.Fatalf("list limit = %d, want %d", gateway.listRequest.Limit, DefaultPageLimit)
	}

	if _, err := service.ListMessages(context.Background(), ListMessagesRequest{
		MailboxRef: "mailbox", Limit: MaximumPageLimit + 1,
	}); err == nil {
		t.Fatal("ListMessages() error = nil, want maximum limit error")
	}
}

func TestMutationValidationTable(t *testing.T) {
	service := NewService(&gatewayStub{})
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "mark without state", run: func() error {
			_, err := service.MarkMessage(context.Background(), MarkMessageRequest{Ref: "msg_ref"})
			return err
		}},
		{name: "transfer without destination", run: func() error {
			_, err := service.TransferMessage(context.Background(), TransferMessageRequest{Ref: "msg_ref"})
			return err
		}},
		{name: "delete without ref", run: func() error {
			_, err := service.DeleteMessage(context.Background(), DeleteMessageRequest{})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); err == nil {
				t.Fatal("error = nil")
			}
		})
	}
}

func TestServicePassthroughAndHealth(t *testing.T) {
	gateway := &gatewayStub{
		accounts: []Account{{Ref: "acct", Name: "Inbox Account"}},
	}
	service := NewService(gateway)

	accounts, err := service.ListAccounts(context.Background())
	if err != nil || len(accounts) != 1 || accounts[0].Ref != "acct" {
		t.Fatalf("ListAccounts() = %#v, error = %v", accounts, err)
	}

	report := service.Probe(context.Background(), false)
	if !IsHealthy(report) {
		t.Fatal("empty probe should be healthy")
	}
	if IsHealthy(DiagnosticReport{Checks: []Check{{Name: "store", Status: "fail"}}}) {
		t.Fatal("failed check should not be healthy")
	}

	raw := &bytes.Buffer{}
	if err := service.WriteRawSource(context.Background(), "msg", raw); err != nil {
		t.Fatalf("WriteRawSource() error = %v", err)
	}
	if err := service.WriteRawSource(context.Background(), "", raw); err == nil {
		t.Fatal("WriteRawSource() empty ref error = nil")
	}

	syncResult, err := service.Sync(context.Background(), "acct")
	if err != nil || !syncResult.Triggered || syncResult.AccountRef != "acct" {
		t.Fatalf("Sync() = %#v, error = %v", syncResult, err)
	}

	deleted, err := service.DeleteMessage(context.Background(), DeleteMessageRequest{Ref: "msg"})
	if err != nil || !deleted.Deleted || deleted.MessageRef != "msg" {
		t.Fatalf("DeleteMessage() = %#v, error = %v", deleted, err)
	}

	if (SendTransport{}).ImapClient() != nil {
		t.Fatal("empty transport ImapClient() != nil")
	}
	if elapsedMilliseconds(time.Now()) < 0 {
		t.Fatal("elapsedMilliseconds() < 0")
	}
}

type serviceStreamingGateway struct {
	*gatewayStub
}

func (g serviceStreamingGateway) WriteRawSource(_ context.Context, _ string, writer io.Writer) error {
	_, err := io.WriteString(writer, "streamed source")
	return err
}

type serviceSyncChecker struct {
	*gatewayStub
	result SyncCheckResult
}

func (g serviceSyncChecker) SyncCheck(context.Context, string) (SyncCheckResult, error) {
	return g.result, nil
}

func TestServicePassthroughsDraftMailboxAndStreamingOperations(t *testing.T) {
	gateway := &gatewayStub{mailboxes: []Mailbox{{Ref: "mbx"}}}
	service := NewService(&serviceStreamingGateway{gatewayStub: gateway})

	mailboxes, err := service.ListMailboxes(context.Background(), ListMailboxesRequest{AccountRef: "acct"})
	if err != nil || len(mailboxes) != 1 || mailboxes[0].Ref != "mbx" {
		t.Fatalf("ListMailboxes() = %#v, error = %v", mailboxes, err)
	}
	if _, err := service.OpenDraft(context.Background(), "draft_ref"); err != nil {
		t.Fatalf("OpenDraft() error = %v", err)
	}
	var raw bytes.Buffer
	if err := service.WriteRawSource(context.Background(), "msg_ref", &raw); err != nil {
		t.Fatalf("streaming WriteRawSource() error = %v", err)
	}
	if raw.String() != "streamed source" {
		t.Fatalf("streaming raw source = %q, want streamed source", raw.String())
	}

	if _, err := service.SyncCheck(context.Background(), "acct"); err == nil {
		t.Fatal("SyncCheck() without checker error = nil")
	}
	want := SyncCheckResult{AccountRef: "acct", Complete: true}
	checked := NewService(serviceSyncChecker{gatewayStub: gateway, result: want})
	got, err := checked.SyncCheck(context.Background(), "acct")
	if err != nil || got.AccountRef != want.AccountRef || !got.Complete {
		t.Fatalf("SyncCheck() = %#v, error = %v, want %#v", got, err, want)
	}
}
