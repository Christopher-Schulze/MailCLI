package mailstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

func newExternalAttachmentFixture(
	t testing.TB,
) (*Store, string, resolvedMessage, string) {
	t.Helper()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	messageRef := ""
	for _, message := range page.Messages {
		if message.Subject == "Quarterly Report" {
			messageRef = message.Ref
			break
		}
	}
	if messageRef == "" {
		t.Fatal("ListMessages() did not return the attachment fixture message")
	}
	resolved, source, err := store.openMessageSource(context.Background(), messageRef)
	if err != nil {
		t.Fatalf("openMessageSource() error = %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("close fixture message source: %v", err)
	}
	directory, err := store.attachmentDirectory(resolved, "2")
	if err != nil {
		t.Fatalf("attachmentDirectory() error = %v", err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	return store, messageRef, resolved, directory
}

func writeCompleteInlineAttachmentMessage(t testing.TB, store *Store, resolved resolvedMessage) {
	t.Helper()
	message := []byte(
		"From: Alice <alice@example.com>\r\n" +
			"To: Christopher <christopher@example.com>\r\n" +
			"Subject: Quarterly Report\r\n" +
			"Message-ID: <1001@example.com>\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: multipart/mixed; boundary=inline-attachment\r\n\r\n" +
			"--inline-attachment\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
			"report body\r\n" +
			"--inline-attachment\r\n" +
			"Content-Type: application/pdf; name=\"invoice.pdf\"\r\n" +
			"Content-Disposition: attachment; filename=\"invoice.pdf\"\r\n" +
			"Content-Transfer-Encoding: base64\r\n\r\n" +
			"aW5saW5l\r\n" +
			"--inline-attachment--\r\n",
	)
	base, err := store.messageBasePath(resolved.PhysicalLocation, resolved.Record.RowID)
	if err != nil {
		t.Fatalf("messageBasePath() error = %v", err)
	}
	framed := append([]byte(fmt.Sprintf("%-10d\n", len(message))), message...)
	framed = append(framed, validPlistTrailer()...)
	if err := os.WriteFile(base+".emlx", framed, 0o600); err != nil {
		t.Fatalf("WriteFile(complete inline MIME fixture) error = %v", err)
	}
}

func createExternalAttachmentHardLinks(t testing.TB, store *Store, directory string, count int) {
	t.Helper()
	seed := filepath.Join(store.versionRoot, "external-attachment-seed.bin")
	if err := os.WriteFile(seed, []byte("same external bytes"), 0o600); err != nil {
		t.Fatalf("WriteFile(seed) error = %v", err)
	}
	for index := range count {
		name := fmt.Sprintf("candidate-%05d.bin", index)
		if index == 0 {
			name = "invoice.pdf"
		}
		if err := os.Link(seed, filepath.Join(directory, name)); err != nil {
			t.Fatalf("Link(%s) error = %v", name, err)
		}
	}
}

func createExternalAttachmentCandidates(t testing.TB, directory string, count int, different bool) []byte {
	t.Helper()
	contents := []byte("identical candidate bytes")
	for index := range count {
		name := fmt.Sprintf("candidate-%04d.bin", index)
		candidate := contents
		if different && index == count-1 {
			candidate = []byte("different candidate bytes")
		}
		if err := os.WriteFile(filepath.Join(directory, name), candidate, 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	return contents
}

func TestExternalAttachmentOperationsRejectSelectedFileReplacement(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Store, externalAttachment, string) error
	}{
		{
			name: "hash",
			run: func(store *Store, selected externalAttachment, _ string) error {
				_, err := store.hashStoreFile(selected)
				return err
			},
		},
		{
			name: "copy",
			run: func(store *Store, selected externalAttachment, output string) error {
				return store.copyExternalAttachment(selected, output)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			rootDirectory, err := openVersionDirectory(root)
			if err != nil {
				t.Fatalf("openVersionDirectory() error = %v", err)
			}
			closeTestResource(t, rootDirectory, "test root")
			store := &Store{versionRoot: root, versionDirectory: rootDirectory}
			path := filepath.Join(root, "attachment.bin")
			if err := os.WriteFile(path, []byte("reviewed bytes"), 0o600); err != nil {
				t.Fatalf("write selected attachment: %v", err)
			}
			identity, err := os.Lstat(path)
			if err != nil {
				t.Fatalf("inspect selected attachment: %v", err)
			}
			selected := externalAttachment{Path: path, Size: identity.Size(), identity: identity}
			if err := os.Rename(path, filepath.Join(root, "original.bin")); err != nil {
				t.Fatalf("move selected attachment: %v", err)
			}
			if err := os.WriteFile(path, []byte("replaced bytes"), 0o600); err != nil {
				t.Fatalf("write replacement attachment: %v", err)
			}
			output := filepath.Join(root, "output.bin")
			if err := test.run(store, selected, output); errorCodeForTest(err) != "store_changed" {
				t.Fatalf("operation error = %v, want store_changed", err)
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("output exists after rejected replacement: %v", err)
			}
		})
	}
}

func TestFindExternalAttachmentBoundsDirectoryEntriesAndPinnedStats(t *testing.T) {
	store, _, resolved, directory := newExternalAttachmentFixture(t)
	createExternalAttachmentHardLinks(t, store, directory, maximumExternalAttachmentDirectoryEntries)
	selected, available, err := store.findExternalAttachment(
		resolved, attachmentRecord{ID: "2", Name: "invoice.pdf"},
	)
	if err != nil || !available {
		t.Fatalf("findExternalAttachment() available = %t, error = %v", available, err)
	}
	if selected.budget == nil ||
		selected.budget.directoryEntries != maximumExternalAttachmentDirectoryEntries ||
		selected.budget.pinnedFileStats != maximumPinnedExternalAttachmentFileStats {
		t.Fatalf("discovery work = %+v, want %d entries and %d pinned stats",
			selected.budget, maximumExternalAttachmentDirectoryEntries, maximumPinnedExternalAttachmentFileStats)
	}
}

func TestFindExternalAttachmentBoundsHashedAmbiguityCandidates(t *testing.T) {
	t.Run("exact limit", func(t *testing.T) {
		store, _, resolved, directory := newExternalAttachmentFixture(t)
		contents := createExternalAttachmentCandidates(t, directory, maximumExternalAttachmentHashCandidates, false)
		selected, available, err := store.findExternalAttachment(
			resolved, attachmentRecord{ID: "2", Name: "invoice.pdf"},
		)
		if err != nil || !available {
			t.Fatalf("findExternalAttachment() available = %t, error = %v", available, err)
		}
		if selected.budget == nil ||
			selected.budget.directoryEntries != maximumExternalAttachmentHashCandidates ||
			selected.budget.pinnedFileStats != maximumExternalAttachmentHashCandidates ||
			selected.budget.hashedAmbiguityCandidates != maximumExternalAttachmentHashCandidates ||
			selected.budget.hashBytes != int64(maximumExternalAttachmentHashCandidates*len(contents)) {
			t.Fatalf("discovery work = %+v, want %d entries, stats and hashes of %d bytes each",
				selected.budget, maximumExternalAttachmentHashCandidates, len(contents))
		}
	})

	t.Run("over limit", func(t *testing.T) {
		store, _, resolved, directory := newExternalAttachmentFixture(t)
		createExternalAttachmentCandidates(t, directory, maximumExternalAttachmentHashCandidates+1, false)
		selected, available, err := store.findExternalAttachment(
			resolved, attachmentRecord{ID: "2", Name: "invoice.pdf"},
		)
		if errorCodeForTest(err) != "attachment_resource_limit" || available || selected.Path != "" ||
			!strings.Contains(err.Error(), "hashed ambiguity candidates") {
			t.Fatalf("findExternalAttachment() = %+v, %t, %v; want typed candidate limit", selected, available, err)
		}
	})

	t.Run("different bytes remain ambiguous", func(t *testing.T) {
		store, _, resolved, directory := newExternalAttachmentFixture(t)
		createExternalAttachmentCandidates(t, directory, 2, true)
		selected, available, err := store.findExternalAttachment(
			resolved, attachmentRecord{ID: "2", Name: "invoice.pdf"},
		)
		if errorCodeForTest(err) != "ambiguous_attachment" || available || selected.Path != "" {
			t.Fatalf("findExternalAttachment() = %+v, %t, %v; want ambiguous_attachment", selected, available, err)
		}
	})
}

func TestExternalAttachmentHashBudgetStopsBeforeCumulativeLimit(t *testing.T) {
	budget := &externalAttachmentDiscoveryBudget{}
	if err := budget.reserveHashBytes(maximumExternalAttachmentHashBytes); err != nil {
		t.Fatalf("reserveHashBytes(exact limit) error = %v", err)
	}
	if budget.hashBytes != maximumExternalAttachmentHashBytes {
		t.Fatalf("hashBytes = %d, want exact %d-byte boundary", budget.hashBytes, maximumExternalAttachmentHashBytes)
	}
	if err := budget.reserveHashBytes(1); errorCodeForTest(err) != "attachment_resource_limit" {
		t.Fatalf("reserveHashBytes(over limit) error = %v, want attachment_resource_limit", err)
	}
	if budget.hashBytes != maximumExternalAttachmentHashBytes {
		t.Fatalf("hashBytes after rejected reservation = %d, want unchanged %d",
			budget.hashBytes, maximumExternalAttachmentHashBytes)
	}

	store, _, resolved, directory := newExternalAttachmentFixture(t)
	if err := os.WriteFile(filepath.Join(directory, "a-small.bin"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile(small candidate) error = %v", err)
	}
	largePath := filepath.Join(directory, "b-large.bin")
	largeFile, err := os.OpenFile(largePath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("OpenFile(large candidate) error = %v", err)
	}
	if err := largeFile.Truncate(maximumExternalAttachmentHashBytes); err != nil {
		_ = largeFile.Close()
		t.Fatalf("Truncate(large candidate) error = %v", err)
	}
	if err := largeFile.Close(); err != nil {
		t.Fatalf("Close(large candidate) error = %v", err)
	}
	selected, available, err := store.findExternalAttachment(
		resolved, attachmentRecord{ID: "2", Name: "invoice.pdf"},
	)
	if errorCodeForTest(err) != "attachment_resource_limit" || available || selected.Path != "" ||
		!strings.Contains(err.Error(), "cumulative hash bytes") {
		t.Fatalf("findExternalAttachment() = %+v, %t, %v; want cumulative hash limit", selected, available, err)
	}
}

func TestWriteVerifiedExclusiveFileCopiesAndVerifiesBytes(t *testing.T) {
	t.Parallel()
	source := []byte("verified attachment bytes")
	output := filepath.Join(t.TempDir(), "attachment.bin")
	if err := writeVerifiedExclusiveFile(output, bytes.NewReader(source), int64(len(source)), sha256.Sum256(source)); err != nil {
		t.Fatalf("writeVerifiedExclusiveFile() error = %v", err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.Equal(got, source) {
		t.Fatalf("output bytes = %q, want %q", got, source)
	}
}

func TestCopyExternalAttachmentCopiesAndVerifiesBytes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	rootDirectory, err := openVersionDirectory(root)
	if err != nil {
		t.Fatalf("openVersionDirectory() error = %v", err)
	}
	closeTestResource(t, rootDirectory, "test root")
	store := &Store{versionRoot: root, versionDirectory: rootDirectory}
	source := []byte("external attachment bytes")
	path := filepath.Join(root, "attachment.bin")
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatalf("WriteFile() source error = %v", err)
	}
	identity, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat() source error = %v", err)
	}
	output := filepath.Join(root, "output.bin")
	selected := externalAttachment{Path: path, Size: identity.Size(), identity: identity}
	if err := store.copyExternalAttachment(selected, output); err != nil {
		t.Fatalf("copyExternalAttachment() error = %v", err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("ReadFile() output error = %v", err)
	}
	if !bytes.Equal(got, source) {
		t.Fatalf("output bytes = %q, want %q", got, source)
	}
}

func TestCopyExternalAttachmentReturnsVerifiedEvidence(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	rootDirectory, err := openVersionDirectory(root)
	if err != nil {
		t.Fatalf("openVersionDirectory() error = %v", err)
	}
	closeTestResource(t, rootDirectory, "test root")
	store := &Store{versionRoot: root, versionDirectory: rootDirectory}
	source := []byte("external attachment evidence")
	path := filepath.Join(root, "attachment.bin")
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatalf("WriteFile() source error = %v", err)
	}
	identity, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat() source error = %v", err)
	}
	output := filepath.Join(root, "output.bin")
	evidence, err := store.copyExternalAttachmentWithEvidence(
		externalAttachment{Path: path, Size: identity.Size(), identity: identity}, output,
	)
	if err != nil {
		t.Fatalf("copyExternalAttachmentWithEvidence() error = %v", err)
	}
	digest := sha256.Sum256(source)
	outputInfo, err := os.Lstat(output)
	if err != nil {
		t.Fatalf("Lstat() output error = %v", err)
	}
	if evidence.Path != output || evidence.Size != int64(len(source)) ||
		evidence.SHA256 != hex.EncodeToString(digest[:]) ||
		evidence.Identity == nil || evidence.Identity.Size() != int64(len(source)) ||
		!os.SameFile(outputInfo, evidence.Identity) {
		t.Fatalf("evidence = %+v, want path %q, size %d, sha256 %s", evidence, output, len(source), hex.EncodeToString(digest[:]))
	}
}

func TestWriteVerifiedExclusiveFileRejectsSameSizeSourceMutation(t *testing.T) {
	t.Parallel()
	expected := []byte("reviewed bytes")
	mutated := []byte("replaced bytes")
	output := filepath.Join(t.TempDir(), "attachment.bin")
	err := writeVerifiedExclusiveFile(output, bytes.NewReader(mutated), int64(len(expected)), sha256.Sum256(expected))
	if errorCodeForTest(err) != "store_changed" {
		t.Fatalf("writeVerifiedExclusiveFile() error = %v, want store_changed", err)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("output exists after rejected source mutation: %v", err)
	}
}

func TestWriteVerifiedExclusiveFileRejectsShortCopy(t *testing.T) {
	t.Parallel()
	expected := []byte("complete attachment")
	output := filepath.Join(t.TempDir(), "attachment.bin")
	err := writeVerifiedExclusiveFile(output, bytes.NewReader([]byte("short")), int64(len(expected)), sha256.Sum256(expected))
	if errorCodeForTest(err) != "store_changed" {
		t.Fatalf("writeVerifiedExclusiveFile() error = %v, want store_changed", err)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("output exists after rejected short copy: %v", err)
	}
}

func TestWriteVerifiedExclusiveFileSupportsNestedOutputDir(t *testing.T) {
	t.Parallel()
	parent := filepath.Join(t.TempDir(), "nested", "deeper")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	source := []byte("nested attachment bytes")
	output := filepath.Join(parent, "attachment.bin")
	if err := writeVerifiedExclusiveFile(output, bytes.NewReader(source), int64(len(source)), sha256.Sum256(source)); err != nil {
		t.Fatalf("writeVerifiedExclusiveFile() error = %v", err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.Equal(got, source) {
		t.Fatalf("output bytes = %q, want %q", got, source)
	}
}

func TestWriteVerifiedExclusiveFileRejectsSymlinkedParent(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(base, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	source := []byte("attachment bytes")
	output := filepath.Join(link, "attachment.bin")
	err := writeVerifiedExclusiveFile(output, bytes.NewReader(source), int64(len(source)), sha256.Sum256(source))
	if errorCodeForTest(err) != "unsafe_message_source" {
		t.Fatalf("writeVerifiedExclusiveFile() error = %v, want unsafe_message_source", err)
	}
	if _, statErr := os.Lstat(output); !os.IsNotExist(statErr) {
		t.Fatalf("output exists after rejected symlinked parent: %v", statErr)
	}
	entries, readErr := os.ReadDir(target)
	if readErr != nil {
		t.Fatalf("ReadDir() error = %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("symlinked parent target received %d entries, want none", len(entries))
	}
}

func TestWriteVerifiedExclusiveFileRejectsParentSwap(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	moved := parent + ".moved"
	source := []byte("verified bytes")
	output := filepath.Join(parent, "attachment.bin")
	reader := &replacingAttachmentReader{
		reader: bytes.NewReader(source),
		replace: func() error {
			if err := os.Rename(parent, moved); err != nil {
				return err
			}
			return os.Mkdir(parent, 0o700)
		},
	}
	err := writeVerifiedExclusiveFile(output, reader, int64(len(source)), sha256.Sum256(source))
	if errorCodeForTest(err) != "store_changed" {
		t.Fatalf("writeVerifiedExclusiveFile() error = %v, want store_changed", err)
	}
	if _, statErr := os.Lstat(output); !os.IsNotExist(statErr) {
		t.Fatalf("output exists after rejected parent swap: %v", statErr)
	}
	if _, statErr := os.Lstat(filepath.Join(moved, "attachment.bin")); !os.IsNotExist(statErr) {
		t.Fatalf("moved parent retains orphan output: %v", statErr)
	}
}

func TestWriteVerifiedExclusiveFileDoesNotRemoveRegularReplacement(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	output := filepath.Join(root, "attachment.bin")
	ownedPath := filepath.Join(root, "owned.bin")
	replacement := []byte("replacement bytes")
	reader := &replacingAttachmentReader{
		reader: bytes.NewReader([]byte("verified bytes")),
		replace: func() error {
			if err := os.Rename(output, ownedPath); err != nil {
				return err
			}
			return os.WriteFile(output, replacement, 0o600)
		},
	}
	err := writeVerifiedExclusiveFile(output, reader, int64(len("verified bytes")), sha256.Sum256([]byte("verified bytes")))
	if errorCodeForTest(err) != "store_changed" {
		t.Fatalf("writeVerifiedExclusiveFile() error = %v, want store_changed", err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("ReadFile() replacement error = %v", err)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatalf("replacement bytes = %q, want %q", got, replacement)
	}
}

func TestWriteVerifiedExclusiveFileDoesNotRemoveSymlinkReplacement(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	output := filepath.Join(root, "attachment.bin")
	ownedPath := filepath.Join(root, "owned.bin")
	target := filepath.Join(root, "target.bin")
	if err := os.WriteFile(target, []byte("keep target"), 0o600); err != nil {
		t.Fatalf("WriteFile() target error = %v", err)
	}
	reader := &replacingAttachmentReader{
		reader: bytes.NewReader([]byte("verified bytes")),
		replace: func() error {
			if err := os.Rename(output, ownedPath); err != nil {
				return err
			}
			return os.Symlink(target, output)
		},
	}
	err := writeVerifiedExclusiveFile(output, reader, int64(len("verified bytes")), sha256.Sum256([]byte("verified bytes")))
	if errorCodeForTest(err) != "store_changed" {
		t.Fatalf("writeVerifiedExclusiveFile() error = %v, want store_changed", err)
	}
	info, err := os.Lstat(output)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("replacement output = %v, want symlink", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile() target error = %v", err)
	}
	if string(got) != "keep target" {
		t.Fatalf("target bytes = %q, want %q", got, "keep target")
	}
}

type replacingAttachmentReader struct {
	reader  *bytes.Reader
	replace func() error
	done    bool
}

func (r *replacingAttachmentReader) Read(buffer []byte) (int, error) {
	if !r.done {
		if err := r.replace(); err != nil {
			return 0, err
		}
		r.done = true
	}
	return r.reader.Read(buffer)
}

func BenchmarkExternalAttachmentDiscovery(b *testing.B) {
	tests := []struct {
		name         string
		entries      int
		matchingName bool
	}{
		{name: "unique_1024_entries", entries: 1024, matchingName: true},
		{name: "identical_128_candidates", entries: 128},
	}
	for _, test := range tests {
		b.Run(test.name, func(b *testing.B) {
			store, _, resolved, directory := newExternalAttachmentFixture(b)
			contents := bytes.Repeat([]byte("x"), 64)
			for index := range test.entries {
				name := fmt.Sprintf("candidate-%04d.bin", index)
				if test.matchingName && index == 0 {
					name = "invoice.pdf"
				}
				if err := os.WriteFile(filepath.Join(directory, name), contents, 0o600); err != nil {
					b.Fatalf("WriteFile(%s) error = %v", name, err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			var selected externalAttachment
			for range b.N {
				candidate, available, err := store.findExternalAttachment(
					resolved, attachmentRecord{ID: "2", Name: "invoice.pdf"},
				)
				if err != nil || !available {
					b.Fatalf("findExternalAttachment() available = %t, error = %v", available, err)
				}
				selected = candidate
			}
			if selected.budget != nil {
				b.ReportMetric(float64(selected.budget.directoryEntries), "entries/op")
				b.ReportMetric(float64(selected.budget.pinnedFileStats), "file_stats/op")
				b.ReportMetric(float64(selected.budget.hashedAmbiguityCandidates), "hash_candidates/op")
				b.ReportMetric(float64(selected.budget.hashBytes), "hash_bytes/op")
			}
		})
	}
}
