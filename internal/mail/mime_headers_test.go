package mail

import (
	"bytes"
	"context"
	"fmt"
	"mime"
	stdmail "net/mail"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestComposedSubjectRoundTripsWithinHeaderLimits(t *testing.T) {
	for _, test := range []struct{ name, value string }{
		{"empty", ""}, {"short", "Reviewed subject"},
		{"encoded boundary minus one", strings.Repeat("a", 66)},
		{"encoded boundary", strings.Repeat("a", 67)},
		{"encoded boundary plus one", strings.Repeat("a", 68)},
		{"physical boundary minus one", strings.Repeat("a", 988)},
		{"physical boundary", strings.Repeat("a", 989)},
		{"physical boundary plus one", strings.Repeat("a", 990)},
		{"review reproduction", strings.Repeat("a", 2000)},
		{"maximum draft subject", strings.Repeat("a", MaximumDraftSubjectBytes)},
		{"two byte characters", strings.Repeat("é", 100)},
		{"three byte characters", strings.Repeat("界", 100)},
		{"four byte characters", strings.Repeat("🦊", 100)},
		{"mixed encoding boundary", strings.Repeat("a", 44) + "🦊" + strings.Repeat("b", 80)},
		{"folding whitespace", "  \t" + strings.Repeat("one  two\tthree ", 100) + " \t "},
		{"literal encoded word", "=?UTF-8?b?YQ==?="},
		{"punctuation", strings.Repeat("\"quoted\\ value\" (comment), ", 50)},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload, err := BuildMessage(Draft{From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}}, Subject: test.value, Body: "Body"}, "<header@example.com>")
			if err != nil {
				t.Fatal(err)
			}
			message := readComposedHeaderTestMessage(t, payload)
			decoded, err := (&mime.WordDecoder{}).DecodeHeader(message.Header.Get("Subject"))
			if err != nil || decoded != test.value {
				t.Fatalf("subject round trip: error=%v equal=%t got=%q", err, decoded == test.value, decoded)
			}
		})
	}
}

func TestComposedDisplayNamesRespectHeaderGrammar(t *testing.T) {
	for _, name := range []string{"Short", strings.Repeat("A", 2000), strings.Repeat("Ä界🦊", 80), strings.Repeat("quote\" slash\\ comma, ", 40), "=?UTF-8?b?YQ==?="} {
		t.Run(fmt.Sprintf("name bytes %d", len(name)), func(t *testing.T) {
			// A quoted raw UTF-8 sender exercises the actual From boundary.
			from := "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(name) + "\" <sender@example.com>"
			payload, err := BuildMessage(Draft{From: from, To: []Recipient{{Name: name, Address: "to@example.com"}}, CC: []Recipient{{Name: name, Address: "cc@example.com"}}, Body: "Body"}, "<names@example.com>")
			if err != nil {
				t.Fatal(err)
			}
			message := readComposedHeaderTestMessage(t, payload)
			for _, header := range []string{"From", "To", "Cc"} {
				addresses, err := message.Header.AddressList(header)
				if err != nil || len(addresses) != 1 || addresses[0].Name != name {
					t.Fatalf("%s round trip: addresses=%+v error=%v", header, addresses, err)
				}
			}
		})
	}
}

func TestComposedStructuredHeadersBoundWithoutReencoding(t *testing.T) {
	for _, length := range []int{996, 997, 998, 2000} {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			id := "<" + strings.Repeat("x", length-len("<@example.com>")) + "@example.com>"
			payload, err := BuildMessage(Draft{From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}}, Body: "Body"}, id)
			if length > 997 {
				if errorCode(err) != "invalid_argument" || !strings.Contains(err.Error(), "Message-ID header") || len(payload) != 0 {
					t.Fatalf("oversized identifier: bytes=%d error=%v", len(payload), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			message := readComposedHeaderTestMessage(t, payload)
			if message.Header.Get("Message-ID") != id {
				t.Fatal("structured identifier changed")
			}
		})
	}
}

func TestComposedMixedAddressListRoundTrips(t *testing.T) {
	recipients := []Recipient{
		{Name: strings.Repeat("\\\"", 25), Address: "quoted@example.com"},
		{Name: "Ünicode", Address: "unicode@example.com"},
		{Name: "  spaced\tname  ", Address: "spaced@example.com"},
	}
	payload, err := BuildMessage(Draft{From: "sender@example.com", To: recipients, Body: "Body"}, "<mixed@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	message := readComposedHeaderTestMessage(t, payload)
	addresses, err := message.Header.AddressList("To")
	if err != nil || len(addresses) != len(recipients) {
		t.Fatalf("mixed address list: %+v, error=%v", addresses, err)
	}
	for index, address := range addresses {
		if address.Name != recipients[index].Name || address.Address != recipients[index].Address {
			t.Fatalf("recipient %d changed: %+v", index, address)
		}
	}
}

func TestComposedThreadHeadersPreserveIdentityAndBounds(t *testing.T) {
	for _, length := range []int{997, 998} {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			id := "<" + strings.Repeat("x", length-len("<@example.com>")) + "@example.com>"
			draft := Draft{From: "sender@example.com", Kind: DraftKindReply, SourceMessageID: id, SourceReferences: "<ancestor@example.com>"}
			payload, err := BuildMessage(draft, "<thread@example.com>")
			if length > 997 {
				if errorCode(err) != "invalid_argument" || !strings.Contains(err.Error(), "In-Reply-To header") || len(payload) != 0 {
					t.Fatalf("oversized parent: bytes=%d error=%v", len(payload), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			message := readComposedHeaderTestMessage(t, payload)
			if message.Header.Get("In-Reply-To") != id || message.Header.Get("References") != "<ancestor@example.com> "+id {
				t.Fatal("folding changed thread identity")
			}
		})
	}
}

func TestComposedHeaderControlsAreRejected(t *testing.T) {
	for _, test := range []struct {
		name, messageID string
		change          func(*Draft)
	}{
		{"identifier injection", "<id@example.com>\r\nBcc: injected@example.com", nil},
		{"sender injection", "<id@example.com>", func(draft *Draft) { draft.From = "sender@example.com\r\nBcc: injected@example.com" }},
		{"invalid UTF-8 subject", "<id@example.com>", func(draft *Draft) { draft.Subject = "bad\xff" }},
		{"invalid UTF-8 name", "<id@example.com>", func(draft *Draft) { draft.To[0].Name = "bad\xff" }},
		{"invalid decoded UTF-8 sender", "<id@example.com>", func(draft *Draft) {
			// Two legal-length encoded words decode to 90 continuation bytes.
			draft.From = strings.Repeat("=?UTF-8?b?"+strings.Repeat("gICA", 15)+"?= ", 2) + "<sender@example.com>"
		}},
		{"parent injection", "<id@example.com>", func(draft *Draft) {
			draft.Kind = DraftKindReply
			draft.SourceMessageID = "<parent@example.com>\r\nBcc: injected@example.com"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			draft := Draft{From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}}}
			if test.change != nil {
				test.change(&draft)
			}
			payload, err := BuildMessage(draft, test.messageID)
			if err == nil || len(payload) != 0 {
				t.Fatalf("unsafe header composed: bytes=%d error=%v", len(payload), err)
			}
		})
	}
}

func TestComposedAddressLiteralEncodingMarker(t *testing.T) {
	address := strings.Repeat("a", 80) + "=?literal@example.com"
	payload, err := BuildMessage(Draft{From: address, To: []Recipient{{Address: address}}, Body: "Body"}, "<literal@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	message := readComposedHeaderTestMessage(t, payload)
	for _, name := range []string{"From", "To"} {
		addresses, err := message.Header.AddressList(name)
		if err != nil || len(addresses) != 1 || addresses[0].Address != address {
			t.Fatalf("literal marker changed %s address: %+v, error=%v", name, addresses, err)
		}
	}
}

func TestComposerPreservesQuotedPairsAndWhitespace(t *testing.T) {
	value := "\"" + strings.Repeat("quoted\\ ", 9) + "end\" <first@example.com>,\t \"second\" <second@example.com>"
	var buffer bytes.Buffer
	if err := writeHeader(&buffer, "To", value); err != nil {
		t.Fatal(err)
	}
	unfolded := strings.ReplaceAll(buffer.String(), composerCRLF, "")
	if unfolded != "To: "+value {
		t.Fatalf("folding changed value: %q", unfolded)
	}
	buffer.WriteString(composerCRLF)
	message := readComposedHeaderTestMessage(t, buffer.Bytes())
	addresses, err := message.Header.AddressList("To")
	if err != nil || len(addresses) != 2 || addresses[0].Name != strings.Repeat("quoted ", 9)+"end" {
		t.Fatalf("folded quoted pair parsed as %+v, error=%v", addresses, err)
	}
}

func TestComposerAttachmentHeadersStayStructuredAndBounded(t *testing.T) {
	for _, name := range []string{"résumé final.txt", strings.Repeat("界", 80) + ".txt", strings.Repeat("x", 1200) + ".txt"} {
		t.Run(fmt.Sprint(len(name)), func(t *testing.T) {
			headers, err := composerAttachmentHeaders(filepath.Join("/attachments", name))
			if len(name) > 998 {
				if errorCode(err) != "invalid_argument" || !strings.Contains(err.Error(), "Content-Disposition header") {
					t.Fatalf("oversized MIME parameter: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			message := readComposedHeaderTestMessage(t, []byte(headers+composerCRLF))
			mediaType, parameters, err := mime.ParseMediaType(message.Header.Get("Content-Disposition"))
			if err != nil || mediaType != "attachment" || parameters["filename"] != name || strings.Contains(headers, "=?UTF-8?") {
				t.Fatalf("MIME parameter changed: %q %+v error=%v", mediaType, parameters, err)
			}
		})
	}
}

func TestInvalidComposedHeadersFailBeforeSMTP(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*DraftInput)
	}{
		{"subject CRLF", func(input *DraftInput) { input.Subject = "safe\r\nBcc: injected@example.com" }},
		{"subject control", func(input *DraftInput) { input.Subject = "unsafe\x00subject" }},
		{"recipient name CRLF", func(input *DraftInput) { input.To[0].Name = "name\r\ninjection" }},
		{"oversized address", func(input *DraftInput) { input.To[0].Address = strings.Repeat("a", 1200) + "@example.com" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			submitter, mirror := sendTransportStubs()
			service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
			input := DraftInput{From: "sender@icloud.com", To: []Recipient{{Address: "recipient@example.com"}}, Body: "Body"}
			test.change(&input)
			draft, err := service.CreateDraft(CreateDraftRequest{Input: input})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, draft.Ref+".json")
			before := readRevisionTestFile(t, path)
			_, err = service.SendDraft(context.Background(), SendDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision})
			if errorCode(err) != "invalid_argument" || submitter.calls != 0 || mirror.calls != 0 {
				t.Fatalf("invalid header reached transport: error=%v calls=%d/%d", err, submitter.calls, mirror.calls)
			}
			if !bytes.Equal(before, readRevisionTestFile(t, path)) {
				t.Fatal("failed composition changed the reviewed draft")
			}
			assertNoSendClaim(t, root, draft.Ref)
		})
	}
}

func readComposedHeaderTestMessage(t *testing.T, payload []byte) *stdmail.Message {
	t.Helper()
	headers, _, ok := bytes.Cut(payload, []byte(composerCRLF+composerCRLF))
	if !ok {
		t.Fatal("composed message has no header separator")
	}
	field := ""
	encodedField := false
	fieldMaximum := 0
	for _, line := range strings.Split(string(headers), composerCRLF) {
		if strings.TrimSpace(line) == "" || strings.ContainsAny(line, "\r\n") || len(line) > 998 {
			t.Fatalf("invalid physical header line: bytes=%d content=%q", len(line), line)
		}
		if line[0] != ' ' && line[0] != '\t' {
			field, _, _ = strings.Cut(line, ":")
			encodedField = false
			fieldMaximum = 0
		}
		fieldMaximum = max(fieldMaximum, len(line))
		if strings.Contains(line, "=?UTF-8?") {
			encodedField = true
		}
		if encodedField && fieldMaximum > 76 {
			t.Fatalf("encoded %s field contains a %d-byte line", field, fieldMaximum)
		}
		for _, word := range strings.Fields(line) {
			if !strings.HasPrefix(word, "=?UTF-8?") {
				continue
			}
			decoded, err := (&mime.WordDecoder{}).Decode(word)
			if len(word) > 75 || err != nil || !utf8.ValidString(decoded) {
				t.Fatalf("invalid encoded word: %q error=%v", word, err)
			}
		}
	}
	message, err := stdmail.ReadMessage(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	return message
}
