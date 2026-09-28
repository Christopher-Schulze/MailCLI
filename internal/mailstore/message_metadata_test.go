package mailstore

import (
	"context"
	"mailcli/internal/mail"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestExcerptMIMEFixtures(t *testing.T) {
	for _, test := range []struct {
		name, source, want string
		complete           bool
	}{
		{"plain signature", "Content-Type: text/plain\r\n\r\nHello\r\n> quoted\r\n-- \r\nSignature", "Hello", true},
		{"html only", "Content-Type: text/html; charset=utf-8\r\n\r\n<p>Hello <b>world</b></p>", "Hello world", true},
		{"plain preferred", "Content-Type: multipart/alternative; boundary=x\r\n\r\n--x\r\nContent-Type: text/html\r\n\r\n<p>HTML</p>\r\n--x\r\nContent-Type: text/plain\r\n\r\nPlain\r\n--x--\r\n", "Plain", true},
		{"invalid MIME", "no header boundary", "", false},
		{"iso-8859-1", "Content-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nGr=FC=DFe aus M=FCnchen\r\n", "Grüße aus München", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			text, complete := excerptText(context.Background(), []byte(test.source))
			got := mail.BuildExcerpt(text, 240)
			if got != test.want || complete != test.complete {
				t.Fatalf("excerpt=%q complete=%t", got, complete)
			}
		})
	}
}

func TestExcerptSourceCapAndThreadingRead(t *testing.T) {
	store, ref, _ := newLargeReadIntentFixture(t, 512<<10)
	metrics := &readMetrics{}
	store.readMetrics = metrics
	client := &Client{store: store}
	result, err := client.EnrichMessage(context.Background(), ref, mail.MessageEnrichmentRequest{Excerpt: true, ExcerptLength: 13})
	if err != nil || result.ExcerptSource != mail.ExcerptSourceLocal || result.ExcerptComplete || utf8.RuneCountInString(result.Excerpt) > 13 || metrics.sourceBytes.Load() > mail.MaximumExcerptSourceBytes {
		t.Fatalf("metadata=%+v bytes=%d err=%v", result, metrics.sourceBytes.Load(), err)
	}
	metrics.sourceBytes.Store(0)
	result, err = client.EnrichMessage(context.Background(), ref, mail.MessageEnrichmentRequest{Threading: true, ExcerptLength: 240})
	if err != nil || !result.ThreadingComplete || metrics.sourceBytes.Load() > maximumHeaderBytes || result.Excerpt != "" {
		t.Fatalf("threading=%+v bytes=%d err=%v", result, metrics.sourceBytes.Load(), err)
	}
}

func TestExcerptUsesCompleteLargeLocalSourceWithoutIMAP(t *testing.T) {
	store, ref, _ := newLargeReadIntentFixture(t, 512<<10)
	data, complete, localPartial, err := store.readExcerptSource(context.Background(), ref)
	if err != nil || complete || localPartial || int64(len(data)) != mail.MaximumExcerptSourceBytes {
		t.Fatalf("bytes=%d complete=%t partial=%t err=%v", len(data), complete, localPartial, err)
	}
	if excerptNeedsRemote(err, localPartial) {
		t.Fatal("a complete local source larger than the cap requested an IMAP partial fetch")
	}
	if !excerptNeedsRemote(nil, true) {
		t.Fatal("a partial local source did not allow the bounded IMAP prefix")
	}
	if excerptNeedsRemote(context.Canceled, false) {
		t.Fatal("an unsafe local error allowed an IMAP fetch")
	}
}

func TestEnrichmentReportsTheFailureInsteadOfHidingIt(t *testing.T) {
	store, _, _ := newLargeReadIntentFixture(t, 1024)
	client := &Client{store: store}
	for _, request := range []mail.MessageEnrichmentRequest{
		{Threading: true, ExcerptLength: 240},
		{Excerpt: true, ExcerptLength: 240},
	} {
		result, err := client.EnrichMessage(context.Background(), "msg_not-a-reference", request)
		if err != nil || result.EnrichmentError == "" || result.ThreadingComplete || result.Excerpt != "" {
			t.Fatalf("request=%+v result=%+v err=%v", request, result, err)
		}
	}
}

func TestExcerptDecodedTextCap(t *testing.T) {
	text, complete := excerptText(context.Background(), []byte("Content-Type: text/plain\r\n\r\n"+strings.Repeat("x", int(mail.MaximumExcerptSourceBytes))))
	if complete || int64(len(text)) > mail.MaximumExcerptSourceBytes {
		t.Fatalf("text bytes=%d complete=%t", len(text), complete)
	}
}
