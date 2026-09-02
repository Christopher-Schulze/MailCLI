package cli

import (
	"bytes"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

func TestParseReleasePublicKeyValid(t *testing.T) {
	key, err := parseReleasePublicKey("VjVSufeZlmmMshZYeMB9u1xKoMvRavstpFqByv8Vzqg=")
	if err != nil {
		t.Fatalf("parseReleasePublicKey error = %v", err)
	}
	if len(key) != 32 {
		t.Errorf("key length = %d, want 32", len(key))
	}
}

func TestParseReleasePublicKeyInvalidBase64(t *testing.T) {
	_, err := parseReleasePublicKey("!!!invalid!!!")
	if err == nil {
		t.Fatal("parseReleasePublicKey error = nil, want invalid base64 error")
	}
}

func TestParseReleasePublicKeyWrongLength(t *testing.T) {
	short := "dG9vIHNob3J0" // "too short" in base64
	_, err := parseReleasePublicKey(short)
	if err == nil {
		t.Fatal("parseReleasePublicKey error = nil, want wrong length error")
	}
}

func TestChecksumForArchiveValid(t *testing.T) {
	digestHex := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	checksums := digestHex + "  mailcli_1.2.0_darwin_arm64.tar.gz\n"
	digest, err := checksumForArchive("mailcli_1.2.0_darwin_arm64.tar.gz", checksums)
	if err != nil {
		t.Fatalf("checksumForArchive error = %v", err)
	}
	if len(digest) != 32 {
		t.Errorf("digest length = %d, want 32 (SHA-256)", len(digest))
	}
}

func TestChecksumForArchiveMissing(t *testing.T) {
	checksums := "abc123  other_archive.tar.gz\n"
	_, err := checksumForArchive("mailcli_1.2.0_darwin_arm64.tar.gz", checksums)
	if err == nil {
		t.Fatal("checksumForArchive error = nil, want missing archive error")
	}
}

func TestChecksumForArchiveDuplicate(t *testing.T) {
	checksums := "abc123  archive.tar.gz\ndef456  archive.tar.gz\n"
	_, err := checksumForArchive("archive.tar.gz", checksums)
	if err == nil {
		t.Fatal("checksumForArchive error = nil, want duplicate entries error")
	}
}

func TestChecksumForArchiveInvalidDigest(t *testing.T) {
	checksums := "nothex  archive.tar.gz\n"
	_, err := checksumForArchive("archive.tar.gz", checksums)
	if err == nil {
		t.Fatal("checksumForArchive error = nil, want invalid digest error")
	}
}

func TestChecksumForArchiveWithAsteriskPrefix(t *testing.T) {
	digestHex := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	checksums := digestHex + " *archive.tar.gz\n"
	_, err := checksumForArchive("archive.tar.gz", checksums)
	if err != nil {
		t.Fatalf("checksumForArchive error = %v", err)
	}
}

func TestMakeDraftPreviewPlain(t *testing.T) {
	draft := mail.Draft{
		Ref:        "draft-1",
		From:       "sender@example.com",
		Subject:    "Test",
		Body:       "Hello world",
		BodyFormat: mail.DraftBodyPlain,
	}
	preview, err := makeDraftPreview(draft, "plain")
	if err != nil {
		t.Fatalf("makeDraftPreview error = %v", err)
	}
	if preview.Body != "Hello world" {
		t.Errorf("Body = %q, want Hello world", preview.Body)
	}
	if preview.View != "plain" {
		t.Errorf("View = %q, want plain", preview.View)
	}
}

func TestMakeDraftPreviewHTMLFormat(t *testing.T) {
	draft := mail.Draft{
		Ref:        "draft-1",
		Body:       "Plain content",
		BodyHTML:   "<p>HTML content</p>",
		BodyFormat: mail.DraftBodyHTML,
	}
	preview, err := makeDraftPreview(draft, "html")
	if err != nil {
		t.Fatalf("makeDraftPreview error = %v", err)
	}
	if preview.Body != "<p>HTML content</p>" {
		t.Errorf("Body = %q, want HTML content", preview.Body)
	}
}

func TestMakeDraftPreviewPlainFormatHTMLView(t *testing.T) {
	draft := mail.Draft{
		Body:       "Plain text with <html>",
		BodyFormat: mail.DraftBodyPlain,
	}
	preview, err := makeDraftPreview(draft, "html")
	if err != nil {
		t.Fatalf("makeDraftPreview error = %v", err)
	}
	want := "<pre>Plain text with &lt;html&gt;</pre>"
	if preview.Body != want {
		t.Errorf("Body = %q, want %q", preview.Body, want)
	}
}

func TestMakeDraftPreviewSourceView(t *testing.T) {
	draft := mail.Draft{
		Body:       "Rendered body",
		BodySource: "Raw source body",
		BodyFormat: mail.DraftBodyMarkdown,
	}
	preview, err := makeDraftPreview(draft, "source")
	if err != nil {
		t.Fatalf("makeDraftPreview error = %v", err)
	}
	if preview.Body != "Raw source body" {
		t.Errorf("Body = %q, want Raw source body", preview.Body)
	}
}

func TestMakeDraftPreviewSourceViewPlainFormat(t *testing.T) {
	draft := mail.Draft{
		Body:       "Same body",
		BodyFormat: mail.DraftBodyPlain,
	}
	preview, err := makeDraftPreview(draft, "source")
	if err != nil {
		t.Fatalf("makeDraftPreview error = %v", err)
	}
	if preview.Body != "Same body" {
		t.Errorf("Body = %q, want Same body (plain has no separate source)", preview.Body)
	}
}

func TestMakeDraftPreviewInvalidView(t *testing.T) {
	draft := mail.Draft{}
	_, err := makeDraftPreview(draft, "invalid")
	if err == nil {
		t.Fatal("makeDraftPreview error = nil, want invalid view error")
	}
}

func TestWriteHumanDraftPreviewBasic(t *testing.T) {
	preview := draftPreview{
		From:       "sender@example.com",
		To:         []mail.Recipient{{Name: "Recipient", Address: "recipient@example.com"}},
		Subject:    "Test Subject",
		BodyFormat: mail.DraftBodyPlain,
		View:       "plain",
		Body:       "Hello world",
	}
	var buf bytes.Buffer
	writeHumanDraftPreview(&buf, preview)
	output := buf.String()
	if !strings.Contains(output, "From: sender@example.com") {
		t.Errorf("output missing From: %q", output)
	}
	if !strings.Contains(output, "Subject: Test Subject") {
		t.Errorf("output missing Subject: %q", output)
	}
	if !strings.Contains(output, "Hello world") {
		t.Errorf("output missing body: %q", output)
	}
}

func TestWriteHumanDraftPreviewWithAttachments(t *testing.T) {
	preview := draftPreview{
		From:       "sender@example.com",
		To:         []mail.Recipient{{Address: "recipient@example.com"}},
		Subject:    "Test",
		BodyFormat: mail.DraftBodyPlain,
		View:       "plain",
		Body:       "Body",
		Attachments: []mail.DraftAttachment{
			{Path: "/tmp/report.pdf", Size: 1024},
		},
	}
	var buf bytes.Buffer
	writeHumanDraftPreview(&buf, preview)
	output := buf.String()
	if !strings.Contains(output, "Attachments:") {
		t.Errorf("output missing Attachments section: %q", output)
	}
	if !strings.Contains(output, "/tmp/report.pdf") {
		t.Errorf("output missing attachment path: %q", output)
	}
	if !strings.Contains(output, "1024 bytes") {
		t.Errorf("output missing attachment size: %q", output)
	}
}

func TestWriteHumanDraftPreviewWithCCAndBCC(t *testing.T) {
	preview := draftPreview{
		From:       "sender@example.com",
		To:         []mail.Recipient{{Address: "to@example.com"}},
		CC:         []mail.Recipient{{Address: "cc@example.com"}},
		BCC:        []mail.Recipient{{Address: "bcc@example.com"}},
		Subject:    "Test",
		BodyFormat: mail.DraftBodyPlain,
		View:       "plain",
		Body:       "Body",
	}
	var buf bytes.Buffer
	writeHumanDraftPreview(&buf, preview)
	output := buf.String()
	if !strings.Contains(output, "CC:") {
		t.Errorf("output missing CC: %q", output)
	}
	if !strings.Contains(output, "BCC:") {
		t.Errorf("output missing BCC: %q", output)
	}
}

func TestUpdateErrorError(t *testing.T) {
	err := &updateError{code: "update_failed", message: "something went wrong"}
	got := err.Error()
	want := "something went wrong"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestUpdateErrorErrorCode(t *testing.T) {
	err := &updateError{code: "update_signature_invalid", message: "bad signature"}
	if got := err.ErrorCode(); got != "update_signature_invalid" {
		t.Errorf("ErrorCode() = %q, want update_signature_invalid", got)
	}
}
