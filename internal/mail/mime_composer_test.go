package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	stdmail "net/mail"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

const composerGoldenMessageID = "<build-0001@mailcli.local>"

var (
	composerGoldenDatePattern      = regexp.MustCompile(`(?m)^Date: .*$`)
	composerGoldenMessageIDPattern = regexp.MustCompile(`(?m)^Message-ID: .*$`)
	composerGoldenBoundaryPattern  = regexp.MustCompile(`=_[0-9a-f]{32}`)
)

type scriptedAttachmentReader struct {
	chunks [][]byte
	index  int
	bytes  int
}

func (r *scriptedAttachmentReader) Read(buffer []byte) (int, error) {
	if r.index == len(r.chunks) {
		return 0, io.EOF
	}
	chunk := r.chunks[r.index]
	read := copy(buffer, chunk)
	r.bytes += read
	if read == len(chunk) {
		r.index++
	} else {
		r.chunks[r.index] = chunk[read:]
	}
	return read, nil
}

func (r *scriptedAttachmentReader) Close() error { return nil }

func TestBuildMessageGolden(t *testing.T) {
	directory := t.TempDir()
	notes := composerGoldenAttachment(t, directory, "notes.txt", "meeting notes\nline two\n")
	resume := composerGoldenAttachment(t, directory, "résumé final.txt", "curriculum vitae with ünïcode\n")

	tests := []struct {
		name  string
		draft Draft
	}{
		{
			name: "plain-only",
			draft: Draft{
				Kind: DraftKindNew, From: "sender@example.com",
				To:   []Recipient{{Address: "alice@example.com"}},
				Body: "Hello Alice,\n\nHere is the update.\n\nBest,\nSender\n",
			},
		},
		{
			name: "plain-html",
			draft: Draft{
				Kind: DraftKindNew, From: "sender@example.com",
				To: []Recipient{{Address: "alice@example.com"}}, Subject: "Styled update",
				Body: "Hello Alice,\n\nHere is the update.\n", BodyFormat: DraftBodyHTML,
				BodyHTML: "<p>Hello Alice,</p><p>Here is the <strong>update</strong>.</p>",
			},
		},
		{
			name: "attachments",
			draft: Draft{
				Kind: DraftKindNew, From: "sender@example.com",
				To:      []Recipient{{Name: "Alice", Address: "alice@example.com"}},
				Subject: "Documents attached", Body: "Both documents are attached.\n",
				Attachments: []DraftAttachment{notes, resume},
			},
		},
		{
			name: "reply-threading",
			draft: Draft{
				Kind: DraftKindReply, SourceMessageID: "<original-123@example.com>",
				SourceReferences: "<first-1@example.com> <second-2@example.com>",
				From:             "sender@example.com", To: []Recipient{{Address: "original@example.com"}},
				Subject: "Re: Original subject", Body: "Thanks, understood.\n",
			},
		},
		{
			name: "bcc-excluded",
			draft: Draft{
				Kind: DraftKindNew, From: "sender@example.com",
				To:      []Recipient{{Name: "Alice", Address: "alice@example.com"}},
				CC:      []Recipient{{Name: "Copy", Address: "copy@example.com"}},
				BCC:     []Recipient{{Name: "Secret", Address: "secret@example.com"}},
				Subject: "Visible to To and CC only", Body: "The BCC recipient must never appear.\n",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message, err := BuildMessage(test.draft, composerGoldenMessageID)
			if err != nil {
				t.Fatalf("BuildMessage() error = %v", err)
			}
			messageText := string(message)
			if !strings.HasSuffix(messageText, composerCRLF) ||
				strings.Count(messageText, "\n") != strings.Count(messageText, composerCRLF) {
				t.Fatal("composed message contains a bare LF or lacks its final CRLF")
			}
			got := normalizeComposerGolden(message)
			goldenPath := filepath.Join("testdata", "golden", test.name+".golden")
			golden, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("ReadFile(%q) error = %v", goldenPath, err)
			}
			want := strings.ReplaceAll(string(golden), composerCRLF, "\n")
			if got != want {
				t.Fatalf("message mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}

func composerGoldenAttachment(t *testing.T, directory, name, content string) DraftAttachment {
	t.Helper()
	path := filepath.Join(directory, name)
	payload := []byte(content)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
	digest := sha256.Sum256(payload)
	return DraftAttachment{Path: path, Size: int64(len(payload)), SHA256: hex.EncodeToString(digest[:])}
}

func normalizeComposerGolden(message []byte) string {
	normalized := composerGoldenDatePattern.ReplaceAllString(string(message), "Date: <DATE>")
	normalized = composerGoldenMessageIDPattern.ReplaceAllString(normalized, "Message-ID: <MESSAGE-ID>")
	normalized = composerGoldenBoundaryPattern.ReplaceAllString(normalized, "<BOUNDARY>")
	return strings.ReplaceAll(normalized, composerCRLF, "\n")
}

func TestComposeMessageSpoolContextStopsCanceledWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ComposeMessageSpoolContext(ctx, Draft{}, "<canceled@example.com>")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ComposeMessageSpoolContext() error = %v, want context.Canceled", err)
	}
}

func TestComposeMessageSpoolReadsAttachmentOnceAndChecksFinalHash(t *testing.T) {
	payload := []byte("final attachment bytes")
	sum := sha256.Sum256(payload)
	reader := &scriptedAttachmentReader{chunks: [][]byte{payload}}
	opens := 0
	draft := Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Attachment", Body: "Body", Attachments: []DraftAttachment{{
			Path: "brief.txt", Size: int64(len(payload)), SHA256: hex.EncodeToString(sum[:]),
		}},
	}
	message, err := composeMessageSpoolContext(context.Background(), draft, "<once@example.com>", func(string) (io.ReadCloser, error) {
		opens++
		return reader, nil
	})
	if err != nil {
		t.Fatalf("composeMessageSpoolContext() error = %v", err)
	}
	defer func() { _ = message.Remove() }()
	if opens != 1 || reader.bytes != len(payload) {
		t.Fatalf("attachment reads = opens %d, bytes %d; want one open and %d bytes", opens, reader.bytes, len(payload))
	}
}

func TestComposeMessageSpoolRejectsMutationDuringFinalRead(t *testing.T) {
	first := []byte("original-")
	originalTail := []byte("content")
	changedTail := []byte("CHANGED")
	original := append(append([]byte(nil), first...), originalTail...)
	sum := sha256.Sum256(original)
	reader := &scriptedAttachmentReader{chunks: [][]byte{first, changedTail}}
	opens := 0
	draft := Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Attachment", Body: "Body", Attachments: []DraftAttachment{{
			Path: "brief.txt", Size: int64(len(original)), SHA256: hex.EncodeToString(sum[:]),
		}},
	}
	_, err := composeMessageSpoolContext(context.Background(), draft, "<changed@example.com>", func(string) (io.ReadCloser, error) {
		opens++
		return reader, nil
	})
	if err == nil || !strings.Contains(err.Error(), "changed after review") {
		t.Fatalf("composeMessageSpoolContext() error = %v, want mutation rejection", err)
	}
	if opens != 1 || reader.bytes != len(original) {
		t.Fatalf("mutation read = opens %d, bytes %d; want one open and %d bytes", opens, reader.bytes, len(original))
	}
}

func TestComposerErrorErrorWithInner(t *testing.T) {
	inner := errors.New("disk full")
	err := &ComposerError{Message: "write attachment", Err: inner}
	got := err.Error()
	want := "write attachment: disk full"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestComposerErrorErrorWithoutInner(t *testing.T) {
	err := &ComposerError{Message: "message id is required"}
	got := err.Error()
	if got != "message id is required" {
		t.Errorf("Error() = %q, want message id is required", got)
	}
}

func TestComposerErrorUnwrap(t *testing.T) {
	inner := errors.New("inner error")
	err := &ComposerError{Message: "outer", Err: inner}
	if unwrapped := err.Unwrap(); unwrapped != inner {
		t.Errorf("Unwrap() = %v, want %v", unwrapped, inner)
	}
}

func TestComposerErrorUnwrapNil(t *testing.T) {
	err := &ComposerError{Message: "no inner"}
	if unwrapped := err.Unwrap(); unwrapped != nil {
		t.Errorf("Unwrap() = %v, want nil", unwrapped)
	}
}

func TestThreadReferencesEmpty(t *testing.T) {
	got := threadReferences("", "<msg-123@example.com>")
	if got != "<msg-123@example.com>" {
		t.Errorf("threadReferences(empty) = %q, want source message ID only", got)
	}
}

func TestThreadReferencesWithPrior(t *testing.T) {
	prior := "<msg-1@example.com> <msg-2@example.com>"
	got := threadReferences(prior, "<msg-3@example.com>")
	want := "<msg-1@example.com> <msg-2@example.com> <msg-3@example.com>"
	if got != want {
		t.Errorf("threadReferences(with prior) = %q, want %q", got, want)
	}
}

func TestThreadReferencesWithWhitespace(t *testing.T) {
	got := threadReferences("  <msg-1@example.com>  ", "<msg-2@example.com>")
	want := "<msg-1@example.com> <msg-2@example.com>"
	if got != want {
		t.Errorf("threadReferences(whitespace) = %q, want %q", got, want)
	}
}

func TestThreadReferencesDeduplicatesAndMovesParentToEnd(t *testing.T) {
	prior := "<msg-1@example.com> <msg-2@example.com> <msg-1@example.com> <msg-3@example.com>"
	got := threadReferences(prior, "<msg-2@example.com>")
	want := "<msg-1@example.com> <msg-3@example.com> <msg-2@example.com>"
	if got != want {
		t.Errorf("threadReferences(duplicate parent) = %q, want %q", got, want)
	}
}

func TestBuildMessageRejectsMalformedThreadReferences(t *testing.T) {
	draft := Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Re: Original", Body: "Reply body", Kind: DraftKindReply,
		SourceMessageID: "<original@example.com>", SourceReferences: "<valid@example.com> malformed",
	}
	_, err := BuildMessage(draft, "<reply@example.com>")
	if errorCode(err) != "invalid_message_source" {
		t.Fatalf("BuildMessage() error = %v, want invalid_message_source", err)
	}
}

func TestBuildMessageLeavesMissingThreadSourceUnthreaded(t *testing.T) {
	draft := Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Reply", Body: "Reply body", Kind: DraftKindReply,
		SourceReferences: "<historical@example.com>",
	}
	message, err := BuildMessage(draft, "<reply@example.com>")
	if err != nil {
		t.Fatalf("BuildMessage() error = %v", err)
	}
	parsed, err := stdmail.ReadMessage(strings.NewReader(string(message)))
	if err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}
	if parsed.Header.Get("In-Reply-To") != "" || parsed.Header.Get("References") != "" {
		t.Fatalf("missing source ID produced threading headers: In-Reply-To=%q References=%q", parsed.Header.Get("In-Reply-To"), parsed.Header.Get("References"))
	}
}

func TestBuildMessageUsesVerifiedSpoolForAttachments(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "invoice.pdf")
	payload := []byte("invoice-bytes")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	sum := sha256.Sum256(payload)
	draft := Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Attachment", Body: "Body",
		Attachments: []DraftAttachment{{Path: path, Size: int64(len(payload)), SHA256: hex.EncodeToString(sum[:])}},
	}
	message, err := BuildMessage(draft, "<test@example.com>")
	if err != nil {
		t.Fatalf("BuildMessage() error = %v", err)
	}
	messageText := string(message)
	if !contains(messageText, "Content-Disposition: attachment; filename=invoice.pdf") {
		t.Fatal("verified composition missing attachment disposition")
	}
	if !contains(messageText, "aW52b2ljZS1ieXRlcw==") {
		t.Fatal("verified composition missing attachment payload")
	}
}

func TestBuildMessageRejectsSameSizeAttachmentReplacement(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "reviewed.txt")
	original := []byte("original payload")
	replacement := []byte("changed payload!")
	if len(original) != len(replacement) {
		t.Fatalf("test payload sizes differ: %d != %d", len(original), len(replacement))
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("write original attachment: %v", err)
	}
	sum := sha256.Sum256(original)
	draft := Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Attachment", Body: "Body",
		Attachments: []DraftAttachment{{Path: path, Size: int64(len(original)), SHA256: hex.EncodeToString(sum[:])}},
	}
	if err := os.WriteFile(path, replacement, 0o600); err != nil {
		t.Fatalf("replace attachment: %v", err)
	}
	_, err := BuildMessage(draft, "<replacement@example.com>")
	if err == nil || !strings.Contains(err.Error(), "changed after review") {
		t.Fatalf("BuildMessage() error = %v, want same-size replacement rejection", err)
	}
}

func TestBuildMessageWithHTMLBody(t *testing.T) {
	draft := Draft{
		From:       "sender@example.com",
		To:         []Recipient{{Address: "recipient@example.com"}},
		Subject:    "HTML Test",
		Body:       "Plain text",
		BodyHTML:   "<p>HTML text</p>",
		BodyFormat: DraftBodyHTML,
	}
	msg, err := BuildMessage(draft, "<test@example.com>")
	if err != nil {
		t.Fatalf("BuildMessage error = %v", err)
	}
	msgStr := string(msg)
	if !contains(msgStr, "text/html") {
		t.Error("message missing text/html content type")
	}
	if !contains(msgStr, "text/plain") {
		t.Error("message missing text/plain content type")
	}
}

func TestBuildMessageReplyWithThreading(t *testing.T) {
	draft := Draft{
		From:             "sender@example.com",
		To:               []Recipient{{Address: "recipient@example.com"}},
		Subject:          "Re: Original",
		Body:             "Reply body",
		Kind:             DraftKindReply,
		SourceMessageID:  "<original@example.com>",
		SourceReferences: "<msg-1@example.com>",
	}
	msg, err := BuildMessage(draft, "<reply@example.com>")
	if err != nil {
		t.Fatalf("BuildMessage error = %v", err)
	}
	msgStr := string(msg)
	if !contains(msgStr, "In-Reply-To: <original@example.com>") {
		t.Error("message missing In-Reply-To header")
	}
	if !contains(msgStr, "References: <msg-1@example.com> <original@example.com>") {
		t.Error("message missing References header")
	}
}

func TestComposeMessageSpoolCanBeRemovedAfterReplay(t *testing.T) {
	draft := Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Spool", Body: "Body",
	}
	message, err := ComposeMessageSpool(draft, "<spool@example.com>")
	if err != nil {
		t.Fatalf("ComposeMessageSpool() error = %v", err)
	}
	reader, err := message.Open()
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if message.Size() <= 0 || message.MessageID() != "<spool@example.com>" {
		t.Fatalf("message metadata = size %d, id %q", message.Size(), message.MessageID())
	}
	if err := message.Remove(); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := message.Open(); err == nil {
		t.Fatal("Open() after Remove() succeeded")
	}
}

func TestComposeMessageSpoolPinsContentAndProtectsReplacement(t *testing.T) {
	draft := Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Pinned spool", Body: "Original body",
	}
	message, err := ComposeMessageSpool(draft, "<pinned@example.com>")
	if err != nil {
		t.Fatalf("ComposeMessageSpool() error = %v", err)
	}
	path := message.path
	movedPath := path + ".moved"
	t.Cleanup(func() {
		_ = os.Remove(path)
		_ = os.Remove(movedPath)
		_ = message.Remove()
	})

	initialReader, err := message.Open()
	if err != nil {
		t.Fatalf("Open(initial) error = %v", err)
	}
	original, readErr := io.ReadAll(initialReader)
	if closeErr := initialReader.Close(); readErr != nil || closeErr != nil {
		t.Fatalf("initial read/close = %v/%v", readErr, closeErr)
	}
	if err := os.Rename(path, movedPath); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	replacement := []byte("replacement bytes")
	if err := os.WriteFile(path, replacement, 0o600); err != nil {
		t.Fatalf("WriteFile(replacement) error = %v", err)
	}

	replacementReader, err := message.Open()
	if err != nil {
		t.Fatalf("Open(after replacement) error = %v", err)
	}
	got, readErr := io.ReadAll(replacementReader)
	closeErr := replacementReader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("replacement read/close = %v/%v", readErr, closeErr)
	}
	if string(got) != string(original) {
		t.Fatalf("replacement path changed composed bytes")
	}

	if err := message.Remove(); errorCode(err) != "draft_lock_changed" {
		t.Fatalf("Remove() error = %v, want draft_lock_changed", err)
	}
	if message.file != nil {
		t.Fatal("owner descriptor remains open after replacement cleanup")
	}
	retained, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(replacement) error = %v", err)
	}
	if string(retained) != string(replacement) {
		t.Fatalf("replacement path changed to %q", retained)
	}
}

func TestComposeMessageSpoolProvidesIndependentBoundedReaders(t *testing.T) {
	draft := Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Concurrent spool", Body: strings.Repeat("streamed body\n", 16*1024),
	}
	message, err := ComposeMessageSpool(draft, "<concurrent@example.com>")
	if err != nil {
		t.Fatalf("ComposeMessageSpool() error = %v", err)
	}
	t.Cleanup(func() { _ = message.Remove() })

	baselineReader, err := message.Open()
	if err != nil {
		t.Fatalf("Open(baseline) error = %v", err)
	}
	baseline, readErr := io.ReadAll(baselineReader)
	closeErr := baselineReader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("baseline read/close = %v/%v", readErr, closeErr)
	}
	file, err := os.OpenFile(message.path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("OpenFile(append) error = %v", err)
	}
	if _, err := file.WriteString("ignored bytes beyond the composed size"); err != nil {
		_ = file.Close()
		t.Fatalf("WriteString(append) error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close(append) error = %v", err)
	}

	readers := make([]io.ReadCloser, 2)
	for index := range readers {
		readers[index], err = message.Open()
		if err != nil {
			t.Fatalf("Open(%d) error = %v", index, err)
		}
	}
	var (
		payloads   [2][]byte
		readErrors [2]error
		wait       sync.WaitGroup
		start      = make(chan struct{})
	)
	wait.Add(len(readers))
	for index, reader := range readers {
		go func(index int, reader io.ReadCloser) {
			defer wait.Done()
			<-start
			payloads[index], readErrors[index] = io.ReadAll(reader)
			readErrors[index] = errors.Join(readErrors[index], reader.Close())
		}(index, reader)
	}
	close(start)
	wait.Wait()
	for index := range payloads {
		if readErrors[index] != nil {
			t.Fatalf("reader %d error = %v", index, readErrors[index])
		}
		if len(payloads[index]) != len(baseline) || len(payloads[index]) != int(message.Size()) {
			t.Fatalf("reader %d length = %d, baseline %d, message size %d", index, len(payloads[index]), len(baseline), message.Size())
		}
		if string(payloads[index]) != string(baseline) {
			t.Fatalf("reader %d bytes differ from baseline", index)
		}
	}
	if err := message.Remove(); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if message.file != nil {
		t.Fatal("owner descriptor remains open after all readers closed")
	}
}

func TestComposeMessageSpoolRetainsOwnerUntilLastReaderCloses(t *testing.T) {
	message, err := ComposeMessageSpool(Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Reader lifetime", Body: strings.Repeat("retained content\n", 1024),
	}, "<lifetime@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = message.Remove() })
	owner := message.file
	original, err := os.ReadFile(message.path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := message.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := message.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	prefix := make([]byte, 97)
	if _, err := io.ReadFull(first, prefix); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (contextReader{ctx: ctx, reader: first}).Read(prefix); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled consumer error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("second Close() = %v", err)
	}
	if _, err := first.Read(prefix); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Read after Close() = %v", err)
	}
	if err := message.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(message.path); !os.IsNotExist(err) {
		t.Fatalf("spool pathname after Remove() = %v", err)
	}
	if _, err := message.Open(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Open after Remove() = %v", err)
	}
	if _, err := owner.Stat(); err != nil {
		t.Fatalf("owner closed while a consumer remains: %v", err)
	}
	remaining, err := io.ReadAll(second)
	if err != nil || string(remaining) != string(original) {
		t.Fatalf("surviving reader: bytes equal %t, error %v", string(remaining) == string(original), err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("owner after last consumer Close() = %v", err)
	}
	if err := message.Remove(); err != nil {
		t.Fatalf("repeated Remove() = %v", err)
	}
}

func TestComposeMessageSpoolReportsShortRead(t *testing.T) {
	message, err := ComposeMessageSpool(Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Short read", Body: "Body",
	}, "<short-read@example.com>")
	if err != nil {
		t.Fatalf("ComposeMessageSpool() error = %v", err)
	}
	t.Cleanup(func() { _ = message.Remove() })
	file, err := os.OpenFile(message.path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatalf("OpenFile(truncate) error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close(truncate) error = %v", err)
	}
	reader, err := message.Open()
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	_, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if !errors.Is(readErr, io.ErrUnexpectedEOF) || closeErr != nil {
		t.Fatalf("short read/close = %v/%v, want io.ErrUnexpectedEOF and nil", readErr, closeErr)
	}
}

func TestPersistAcceptedMessageSpoolUsesPinnedComposition(t *testing.T) {
	message, err := ComposeMessageSpool(Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Recovery spool", Body: "Accepted bytes",
	}, "<recovery@example.com>")
	if err != nil {
		t.Fatalf("ComposeMessageSpool() error = %v", err)
	}
	path := message.path
	movedPath := path + ".moved"
	t.Cleanup(func() {
		_ = os.Remove(path)
		_ = os.Remove(movedPath)
		_ = message.Remove()
	})
	reader, err := message.Open()
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	original, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close = %v/%v", readErr, closeErr)
	}
	if err := os.Rename(path, movedPath); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	if err := os.WriteFile(path, []byte("replacement bytes"), 0o600); err != nil {
		t.Fatalf("WriteFile(replacement) error = %v", err)
	}

	root := t.TempDir()
	ref := "draft_123456789012345678901234"
	pinnedRoot, err := os.OpenRoot(root)
	if err != nil {
		t.Fatalf("OpenRoot() error = %v", err)
	}
	directory, err := os.Open(root)
	if err != nil {
		_ = pinnedRoot.Close()
		t.Fatalf("Open(directory) error = %v", err)
	}
	t.Cleanup(func() {
		_ = pinnedRoot.Close()
		_ = directory.Close()
	})
	state := &draftStorage{rootName: root, root: pinnedRoot, directory: directory}
	retained, err := persistAcceptedMessageSpool(root, ref, message, state)
	if err != nil {
		t.Fatalf("persistAcceptedMessageSpool() error = %v", err)
	}
	spoolPath, err := acceptedMessageSpoolPath(root, ref)
	if err != nil {
		t.Fatalf("acceptedMessageSpoolPath() error = %v", err)
	}
	persisted, err := os.ReadFile(spoolPath)
	if err != nil {
		t.Fatalf("ReadFile(recovery spool) error = %v", err)
	}
	if string(persisted) != string(original) || retained.Size != int64(len(original)) {
		t.Fatalf("recovery spool did not preserve pinned bytes: size %d, bytes_equal %t", retained.Size, string(persisted) == string(original))
	}
}

func TestBuildMessageForwardWithThreading(t *testing.T) {
	draft := Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Fwd: Original", Body: "Forward body", Kind: DraftKindForward,
		SourceMessageID: "<original@example.com>", SourceReferences: "<msg-1@example.com>",
	}
	msg, err := BuildMessage(draft, "<forward@example.com>")
	if err != nil {
		t.Fatalf("BuildMessage error = %v", err)
	}
	msgStr := string(msg)
	if !contains(msgStr, "In-Reply-To: <original@example.com>") ||
		!contains(msgStr, "References: <msg-1@example.com> <original@example.com>") {
		t.Fatalf("forward message missing threading headers: %s", msgStr)
	}
}

func TestBuildMessageEmptyMessageID(t *testing.T) {
	draft := Draft{From: "sender@example.com", To: []Recipient{{Address: "r@example.com"}}}
	_, err := BuildMessage(draft, "")
	if err == nil {
		t.Fatal("BuildMessage error = nil, want message id required")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
