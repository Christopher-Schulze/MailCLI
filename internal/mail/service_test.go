package mail

import (
	"context"
	"testing"
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

func (g *gatewayStub) DeleteMessage(context.Context, string) error {
	return nil
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
			_, err := service.DeleteMessage(context.Background(), "")
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
