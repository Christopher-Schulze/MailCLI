package mail

import (
	"context"
	"strings"
	"testing"
)

func TestNewMessageFromHeaderDecodesAndBoundsText(t *testing.T) {
	tests := []struct {
		name   string
		header string
		seen   bool
		want   NewMessage
	}{
		{
			name: "encoded subject, named sender, RFC date",
			header: "From: =?UTF-8?Q?J=C3=BCrgen_M=C3=BCller?= <j@example.com>\r\n" +
				"Subject: =?UTF-8?B?w5xiZXJyYXNjaHVuZw==?=\r\nDate: Tue, 29 Sep 2026 10:15:00 +0200\r\nMessage-ID: <m1@example.com>\r\n\r\n",
			want: NewMessage{
				ServerRef: "srv_x", Subject: "Überraschung", Sender: "Jürgen Müller <j@example.com>",
				DateSent: "2026-09-29T08:15:00Z", MessageID: "<m1@example.com>", Unseen: true,
			},
		},
		{
			name:   "folded subject, bare address, seen",
			header: "From: plain@example.com\r\nSubject: first\r\n second\r\n\ttail\r\n\r\n",
			seen:   true,
			want:   NewMessage{ServerRef: "srv_x", Subject: "first second tail", Sender: "plain@example.com", Unseen: false},
		},
		{
			name:   "missing fields and an unparseable date stay empty",
			header: "Date: not a date\r\n\r\n",
			want:   NewMessage{ServerRef: "srv_x", Unseen: true},
		},
		{
			name:   "control characters become single spaces and cannot start a new line",
			header: "Subject: a\x00b\x07c   d\r\n\r\n",
			want:   NewMessage{ServerRef: "srv_x", Subject: "a b c d", Unseen: true},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := NewMessageFromHeader("srv_x", []byte(test.header), test.seen); got != test.want {
				t.Fatalf("NewMessageFromHeader() = %+v, want %+v", got, test.want)
			}
		})
	}
	long := NewMessageFromHeader("srv_x", []byte("Subject: "+strings.Repeat("ä", 500)+"\r\n\r\n"), true)
	if runes := []rune(long.Subject); len(runes) != newMessageTextRunes || runes[len(runes)-1] != '…' {
		t.Fatalf("long subject has %d runes, want %d ending in an ellipsis", len(runes), newMessageTextRunes)
	}
}

type newMessagesGateway struct {
	Gateway
	got NewMessagesRequest
}

func (g *newMessagesGateway) NewMessages(_ context.Context, request NewMessagesRequest) (NewMessagesResult, error) {
	g.got = request
	return NewMessagesResult{Complete: true}, nil
}

func TestServiceNewMessagesValidatesTheLimitAndNeedsAReader(t *testing.T) {
	gateway := &newMessagesGateway{}
	service := NewService(gateway)
	if _, err := service.NewMessages(context.Background(), NewMessagesRequest{}); err != nil || gateway.got.Limit != DefaultNewMessagesLimit {
		t.Fatalf("default limit = %d, err = %v", gateway.got.Limit, err)
	}
	for _, limit := range []int{-1, MaximumNewMessagesLimit + 1} {
		if _, err := service.NewMessages(context.Background(), NewMessagesRequest{Limit: limit}); err == nil {
			t.Errorf("limit %d accepted", limit)
		}
	}
	if _, err := NewService(struct{ Gateway }{}).NewMessages(context.Background(), NewMessagesRequest{}); err == nil {
		t.Error("a gateway without NewMessagesReader accepted the call")
	}
}
