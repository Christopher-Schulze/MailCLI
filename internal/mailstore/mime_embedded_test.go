package mailstore

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mailcli/internal/mail"
)

const embeddedMessageFixture = "From: Inner <inner@example.com>\r\n" +
	"To: Nested <nested@example.com>\r\nMessage-ID: <inner@example.com>\r\n" +
	"Subject: Original message\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
	"Original body with Grüße.\r\n"

func TestEmbeddedMessageWithoutAttachmentHints(t *testing.T) {
	for _, mediaType := range []string{"message/rfc822", "message/global"} {
		t.Run(mediaType, func(t *testing.T) {
			source := "Message-ID: <outer@example.com>\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
				"--b\r\nContent-Type: text/plain\r\n\r\nOuter body\r\n" +
				"--b\r\nContent-Type: " + mediaType + "\r\n\r\n" + embeddedMessageFixture + "\r\n--b--\r\n"
			document, err := parseMIMEDocument(strings.NewReader(source), false, true, false)
			if err != nil {
				t.Fatal(err)
			}
			part, exists := document.Parts["2"]
			digest := sha256.Sum256([]byte(embeddedMessageFixture))
			if !document.Complete || document.MessageID != "outer@example.com" || document.Content != "Outer body" ||
				len(document.Parts) != 1 || !exists || !part.Complete || part.Name != "" || part.MIMEType != mediaType ||
				part.Size != int64(len(embeddedMessageFixture)) || part.SHA256 != hex.EncodeToString(digest[:]) {
				t.Fatalf("embedded message was lost or changed: document = %+v, part = %+v", document, part)
			}
		})
	}
}

func TestEmbeddedMessageEncodingAndNestedRetrieval(t *testing.T) {
	nested := "Subject: Container\r\nContent-Type: multipart/mixed; boundary=inner\r\n\r\n" +
		"Preamble\r\n--inner\r\nContent-Type: text/plain\r\n\r\nNested text\r\n" +
		"--inner\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\nAAECAw==\r\n" +
		"--inner\r\nContent-Type: message/rfc822\r\n\r\n" + embeddedMessageFixture + "\r\n--inner--\r\nEpilogue\r\n"
	for _, test := range []struct {
		name, headers, encoded, original, filename string
	}{
		{"global-base64", "Content-Type: message/global\r\nContent-Transfer-Encoding: base64", base64.StdEncoding.EncodeToString([]byte(embeddedMessageFixture)), embeddedMessageFixture, ""},
		{"global-quoted-printable", "Content-Type: message/global\r\nContent-Transfer-Encoding: quoted-printable", "Subject: Gr=C3=BC=C3=9Fe\r\n\r\nBody=3Dvalue\r\n", "Subject: Grüße\r\n\r\nBody=value\r\n", ""},
		{"rfc822-base64", "Content-Type: message/rfc822\r\nContent-Transfer-Encoding: base64", base64.StdEncoding.EncodeToString([]byte(embeddedMessageFixture)), embeddedMessageFixture, ""},
		{"inline-named", "Content-Type: message/rfc822\r\nContent-Disposition: inline; filename=original.eml", embeddedMessageFixture, embeddedMessageFixture, "original.eml"},
		{"type-name", "Content-Type: message/rfc822; name=original.eml", embeddedMessageFixture, embeddedMessageFixture, "original.eml"},
		{"empty-body", "Content-Type: message/rfc822", "Subject: Empty\r\n\r\n", "Subject: Empty\r\n\r\n", ""},
		{"nested-multipart-message", "Content-Type: message/rfc822", nested, nested, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := embeddedOuterFixture(test.headers, test.encoded)
			document, err := parseMIMEDocument(strings.NewReader(source), false, true, false)
			part := document.Parts["2"]
			digest := sha256.Sum256([]byte(test.original))
			if err != nil || !document.Complete || len(document.Parts) != 1 || !part.Complete ||
				part.Name != test.filename || part.Size != int64(len(test.original)) || part.SHA256 != hex.EncodeToString(digest[:]) ||
				document.MessageID != "outer@example.com" || document.Content != "Before\n\nAfter" {
				t.Fatalf("embedded representation: %+v, part: %+v, error: %v", document, part, err)
			}
			output := filepath.Join(t.TempDir(), "original.eml")
			proof, err := extractMIMEAttachmentWithEvidence(strings.NewReader(source), "2", output)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(output)
			if err != nil || string(saved) != test.original || proof.Size != part.Size || proof.SHA256 != part.SHA256 {
				t.Fatalf("retrieved bytes: %q, proof: %+v, error: %v", saved, proof, err)
			}
		})
	}
}

func TestEmbeddedMessageMalformedContentRemainsVisible(t *testing.T) {
	for _, test := range []struct{ name, headers, body string }{
		{"bad-base64", "Content-Transfer-Encoding: base64\r\n", "!!!!"},
		{"truncated-base64", "Content-Transfer-Encoding: base64\r\n", "U3ViamVjdDogQQ0KDQpC="},
		{"unknown-wrapper-encoding", "Content-Transfer-Encoding: x-unknown\r\n", embeddedMessageFixture},
		{"missing-header-boundary", "", "Subject: Header only"},
		{"malformed-header", "", "Subject: Example\r\ninvalid header\r\n\r\nBody"},
		{"missing-identity-headers", "", "Content-Type: text/plain\r\n\r\nBody"},
		{"malformed-address", "", "Subject: Example\r\nTo: broken <\r\n\r\nBody"},
		{"malformed-content-type", "", "Subject: Example\r\nContent-Type: text/plain; broken\r\n\r\nBody"},
		{"unknown-inner-encoding", "", "Subject: Example\r\nContent-Transfer-Encoding: x-unknown\r\n\r\nBody"},
		{"unknown-inner-charset", "", "Subject: Example\r\nContent-Type: text/plain; charset=x-unknown\r\n\r\nBody"},
		{"truncated-inner-multipart", "", "Subject: Example\r\nContent-Type: multipart/mixed; boundary=nested\r\n\r\n--nested\r\n\r\nBody"},
		{"externalized-wrapper", "X-Apple-Content-Length: 999999\r\n", embeddedMessageFixture},
		{"externalized-inner-leaf", "", "Subject: Example\r\nX-Apple-Content-Length: 999999\r\n\r\nBody"},
		{"externalized-inner-message", "", "Subject: Example\r\nContent-Type: message/rfc822\r\nX-Apple-Content-Length: 999999\r\n\r\n" + embeddedMessageFixture},
		{"externalized-inner-multipart", "", "Subject: Example\r\nContent-Type: multipart/mixed; boundary=i\r\nX-Apple-Content-Length: 999999\r\n\r\n--i\r\n\r\nBody\r\n--i--\r\n"},
		{"invalid-nested-message", "", "Subject: Example\r\nContent-Type: message/rfc822\r\n\r\nSubject: Unfinished"},
		{"unsupported-nested-message", "", "Subject: Example\r\nContent-Type: message/partial; id=x; number=1\r\n\r\nBody"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := embeddedOuterFixture("Content-Type: message/rfc822\r\n"+strings.TrimSuffix(test.headers, "\r\n"), test.body)
			for _, skip := range []bool{false, true} {
				document, err := parseMIMEDocument(strings.NewReader(source), false, true, skip)
				part, exists := document.Parts["2"]
				if err != nil || document.Complete || !exists || part.Complete || part.SHA256 != "" ||
					!containsMissingPart(document, "2") || document.MessageID != "outer@example.com" || document.Content != "Before\n\nAfter" {
					t.Fatalf("skip=%v: malformed embedded content: %+v, part: %+v, error: %v", skip, document, part, err)
				}
			}
		})
	}
}

func TestEmbeddedMessageDigestDefaultsAndIdentity(t *testing.T) {
	for _, test := range []struct{ name, prefix, suffix, id string }{
		{"digest", "Content-Type: multipart/digest; boundary=d\r\n\r\n--d\r\n\r\n", "\r\n--d\r\nContent-Type: text/plain\r\n\r\nExplicit text\r\n--d--\r\n", "1"},
		{"nested-digest", "Content-Type: multipart/mixed; boundary=o\r\n\r\n--o\r\nContent-Type: multipart/digest; boundary=d\r\n\r\n--d\r\n\r\n", "\r\n--d--\r\n--o--\r\n", "1.1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := test.prefix + embeddedMessageFixture + test.suffix
			document, err := parseMIMEDocument(strings.NewReader(source), false, true, false)
			part := document.Parts[test.id]
			if err != nil || !document.Complete || len(document.Parts) != 1 || !part.Complete || part.MIMEType != "message/rfc822" {
				t.Fatalf("digest part: %+v, document: %+v, error: %v", part, document, err)
			}
			output := filepath.Join(t.TempDir(), "digest.eml")
			if err := extractMIMEAttachment(strings.NewReader(source), test.id, output); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(output)
			if err != nil || string(data) != embeddedMessageFixture {
				t.Fatalf("digest extraction: %q, error: %v", data, err)
			}
			if test.name == "digest" && document.Content != "Explicit text" {
				t.Fatalf("explicit type overridden: %q", document.Content)
			}
		})
	}
}

func TestEmbeddedMessageSharedBudgetsAndPartialSource(t *testing.T) {
	source := embeddedOuterFixture("Content-Type: message/rfc822", embeddedMessageFixture)
	for _, test := range []struct {
		name          string
		partial, skip bool
		resource      mimeBudgetResource
		limit         int64
	}{
		{name: "complete"}, {name: "skip", skip: true}, {name: "partial", partial: true},
		{name: "text", resource: mimeBudgetTextBytes, limit: 10},
		{name: "parts", resource: mimeBudgetParts, limit: 3},
		{name: "metadata", resource: mimeBudgetMetadata, limit: 400},
		{name: "raw", resource: mimeBudgetRawBytes, limit: int64(len(source) - 100)},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits := defaultMIMEParseBudgetLimits()
			switch test.resource {
			case mimeBudgetTextBytes:
				limits.textBytes = test.limit
			case mimeBudgetParts:
				limits.parts = test.limit
			case mimeBudgetMetadata:
				limits.metadata = test.limit
			case mimeBudgetRawBytes:
				limits.rawBytes = test.limit
			}
			document, err := parseMIMEDocumentWithLimits(context.Background(), strings.NewReader(source), test.partial, true, test.skip, limits)
			if err != nil {
				t.Fatal(err)
			}
			part := document.Parts["2"]
			if test.resource != "" {
				if document.Complete || part.Complete || part.SHA256 != "" || !containsMissingPart(document, mimeBudgetDiagnosticID+string(test.resource)) {
					t.Fatalf("budget escaped: %+v, part: %+v", document, part)
				}
			} else if document.Complete == test.partial || part.Complete == test.partial || (part.SHA256 == "") != (test.partial || test.skip) || document.budget.rawBytes != int64(len(source)) {
				t.Fatalf("partial/skip/raw accounting: %+v, part: %+v, bytes: %d", document, part, document.budget.rawBytes)
			}
			if document.budget.rawBytes > limits.rawBytes || document.budget.metadata > limits.metadata || document.budget.parts > limits.parts || document.budget.textBytes > limits.textBytes {
				t.Fatalf("counters exceed limits: %+v", document.budget)
			}
		})
	}
	for _, count := range []int{62, 63} {
		source := "Content-Type: message/rfc822\r\n\r\n" + strings.Repeat("Subject: Nested\r\nContent-Type: message/rfc822\r\n\r\n", count) + embeddedMessageFixture
		document, err := parseMIMEDocument(strings.NewReader(source), false, true, false)
		if err != nil || document.Complete != (count == 62) || containsMissingPart(document, "mime:budget:depth") != (count == 63) {
			t.Fatalf("depth count=%d: %+v, error: %v", count, document, err)
		}
	}
}

func TestEmbeddedMessageCancellation(t *testing.T) {
	reader := newBlockingMIMEReader("Content-Type: message/rfc822\r\n\r\nSubject: Blocked\r\n\r\n", "Body")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		document mimeDocument
		err      error
	}
	done := make(chan result, 1)
	go func() {
		document, err := parseMIMEDocumentWithContext(ctx, reader, false, true, false)
		done <- result{document, err}
	}()
	select {
	case <-reader.bodyStarted:
	case <-time.After(time.Second):
		t.Fatal("parser never reached embedded body")
	}
	cancel()
	select {
	case outcome := <-done:
		part := outcome.document.Parts["1"]
		if !errors.Is(outcome.err, context.Canceled) || outcome.document.Complete || part.Complete || part.SHA256 != "" || !containsMissingPart(outcome.document, mimeCanceledDiagnosticID) {
			t.Fatalf("cancellation lost: %+v, part: %+v", outcome, part)
		}
	case <-time.After(time.Second):
		t.Fatal("embedded parsing did not stop on cancellation")
	}
}

func TestEmbeddedMessageStoreClientServiceAndHydration(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		store, inbox := newSearchFixture(t)
		closeTestResource(t, store, "embedded-message store")
		page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inbox, Limit: 3})
		if err != nil {
			t.Fatal(err)
		}
		ref := messageRefWithExpectedID(t, messageRefWithSubject(t, page.Messages, "Status Update"), "<102@example.com>")
		body := embeddedMessageFixture
		if malformed {
			body = "Subject: Truncated"
		}
		source := strings.Replace(embeddedOuterFixture("Content-Type: message/rfc822", body), "<outer@example.com>", "<102@example.com>", 1)
		writeFixtureEMLX(t, store, 102, "imap://"+testAccountID+"/INBOX", []byte(source))
		service := mail.NewService(&Client{store: store})
		local, err := service.GetMessage(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		hydrated, err := messageFromRawFallback(context.Background(), mail.Message{}, local.Summary, source)
		if err != nil {
			t.Fatal(err)
		}
		for _, result := range []mail.Message{local, hydrated} {
			if result.ContentComplete == malformed || result.Summary.MessageID != "102@example.com" || result.Summary.Subject != "Status Update" || result.Content != "Before\n\nAfter" || len(result.Attachments) != 1 || result.Summary.AttachmentCount != 1 {
				t.Fatalf("message mapping: %+v", result)
			}
			attachment := result.Attachments[0]
			if attachment.ID != "2" || attachment.Name != "" || attachment.MIMEType == nil || *attachment.MIMEType != "message/rfc822" || attachment.Downloaded == malformed || attachment.SizeKnown == malformed || malformed && len(result.MissingParts) == 0 {
				t.Fatalf("attachment mapping: %+v", result)
			}
		}
		hasAttachment := true
		query, err := mail.PrepareQuery(mail.Query{MailboxRef: inbox, Subject: "Status Update", HasAttachment: &hasAttachment, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		matches, err := store.SearchMessages(context.Background(), query)
		if err != nil || len(matches.Messages) != 1 || matches.Messages[0].Summary.AttachmentCount != 1 || matches.Coverage.Complete == malformed {
			t.Fatalf("embedded attachment search: %+v, error: %v", matches, err)
		}
		output := filepath.Join(t.TempDir(), "retrieved.eml")
		saved, err := service.SaveAttachment(context.Background(), mail.SaveAttachmentRequest{MessageRef: ref, AttachmentID: "2", OutputPath: output})
		if malformed {
			if _, statErr := os.Stat(output); err == nil || !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("incomplete local attachment saved: %+v, error: %v, stat: %v", saved, err, statErr)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(output)
		digest := sha256.Sum256([]byte(body))
		if err != nil || string(data) != body || saved.Size != int64(len(body)) || saved.SHA256 != hex.EncodeToString(digest[:]) || saved.Path != output {
			t.Fatalf("service extraction: %+v, bytes: %q, error: %v", saved, data, err)
		}
		info, err := os.Stat(output)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("saved mode: %v, error: %v", info, err)
		}
	}
}

func TestEmbeddedMessageHeaderAndTextBounds(t *testing.T) {
	for _, test := range []struct {
		name, body string
		complete   bool
	}{
		{"header-too-large", "Subject: " + strings.Repeat("x", maximumHeaderBytes) + "\r\n\r\nBody", false},
		{"text-at-limit", "Subject: Text\r\n\r\n" + strings.Repeat("x", maximumTextPartBytes), true},
		{"text-over-limit", "Subject: Text\r\n\r\n" + strings.Repeat("x", maximumTextPartBytes+1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			document, err := parseMIMEDocument(strings.NewReader(embeddedOuterFixture("Content-Type: message/rfc822", test.body)), false, true, false)
			part, exists := document.Parts["2"]
			if err != nil || !exists || document.Complete != test.complete || part.Complete != test.complete || (part.SHA256 != "") != test.complete || document.Content != "Before\n\nAfter" {
				t.Fatalf("bounded embedded content: %+v, part: %+v, error: %v", document, part, err)
			}
		})
	}
}

func FuzzEmbeddedMessageExactRawRetrieval(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("\x00\xff\r\n--outer\r\nSubject: Body bytes\r\n\r\n"))
	f.Add([]byte(embeddedMessageFixture))
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 4096 {
			return
		}
		original := "Subject: Binary message\r\nContent-Type: application/octet-stream\r\n\r\n" + string(body)
		source := embeddedOuterFixture("Content-Type: message/global\r\nContent-Transfer-Encoding: base64", base64.StdEncoding.EncodeToString([]byte(original)))
		document, err := parseMIMEDocument(strings.NewReader(source), false, true, false)
		digest := sha256.Sum256([]byte(original))
		part, exists := document.Parts["2"]
		if err != nil || !exists || !document.Complete || !part.Complete || part.Size != int64(len(original)) || part.SHA256 != hex.EncodeToString(digest[:]) {
			t.Fatalf("raw embedded parse: %+v, part: %+v, error: %v", document, part, err)
		}
		output := filepath.Join(t.TempDir(), "binary.eml")
		proof, err := extractMIMEAttachmentWithEvidence(strings.NewReader(source), "2", output)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(output)
		if err != nil || string(data) != original || proof.Size != part.Size || proof.SHA256 != part.SHA256 {
			t.Fatalf("raw embedded extraction: %+v, bytes: %q, error: %v", proof, data, err)
		}
	})
}

func embeddedOuterFixture(headers, body string) string {
	return "Message-ID: <outer@example.com>\r\nContent-Type: multipart/mixed; boundary=outer\r\n\r\n" +
		"--outer\r\nContent-Type: text/plain\r\n\r\nBefore\r\n" +
		"--outer\r\n" + strings.TrimSuffix(headers, "\r\n") + "\r\n\r\n" + body +
		"\r\n--outer\r\nContent-Type: text/plain\r\n\r\nAfter\r\n--outer--\r\n"
}

func TestExtractMIMEFirstPartNeverSelectsMultipartRoot(t *testing.T) {
	for _, mediaType := range []string{"application/octet-stream", "message/rfc822"} {
		t.Run(mediaType, func(t *testing.T) {
			source := "Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
				"--b\r\nContent-Type: " + mediaType + "\r\n\r\n" + embeddedMessageFixture + "\r\n--b--\r\n"
			output := filepath.Join(t.TempDir(), "original.eml")
			proof, err := extractMIMEAttachmentWithEvidence(strings.NewReader(source), "1", output)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(output)
			digest := sha256.Sum256([]byte(embeddedMessageFixture))
			if err != nil || string(saved) != embeddedMessageFixture || proof.Size != int64(len(saved)) ||
				proof.SHA256 != hex.EncodeToString(digest[:]) {
				t.Fatalf("selected part differs: bytes = %q, proof = %+v, error = %v", saved, proof, err)
			}
		})
	}
}
