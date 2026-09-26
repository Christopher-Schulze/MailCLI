package mail

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestAccountBindingStoreRoundTripNormalizesAndProtectsFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private", "account-bindings.json")
	store := NewAccountBindingStore(path)
	if err := store.UpsertAccountBinding(AccountBinding{
		AccountID:         "account-1",
		SenderAliases:     []string{"ZED@gmail.com", "Alias@gmail.com"},
		CredentialAccount: "login@gmail.com",
	}); err != nil {
		t.Fatalf("UpsertAccountBinding() error = %v", err)
	}
	document, err := store.LoadAccountBindings()
	if err != nil {
		t.Fatalf("LoadAccountBindings() error = %v", err)
	}
	if document.Version != AccountBindingVersion || len(document.Bindings) != 1 {
		t.Fatalf("document = %+v", document)
	}
	binding := document.Bindings[0]
	if binding.AccountID != "ACCOUNT-1" || binding.CredentialAccount != "login@gmail.com" ||
		len(binding.SenderAliases) != 2 || binding.SenderAliases[0] != "Alias@gmail.com" {
		t.Fatalf("binding = %+v", binding)
	}
	if resolved, found, err := ResolveAccountBinding(document, "zed@gmail.com", ""); err != nil || !found || resolved.AccountID != "ACCOUNT-1" {
		t.Fatalf("ResolveAccountBinding() = %+v, found=%t, error=%v", resolved, found, err)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(binding file) error = %v", err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("binding file mode = %o, want 600", fileInfo.Mode().Perm())
	}
	directoryInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat(binding directory) error = %v", err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("binding directory mode = %o, want 700", directoryInfo.Mode().Perm())
	}
}

func TestAccountBindingPublicationFailureReportsRenameBoundary(t *testing.T) {
	tests := []struct {
		name      string
		boundary  accountBindingPublicationBoundary
		status    AccountBindingPublicationStatus
		wantAlias string
	}{
		{
			name: "before rename", boundary: accountBindingBeforeRename,
			status: AccountBindingPublicationNone, wantAlias: "before@icloud.com",
		},
		{
			name: "after rename", boundary: accountBindingAfterRename,
			status: AccountBindingPublicationUnknown, wantAlias: "after@icloud.com",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertAccountBindingPublicationFailure(t, test.boundary, test.status, test.wantAlias)
		})
	}
}

func assertAccountBindingPublicationFailure(
	t *testing.T,
	boundary accountBindingPublicationBoundary,
	wantStatus AccountBindingPublicationStatus,
	wantAlias string,
) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "account-bindings.json")
	seed := NewAccountBindingStore(path)
	if err := seed.UpsertAccountBinding(AccountBinding{
		AccountID: "ACCOUNT-A", SenderAliases: []string{"before@icloud.com"},
		CredentialAccount: "login@icloud.com",
	}); err != nil {
		t.Fatalf("seed UpsertAccountBinding() error = %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	hookCalled := false
	store := &fileAccountBindingStore{
		path: path,
		publicationHook: func(actual accountBindingPublicationBoundary) error {
			if actual != boundary {
				return nil
			}
			hookCalled = true
			return errors.New("injected publication failure")
		},
	}
	err = store.UpdateAccountBindings(context.Background(), func(document AccountBindingFile) (AccountBindingFile, error) {
		document.Bindings[0].SenderAliases = []string{"after@icloud.com"}
		return document, nil
	})
	if err == nil || !hookCalled {
		t.Fatalf("UpdateAccountBindings() error = %v, hook called = %t", err, hookCalled)
	}
	assertAccountBindingPublicationStatus(t, err, wantStatus)
	assertAccountBindingPublicationState(t, seed, path, before, wantStatus, wantAlias)
}

func assertAccountBindingPublicationStatus(t *testing.T, err error, want AccountBindingPublicationStatus) {
	t.Helper()
	var publication interface {
		BindingPublicationStatus() AccountBindingPublicationStatus
	}
	if !errors.As(err, &publication) || publication.BindingPublicationStatus() != want {
		t.Fatalf("publication status = %v, want %q (error %v)", publication, want, err)
	}
}

func assertAccountBindingPublicationState(
	t *testing.T,
	store AccountBindingStore,
	path string,
	before []byte,
	wantStatus AccountBindingPublicationStatus,
	wantAlias string,
) {
	t.Helper()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if wantStatus == AccountBindingPublicationNone && !bytes.Equal(after, before) {
		t.Fatalf("pre-rename failure changed file: before=%s after=%s", before, after)
	}
	if wantStatus == AccountBindingPublicationUnknown && bytes.Equal(after, before) {
		t.Fatal("post-rename failure did not publish the updated file")
	}
	document, err := store.LoadAccountBindings()
	if err != nil {
		t.Fatalf("LoadAccountBindings() error = %v", err)
	}
	if got := document.Bindings[0].SenderAliases[0]; got != wantAlias {
		t.Fatalf("persisted alias = %q, want %q", got, wantAlias)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".account-bindings-") && strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("publication failure left temporary file %q", entry.Name())
		}
	}
}

func TestAccountBindingReadDoesNotRepairPermissions(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "bindings")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "account-bindings.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"bindings":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}

	if _, err := NewAccountBindingStore(path).LoadAccountBindings(); err != nil {
		t.Fatalf("LoadAccountBindings() error = %v", err)
	}
	directoryInfo, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.Mode().Perm() != 0o750 || fileInfo.Mode().Perm() != 0o640 {
		t.Fatalf("read repaired permissions: directory=%o file=%o", directoryInfo.Mode().Perm(), fileInfo.Mode().Perm())
	}
}

func TestAccountBindingTemporarySweepPreservesUnownedAndUnsafeFiles(t *testing.T) {
	prefix := fmt.Sprintf(".account-bindings-%x-", sha256.Sum256([]byte("account-bindings.json")))
	stale := time.Now().Add(-11 * time.Minute)
	for _, test := range []struct {
		name    string
		file    string
		mode    os.FileMode
		old     bool
		removed bool
	}{
		{"owned stale", prefix + strings.Repeat("a", 32) + ".tmp", 0o600, true, true},
		{"owned fresh", prefix + strings.Repeat("b", 32) + ".tmp", 0o600, false, false},
		{"unsafe mode", prefix + strings.Repeat("c", 32) + ".tmp", 0o640, true, false},
		{"unsafe special mode", prefix + strings.Repeat("f", 32) + ".tmp", os.ModeSetuid | 0o600, true, false},
		{"legacy", ".account-bindings-" + strings.Repeat("d", 32) + ".tmp", 0o600, true, false},
		{"other binding", fmt.Sprintf(".account-bindings-%x-%s.tmp", sha256.Sum256([]byte("other.json")), strings.Repeat("e", 32)), 0o600, true, false},
		{"malformed random", prefix + strings.Repeat("F", 32) + ".tmp", 0o600, true, false},
		{"short random", prefix + strings.Repeat("a", 31) + ".tmp", 0o600, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "account-bindings.json")
			file := filepath.Join(directory, test.file)
			if err := os.WriteFile(file, []byte("retained bytes"), test.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(file, test.mode); err != nil {
				t.Fatal(err)
			}
			if test.old {
				if err := os.Chtimes(file, stale, stale); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(file)
			if err != nil {
				t.Fatal(err)
			}
			if err := NewAccountBindingStore(path).UpdateAccountBindings(context.Background(), func(document AccountBindingFile) (AccountBindingFile, error) { return document, nil }); err != nil {
				t.Fatal(err)
			}
			after, err := os.Lstat(file)
			if test.removed {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("owned stale temporary survived: %v", err)
				}
				return
			}
			payload, readErr := os.ReadFile(file)
			if err != nil || readErr != nil || !os.SameFile(before, after) || string(payload) != "retained bytes" {
				t.Fatalf("preserved temporary changed: %v, %v", err, readErr)
			}
		})
	}
}

func TestAccountBindingTemporaryOwnerPreservesPausedOtherWriter(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "first.json")
	entered := make(chan struct{})
	resume := make(chan struct{})
	result := make(chan error, 1)
	finished := false
	defer func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
		if !finished {
			select {
			case <-result:
			case <-time.After(30 * time.Second):
				t.Error("paused writer did not stop during cleanup")
			}
		}
	}()
	store := &fileAccountBindingStore{path: path, publicationHook: func(boundary accountBindingPublicationBoundary) error {
		if boundary == accountBindingBeforeRename {
			close(entered)
			<-resume
		}
		return nil
	}}
	go func() {
		result <- store.UpdateAccountBindings(context.Background(), func(document AccountBindingFile) (AccountBindingFile, error) { return document, nil })
	}()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("writer did not reach temporary publication")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var temporary string
	prefix := fmt.Sprintf(".account-bindings-%x-", sha256.Sum256([]byte("first.json")))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			temporary = filepath.Join(directory, entry.Name())
		}
	}
	if temporary == "" {
		t.Fatal("writer did not create its owner-qualified temporary")
	}
	stale := time.Now().Add(-11 * time.Minute)
	if err := os.Chtimes(temporary, stale, stale); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(temporary)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(temporary)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewAccountBindingStore(filepath.Join(directory, "second.json")).UpdateAccountBindings(context.Background(), func(document AccountBindingFile) (AccountBindingFile, error) { return document, nil }); err != nil {
		t.Fatal(err)
	}
	after, statErr := os.Lstat(temporary)
	retained, readErr := os.ReadFile(temporary)
	if statErr != nil || readErr != nil || !os.SameFile(before, after) || !bytes.Equal(payload, retained) {
		t.Fatalf("other binding changed live temporary: %v, %v", statErr, readErr)
	}
	close(resume)
	select {
	case err := <-result:
		finished = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("paused writer did not finish")
	}
	published, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(payload, published) {
		t.Fatalf("published binding differs from paused bytes: %v", err)
	}
	for _, basename := range []string{"first.json", "second.json"} {
		name := fmt.Sprintf("account-bindings-%x.lock", sha256.Sum256([]byte(basename)))
		info, err := os.Lstat(filepath.Join(directory, name))
		if err != nil || info.Mode() != 0o600 {
			t.Fatalf("owner-qualified lock %s: %v", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(directory, "account-bindings.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("custom owners created a shared lock: %v", err)
	}
}

func TestAccountBindingTemporarySweepPreservesUnsafeObjects(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "account-bindings.json")
			name := fmt.Sprintf(".account-bindings-%x-%s.tmp", sha256.Sum256([]byte(filepath.Base(path))), strings.Repeat("a", 32))
			temporary := filepath.Join(directory, name)
			outside := filepath.Join(t.TempDir(), "unrelated")
			if err := os.WriteFile(outside, []byte("unrelated bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(outside, temporary)
			case "directory":
				err = os.Mkdir(temporary, 0o700)
			case "hardlink":
				err = os.Link(outside, temporary)
			}
			if err != nil {
				t.Fatal(err)
			}
			stale := time.Now().Add(-11 * time.Minute)
			if kind != "symlink" {
				if err := os.Chtimes(temporary, stale, stale); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(temporary)
			if err != nil {
				t.Fatal(err)
			}
			if err := NewAccountBindingStore(path).UpdateAccountBindings(context.Background(), func(document AccountBindingFile) (AccountBindingFile, error) { return document, nil }); err != nil {
				t.Fatal(err)
			}
			after, err := os.Lstat(temporary)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("unsafe object changed: %v", err)
			}
			payload, err := os.ReadFile(outside)
			if err != nil || string(payload) != "unrelated bytes" {
				t.Fatalf("foreign bytes changed: %q, %v", payload, err)
			}
		})
	}
}

func TestAccountBindingTemporaryEligibilityRequiresOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "temporary")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-11 * time.Minute)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-10 * time.Minute)
	for _, test := range []struct {
		name     string
		owner    uint32
		eligible bool
	}{
		{"actual owner", uint32(os.Geteuid()), true},
		{"foreign owner", uint32(os.Geteuid()) + 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := accountBindingTemporaryEligible(info, cutoff, test.owner); got != test.eligible {
				t.Fatalf("temporary eligibility = %t, want %t", got, test.eligible)
			}
		})
	}
}

func TestAccountBindingTemporarySweepRejectsPinnedReplacement(t *testing.T) {
	for _, target := range []string{"file", "parent"} {
		t.Run(target, func(t *testing.T) {
			base := t.TempDir()
			directory := filepath.Join(base, "bindings")
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "account-bindings.json")
			lease, err := acquireAccountBindingLease(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := lease.release(); err != nil {
					t.Error(err)
				}
			}()
			name := accountBindingTemporaryPrefix(filepath.Base(path)) + strings.Repeat("a", 32) + ".tmp"
			temporary := filepath.Join(directory, name)
			if err := os.WriteFile(temporary, []byte("old bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			stale := time.Now().Add(-11 * time.Minute)
			if err := os.Chtimes(temporary, stale, stale); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(temporary)
			if err != nil {
				t.Fatal(err)
			}
			if target == "parent" {
				if err := os.Rename(directory, filepath.Join(base, "moved")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Rename(temporary, temporary+".saved"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(temporary, []byte("replacement bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			err = removeStaleAccountBindingTemporary(lease.storage, name, before, time.Now().Add(-10*time.Minute), lease.lock.directory)
			if errorCodeForBindingTest(err) != "account_binding_changed" {
				t.Fatalf("temporary replacement = %v, want account_binding_changed", err)
			}
			payload, err := os.ReadFile(temporary)
			if err != nil || string(payload) != "replacement bytes" {
				t.Fatalf("replacement changed: %q, %v", payload, err)
			}
			original := temporary + ".saved"
			if target == "parent" {
				original = filepath.Join(base, "moved", name)
			}
			retained, err := os.Lstat(original)
			if err != nil || !os.SameFile(before, retained) {
				t.Fatalf("original changed: %v", err)
			}
		})
	}
}

func TestAccountBindingPostRenameRejectsReplacement(t *testing.T) {
	for _, target := range []string{"parent", "file", "symlink"} {
		t.Run(target, func(t *testing.T) {
			base := t.TempDir()
			directory := filepath.Join(base, "bindings")
			path := filepath.Join(directory, "account-bindings.json")
			var published os.FileInfo
			store := &fileAccountBindingStore{path: path, publicationHook: func(boundary accountBindingPublicationBoundary) error {
				if boundary != accountBindingAfterRename {
					return nil
				}
				var err error
				published, err = os.Lstat(path)
				if err != nil {
					return err
				}
				if target == "parent" {
					if err := os.Rename(directory, filepath.Join(base, "moved")); err != nil {
						return err
					}
					if err := os.Mkdir(directory, 0o700); err != nil {
						return err
					}
				} else if err := os.Rename(path, path+".published"); err != nil {
					return err
				}
				if target == "symlink" {
					return os.Symlink(path+".published", path)
				}
				return os.WriteFile(path, []byte("unrelated replacement"), 0o600)
			}}
			err := store.UpdateAccountBindings(context.Background(), func(document AccountBindingFile) (AccountBindingFile, error) { return document, nil })
			if errorCodeForBindingTest(err) != "account_binding_changed" {
				t.Fatalf("publication replacement = %v, want account_binding_changed", err)
			}
			assertAccountBindingPublicationStatus(t, err, AccountBindingPublicationUnknown)
			original := path + ".published"
			if target == "parent" {
				original = filepath.Join(base, "moved", filepath.Base(path))
			}
			retained, err := os.Lstat(original)
			if err != nil || !os.SameFile(published, retained) {
				t.Fatalf("published inode was not retained: %v", err)
			}
			if target != "symlink" {
				payload, err := os.ReadFile(path)
				if err != nil || string(payload) != "unrelated replacement" {
					t.Fatalf("replacement changed: %q, %v", payload, err)
				}
			}
		})
	}
}

type accountBindingWriterCase struct {
	name        string
	firstID     string
	firstAlias  string
	secondID    string
	secondAlias string
	wantIDs     []string
	wantAliases []string
}

func TestAccountBindingUpdatesSerializeIndependentProcesses(t *testing.T) {
	tests := []accountBindingWriterCase{
		{
			name: "distinct accounts", firstID: "ACCOUNT-A", firstAlias: "first@icloud.com",
			secondID: "ACCOUNT-B", secondAlias: "second@icloud.com", wantIDs: []string{"ACCOUNT-A", "ACCOUNT-B"},
		},
		{
			name: "same account aliases", firstID: "ACCOUNT-A", firstAlias: "first@icloud.com",
			secondID: "ACCOUNT-A", secondAlias: "second@icloud.com", wantIDs: []string{"ACCOUNT-A"},
			wantAliases: []string{"first@icloud.com", "second@icloud.com"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test := test
			path := filepath.Join(t.TempDir(), "account-bindings.json")
			runSerializedAccountBindingWriters(t, path, test)
			assertAccountBindingWriterResult(t, path, test)
		})
	}
}

func runSerializedAccountBindingWriters(t *testing.T, path string, test accountBindingWriterCase) {
	runSerializedAccountBindingWriterModes(t, path, test, false, false)
}

func runSerializedAccountBindingWriterModes(t *testing.T, path string, test accountBindingWriterCase, firstLegacy, secondLegacy bool) {
	t.Helper()
	first := startAccountBindingProcessMode(t, path, test.firstID, test.firstAlias, true, firstLegacy)
	readAccountBindingProcessLine(t, first.output, "MAILCLI_BINDING_ATTEMPT")
	readAccountBindingProcessLine(t, first.output, "MAILCLI_BINDING_CALLBACK")
	second := startAccountBindingProcessMode(t, path, test.secondID, test.secondAlias, false, secondLegacy)
	readAccountBindingProcessLine(t, second.output, "MAILCLI_BINDING_ATTEMPT")
	readAccountBindingProcessLine(t, second.output, "MAILCLI_BINDING_BLOCKED")
	secondCallback := nextAccountBindingProcessLine(second.output)
	select {
	case line := <-secondCallback:
		t.Fatalf("second process entered merge while first held the transaction: %s", line)
	default:
	}
	if _, err := io.WriteString(first.input, "continue\n"); err != nil {
		t.Fatalf("release first writer: %v", err)
	}
	if err := first.input.Close(); err != nil {
		t.Fatalf("close first writer input: %v", err)
	}
	readAccountBindingProcessLine(t, first.output, "MAILCLI_BINDING_DONE")
	waitForAccountBindingProcessLine(t, secondCallback, "MAILCLI_BINDING_CALLBACK")
	readAccountBindingProcessLine(t, second.output, "MAILCLI_BINDING_DONE")
	first.wait(t)
	second.wait(t)
}

func assertAccountBindingWriterResult(t *testing.T, path string, test accountBindingWriterCase) {
	t.Helper()
	document, err := NewAccountBindingStore(path).LoadAccountBindings()
	if err != nil {
		t.Fatalf("LoadAccountBindings() error = %v", err)
	}
	if len(document.Bindings) != len(test.wantIDs) {
		t.Fatalf("persisted bindings = %+v, want account IDs %v", document.Bindings, test.wantIDs)
	}
	for index, wantID := range test.wantIDs {
		if document.Bindings[index].AccountID != wantID {
			t.Fatalf("binding[%d].AccountID = %q, want %q", index, document.Bindings[index].AccountID, wantID)
		}
	}
	if len(test.wantAliases) != 0 && !equalBindingStrings(document.Bindings[0].SenderAliases, test.wantAliases) {
		t.Fatalf("persisted aliases = %v, want %v", document.Bindings[0].SenderAliases, test.wantAliases)
	}
}

func TestAccountBindingBusyAndCanceledUpdatesPreserveDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account-bindings.json")
	store := NewAccountBindingStore(path)
	if err := store.UpsertAccountBinding(AccountBinding{
		AccountID: "ACCOUNT-A", SenderAliases: []string{"first@icloud.com"}, CredentialAccount: "login@icloud.com",
	}); err != nil {
		t.Fatalf("seed UpsertAccountBinding() error = %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	expectAccountBindingLockTimeout(t, store, path)
	expectCanceledAccountBindingUpdate(t, store)
	expectRejectedAccountBindingUpdate(t, store)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("busy update changed document bytes: before=%s after=%s", before, after)
	}
}

func expectAccountBindingLockTimeout(t *testing.T, store AccountBindingStore, path string) {
	t.Helper()
	lockReference, err := accountBindingLockReference(path)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := acquireDraftLease(context.Background(), filepath.Dir(path), lockReference)
	if err != nil {
		t.Fatalf("acquire held account-binding lock: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	called := false
	err = store.UpdateAccountBindings(ctx, func(document AccountBindingFile) (AccountBindingFile, error) {
		called = true
		return document, nil
	})
	waited := time.Since(started)
	if releaseErr := lease.release(); releaseErr != nil {
		t.Fatalf("release held account-binding lock: %v", releaseErr)
	}
	if errorCodeForBindingTest(err) != "account_binding_busy" || called || waited < 1800*time.Millisecond || waited > 4*time.Second {
		t.Fatalf("UpdateAccountBindings() error=%v callback=%t waited=%s, want timed account_binding_busy without callback", err, called, waited)
	}
}

func expectCanceledAccountBindingUpdate(t *testing.T, store AccountBindingStore) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := store.UpdateAccountBindings(ctx, func(document AccountBindingFile) (AccountBindingFile, error) {
		called = true
		return document, nil
	})
	if errorCodeForBindingTest(err) != "account_binding_busy" || called {
		t.Fatalf("canceled UpdateAccountBindings() error=%v callback=%t, want account_binding_busy without callback", err, called)
	}
}

func expectRejectedAccountBindingUpdate(t *testing.T, store AccountBindingStore) {
	t.Helper()
	wantErr := errors.New("reject account-binding update")
	err := store.UpdateAccountBindings(context.Background(), func(AccountBindingFile) (AccountBindingFile, error) {
		return AccountBindingFile{}, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("rejected UpdateAccountBindings() error = %v, want callback error", err)
	}
}

func TestAccountBindingUpdateRejectsReplacedParentWithoutChangingBytes(t *testing.T) {
	fixture := newAccountBindingParentFixture(t)
	entered, continueUpdate, updateResult := startPausedAccountBindingUpdate(fixture.store)
	defer func() {
		select {
		case <-continueUpdate:
		default:
			close(continueUpdate)
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("account-binding update did not enter its locked callback")
	}
	movedDirectory := filepath.Join(fixture.base, "moved-bindings")
	replacement := replaceAccountBindingParent(t, fixture.directory, movedDirectory, fixture.path)
	close(continueUpdate)
	updateErr := <-updateResult
	if errorCodeForBindingTest(updateErr) != "account_binding_changed" {
		t.Fatalf("UpdateAccountBindings() error = %v, want account_binding_changed", updateErr)
	}
	assertAccountBindingParentUnchanged(t, fixture, movedDirectory, replacement)
}

type accountBindingParentFixture struct {
	base      string
	directory string
	path      string
	store     AccountBindingStore
	before    []byte
}

func newAccountBindingParentFixture(t *testing.T) accountBindingParentFixture {
	t.Helper()
	base := t.TempDir()
	directory := filepath.Join(base, "bindings")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "account-bindings.json")
	store := NewAccountBindingStore(path)
	if err := store.UpsertAccountBinding(AccountBinding{
		AccountID: "ACCOUNT-A", SenderAliases: []string{"first@icloud.com"}, CredentialAccount: "login@icloud.com",
	}); err != nil {
		t.Fatalf("seed UpsertAccountBinding() error = %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return accountBindingParentFixture{base: base, directory: directory, path: path, store: store, before: before}
}

func assertAccountBindingParentUnchanged(t *testing.T, fixture accountBindingParentFixture, movedDirectory string, replacement []byte) {
	t.Helper()
	movedPath := filepath.Join(movedDirectory, filepath.Base(fixture.path))
	movedBytes, err := os.ReadFile(movedPath)
	if err != nil {
		t.Fatal(err)
	}
	replacementBytes, err := os.ReadFile(fixture.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(movedBytes, fixture.before) || !bytes.Equal(replacementBytes, replacement) {
		t.Fatalf("parent replacement changed bytes: moved=%s replacement=%s", movedBytes, replacementBytes)
	}
}

func startPausedAccountBindingUpdate(store AccountBindingStore) (<-chan struct{}, chan struct{}, <-chan error) {
	entered := make(chan struct{})
	continueUpdate := make(chan struct{})
	updateResult := make(chan error, 1)
	go func() {
		updateResult <- store.UpdateAccountBindings(context.Background(), func(document AccountBindingFile) (AccountBindingFile, error) {
			close(entered)
			<-continueUpdate
			document.Bindings[0].SenderAliases = append(document.Bindings[0].SenderAliases, "second@icloud.com")
			return document, nil
		})
	}()
	return entered, continueUpdate, updateResult
}

func replaceAccountBindingParent(t *testing.T, directory string, movedDirectory string, path string) []byte {
	t.Helper()
	if err := os.Rename(directory, movedDirectory); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	replacement := []byte("must remain unchanged")
	if err := os.WriteFile(path, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	return replacement
}

type accountBindingProcessWaitContext struct {
	context.Context
	once sync.Once
	t    *testing.T
}

func (c *accountBindingProcessWaitContext) Done() <-chan struct{} {
	c.once.Do(func() {
		if _, err := fmt.Fprintln(os.Stdout, "MAILCLI_BINDING_BLOCKED"); err != nil {
			c.t.Fatalf("write binding helper wait marker: %v", err)
		}
	})
	return c.Context.Done()
}

func TestAccountBindingStoreProcessHelper(t *testing.T) {
	if os.Getenv("MAILCLI_ACCOUNT_BINDING_HELPER") != "1" {
		return
	}
	path := os.Getenv("MAILCLI_ACCOUNT_BINDING_PATH")
	accountID := os.Getenv("MAILCLI_ACCOUNT_BINDING_ID")
	alias := os.Getenv("MAILCLI_ACCOUNT_BINDING_ALIAS")
	if _, err := fmt.Fprintln(os.Stdout, "MAILCLI_BINDING_ATTEMPT"); err != nil {
		t.Fatalf("write binding helper attempt marker: %v", err)
	}
	lockReference, err := accountBindingLockReference(path)
	if err != nil {
		t.Fatal(err)
	}
	baseContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx := &accountBindingProcessWaitContext{Context: baseContext, t: t}
	var lease *draftLease
	var release func() error
	if os.Getenv("MAILCLI_ACCOUNT_BINDING_LEGACY") == "1" {
		lease, err = acquireDraftLease(ctx, filepath.Dir(path), lockReference)
		if err == nil {
			release = lease.release
		}
	} else {
		var bindingLease *accountBindingLease
		bindingLease, err = acquireAccountBindingLeaseWithContext(ctx, path)
		if err == nil {
			lease, release = bindingLease.draftLease, bindingLease.release
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	err = updateAccountBindingsLocked(ctx, lease, filepath.Base(path), func(document AccountBindingFile) (AccountBindingFile, error) {
		if _, err := fmt.Fprintln(os.Stdout, "MAILCLI_BINDING_CALLBACK"); err != nil {
			t.Fatalf("write binding helper callback marker: %v", err)
		}
		if os.Getenv("MAILCLI_ACCOUNT_BINDING_HOLD") == "1" {
			line, err := bufio.NewReader(os.Stdin).ReadString('\n')
			if err != nil || strings.TrimSpace(line) != "continue" {
				return AccountBindingFile{}, fmt.Errorf("held writer release = %q, error = %v", line, err)
			}
		}
		return mergeProcessAccountBinding(document, accountID, alias)
	}, nil)
	err = finishAccountBindingUpdate(err, release())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(os.Stdout, "MAILCLI_BINDING_DONE"); err != nil {
		t.Fatalf("write binding helper completion marker: %v", err)
	}
}

func mergeProcessAccountBinding(document AccountBindingFile, accountID string, alias string) (AccountBindingFile, error) {
	for index := range document.Bindings {
		if document.Bindings[index].AccountID != accountID {
			continue
		}
		for _, existing := range document.Bindings[index].SenderAliases {
			if strings.EqualFold(existing, alias) {
				return document, nil
			}
		}
		document.Bindings[index].SenderAliases = append(document.Bindings[index].SenderAliases, alias)
		return document, nil
	}
	document.Bindings = append(document.Bindings, AccountBinding{
		AccountID: accountID, SenderAliases: []string{alias}, CredentialAccount: "login@icloud.com",
	})
	return document, nil
}

type accountBindingProcess struct {
	command *exec.Cmd
	input   io.WriteCloser
	output  *bufio.Reader
	stderr  *bytes.Buffer
	waited  bool
}

func startAccountBindingProcessMode(t *testing.T, path string, accountID string, alias string, hold, legacy bool) *accountBindingProcess {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestAccountBindingStoreProcessHelper$", "-test.v")
	holdValue := "0"
	if hold {
		holdValue = "1"
	}
	legacyValue := "0"
	if legacy {
		legacyValue = "1"
	}
	environment := make([]string, 0, len(os.Environ())+5)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "MAILCLI_ACCOUNT_BINDING_") {
			environment = append(environment, entry)
		}
	}
	command.Env = append(environment,
		"MAILCLI_ACCOUNT_BINDING_HELPER=1",
		"MAILCLI_ACCOUNT_BINDING_PATH="+path,
		"MAILCLI_ACCOUNT_BINDING_ID="+accountID,
		"MAILCLI_ACCOUNT_BINDING_ALIAS="+alias,
		"MAILCLI_ACCOUNT_BINDING_HOLD="+holdValue,
		"MAILCLI_ACCOUNT_BINDING_LEGACY="+legacyValue,
	)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatalf("binding helper StdinPipe() error = %v", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = input.Close()
		t.Fatalf("binding helper StdoutPipe() error = %v", err)
	}
	process := &accountBindingProcess{command: command, input: input, output: bufio.NewReader(stdout), stderr: &bytes.Buffer{}}
	command.Stderr = process.stderr
	if err := command.Start(); err != nil {
		_ = input.Close()
		t.Fatalf("binding helper Start() error = %v", err)
	}
	t.Cleanup(process.stop)
	return process
}

func TestAccountBindingMixedVersionWritersPreserveLockInodes(t *testing.T) {
	for _, firstLegacy := range []bool{true, false} {
		t.Run(fmt.Sprintf("legacy first %t", firstLegacy), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "account-bindings.json")
			store := NewAccountBindingStore(path)
			if err := store.UpdateAccountBindings(context.Background(), func(document AccountBindingFile) (AccountBindingFile, error) { return document, nil }); err != nil {
				t.Fatal(err)
			}
			legacy, err := accountBindingLockReference(path)
			if err != nil {
				t.Fatal(err)
			}
			names := []string{legacy + ".lock", "account-bindings.lock"}
			before := make([]os.FileInfo, len(names))
			for index, name := range names {
				before[index], err = os.Lstat(filepath.Join(root, name))
				if err != nil {
					t.Fatal(err)
				}
			}
			test := accountBindingWriterCase{firstID: "ACCOUNT-A", firstAlias: "a@icloud.com", secondID: "ACCOUNT-B", secondAlias: "b@icloud.com", wantIDs: []string{"ACCOUNT-A", "ACCOUNT-B"}}
			runSerializedAccountBindingWriterModes(t, path, test, firstLegacy, !firstLegacy)
			assertAccountBindingWriterResult(t, path, test)
			drafts := filepath.Join(root, "drafts")
			if err := os.Mkdir(drafts, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := NewServiceWithDraftRoot(nil, drafts).PruneDrafts(PruneDraftsRequest{OlderThan: 24 * time.Hour, Confirm: true}); err != nil {
				t.Fatal(err)
			}
			for index, name := range names {
				after, err := os.Lstat(filepath.Join(root, name))
				payload, readErr := os.ReadFile(filepath.Join(root, name))
				if err != nil || readErr != nil || !os.SameFile(before[index], after) || len(payload) != 0 {
					t.Fatalf("binding lock %s changed: %v, %v", name, err, readErr)
				}
			}
		})
	}
}

func TestAccountBindingNewLockBlocksAndPreservesUnsafePaths(t *testing.T) {
	for _, kind := range []string{"held", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "account-bindings.json")
			lockPath := filepath.Join(root, "account-bindings.lock")
			outside := filepath.Join(t.TempDir(), "foreign")
			if err := os.WriteFile(outside, []byte("foreign bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "held":
				file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := file.Close(); err != nil {
						t.Error(err)
					}
				}()
				if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(outside, lockPath); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(lockPath, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			called := false
			err = NewAccountBindingStore(path).UpdateAccountBindings(ctx, func(document AccountBindingFile) (AccountBindingFile, error) { called = true; return document, nil })
			want := "account_binding_unsafe"
			if kind == "held" {
				want = "account_binding_busy"
			}
			if errorCodeForBindingTest(err) != want || called {
				t.Fatalf("lock refusal = %v, callback=%t", err, called)
			}
			after, err := os.Lstat(lockPath)
			payload, readErr := os.ReadFile(outside)
			if err != nil || readErr != nil || !os.SameFile(before, after) || string(payload) != "foreign bytes" {
				t.Fatalf("lock or foreign bytes changed: %v, %v", err, readErr)
			}
		})
	}
}

func TestAccountBindingReleaseFailureRetainsPublishedStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account-bindings.json")
	lease, err := acquireAccountBindingLease(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			if err := lease.release(); err != nil {
				t.Error(err)
			}
		}
	}()
	result := updateAccountBindingsLocked(context.Background(), lease.draftLease, filepath.Base(path), func(document AccountBindingFile) (AccountBindingFile, error) {
		document.Bindings = append(document.Bindings, AccountBinding{AccountID: "ACCOUNT-A", SenderAliases: []string{"a@icloud.com"}, CredentialAccount: "login@icloud.com"})
		return document, nil
	}, nil)
	if result != nil {
		t.Fatal(result)
	}
	if err := lease.bindingLock.Close(); err != nil {
		t.Fatal(err)
	}
	releaseErr := lease.release()
	released = true
	err = finishAccountBindingUpdate(result, releaseErr)
	assertAccountBindingPublicationStatus(t, err, AccountBindingPublicationComplete)
	var release interface{ BindingLockReleaseFailed() bool }
	if !errors.As(err, &release) || !release.BindingLockReleaseFailed() || !strings.Contains(err.Error(), "warning:") {
		t.Fatalf("missing release warning: %v", err)
	}
	document, err := NewAccountBindingStore(path).LoadAccountBindings()
	if err != nil || len(document.Bindings) != 1 || document.Bindings[0].AccountID != "ACCOUNT-A" {
		t.Fatalf("published binding lost: %+v, %v", document, err)
	}
	for _, status := range []AccountBindingPublicationStatus{AccountBindingPublicationNone, AccountBindingPublicationUnknown} {
		publicationErr := &AccountBindingError{Code: "account_binding_unavailable", Message: "publication failed", publicationStatus: status}
		combined := finishAccountBindingUpdate(publicationErr, releaseErr)
		assertAccountBindingPublicationStatus(t, combined, status)
		if !errors.Is(combined, publicationErr) || !errors.As(combined, &release) {
			t.Fatalf("publication/release failure lost: %v", combined)
		}
	}
}

func readAccountBindingProcessLine(t *testing.T, output *bufio.Reader, want string) {
	t.Helper()
	waitForAccountBindingProcessLine(t, nextAccountBindingProcessLine(output), want)
}

func waitForAccountBindingProcessLine(t *testing.T, lines <-chan string, want string) {
	t.Helper()
	var line string
	select {
	case line = <-lines:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for binding helper line %q", want)
	}
	if line != want {
		t.Fatalf("binding helper line = %q, want %q", line, want)
	}
}

func nextAccountBindingProcessLine(output *bufio.Reader) <-chan string {
	line := make(chan string, 1)
	go func() {
		for {
			value, err := output.ReadString('\n')
			if err != nil {
				line <- "read error: " + err.Error()
				return
			}
			value = strings.TrimSpace(value)
			if strings.HasPrefix(value, "MAILCLI_BINDING_") {
				line <- value
				return
			}
		}
	}()
	return line
}

func (p *accountBindingProcess) wait(t *testing.T) {
	t.Helper()
	if p.waited {
		return
	}
	if err := p.command.Wait(); err != nil {
		p.waited = true
		t.Fatalf("binding helper Wait() error = %v; stderr=%s", err, p.stderr.String())
	}
	p.waited = true
}

func (p *accountBindingProcess) stop() {
	if !p.waited && p.command.Process != nil {
		_ = p.input.Close()
		_ = p.command.Process.Kill()
		_ = p.command.Wait()
		p.waited = true
	}
}

func equalBindingStrings(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestResolveAccountBindingRejectsAmbiguousAlias(t *testing.T) {
	_, found, err := ResolveAccountBinding(AccountBindingFile{
		Version: AccountBindingVersion,
		Bindings: []AccountBinding{
			{AccountID: "A", SenderAliases: []string{"shared@icloud.com"}, CredentialAccount: "a@icloud.com"},
			{AccountID: "B", SenderAliases: []string{"shared@icloud.com"}, CredentialAccount: "b@icloud.com"},
		},
	}, "shared@icloud.com", "")
	if found || errorCodeForBindingTest(err) != "account_binding_ambiguous" {
		t.Fatalf("ResolveAccountBinding() found=%t, error=%v", found, err)
	}
	resolved, found, err := ResolveAccountBinding(AccountBindingFile{
		Version: AccountBindingVersion,
		Bindings: []AccountBinding{
			{AccountID: "A", SenderAliases: []string{"shared@icloud.com"}, CredentialAccount: "a@icloud.com"},
			{AccountID: "B", SenderAliases: []string{"shared@icloud.com"}, CredentialAccount: "b@icloud.com"},
		},
	}, "shared@icloud.com", "B")
	if err != nil || !found || resolved.AccountID != "B" {
		t.Fatalf("explicit ResolveAccountBinding() = %+v, found=%t, error=%v", resolved, found, err)
	}
}

func TestNormalizeAccountBindingRejectsProviderMismatch(t *testing.T) {
	_, err := NormalizeAccountBinding(AccountBinding{
		AccountID:         "ACCOUNT-1",
		SenderAliases:     []string{"alias@gmail.com"},
		CredentialAccount: "login@icloud.com",
	})
	if errorCodeForBindingTest(err) != "account_binding_provider_mismatch" {
		t.Fatalf("NormalizeAccountBinding() error = %v", err)
	}
}

func TestNormalizeAccountBindingAcceptsExplicitHostsForUnsupportedDomain(t *testing.T) {
	binding, err := NormalizeAccountBinding(AccountBinding{
		AccountID:         "ACCOUNT-1",
		SenderAliases:     []string{"user@corp.example"},
		CredentialAccount: "user@corp.example",
		SMTPHost:          "smtp.corp.example", SMTPPort: 587,
		IMAPHost: "imap.corp.example", IMAPPort: 993,
	})
	if err != nil {
		t.Fatalf("NormalizeAccountBinding() error = %v", err)
	}
	if binding.SMTPHost != "smtp.corp.example" || binding.SMTPPort != 587 ||
		binding.IMAPHost != "imap.corp.example" || binding.IMAPPort != 993 {
		t.Fatalf("binding = %+v", binding)
	}
}

func TestNormalizeAccountBindingRejectsInvalidHosts(t *testing.T) {
	base := AccountBinding{
		AccountID:         "ACCOUNT-1",
		SenderAliases:     []string{"user@corp.example"},
		CredentialAccount: "user@corp.example",
	}
	for _, tc := range []struct {
		name               string
		smtpHost, imapHost string
		smtpPort, imapPort int
	}{
		{name: "loopback ipv4", imapHost: "127.0.0.1", imapPort: 993},
		{name: "loopback ipv6", imapHost: "::1", imapPort: 993},
		{name: "private 10/8", imapHost: "10.0.0.5", imapPort: 993},
		{name: "private 192.168", imapHost: "192.168.1.1", imapPort: 993},
		{name: "link local", imapHost: "169.254.0.1", imapPort: 993},
		{name: "localhost name", imapHost: "localhost", imapPort: 993},
		{name: "localhost suffix", imapHost: "mx.localhost", imapPort: 993},
		{name: "local suffix", imapHost: "imap.home.local", imapPort: 993},
		{name: "internal suffix", imapHost: "mx.corp.internal", imapPort: 993},
		{name: "single label", imapHost: "mail", imapPort: 993},
		{name: "host with port", imapHost: "imap.example.com:993", imapPort: 993},
		{name: "host with path", imapHost: "imap.example.com/x", imapPort: 993},
		{name: "space in host", imapHost: "imap example.com", imapPort: 993},
		{name: "port out of range", imapHost: "imap.example.com", imapPort: 70000},
		{name: "host without port", imapHost: "imap.example.com", imapPort: 0},
		{name: "port without host", imapHost: "", imapPort: 993},
		{name: "smtp private", smtpHost: "172.16.0.1", smtpPort: 587},
		{name: "smtp link-local v6", smtpHost: "fe80::1", smtpPort: 587},
		{name: "smtp multicast", smtpHost: "224.0.0.1", smtpPort: 587},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding := base
			binding.SMTPHost, binding.SMTPPort = tc.smtpHost, tc.smtpPort
			binding.IMAPHost, binding.IMAPPort = tc.imapHost, tc.imapPort
			_, err := NormalizeAccountBinding(binding)
			if errorCodeForBindingTest(err) != "account_binding_host_invalid" {
				t.Fatalf("NormalizeAccountBinding(%s) error = %v, want account_binding_host_invalid", tc.name, err)
			}
		})
	}
}

func TestNormalizeAccountBindingAcceptsPublicIPLiteral(t *testing.T) {
	binding, err := NormalizeAccountBinding(AccountBinding{
		AccountID:         "ACCOUNT-1",
		SenderAliases:     []string{"user@corp.example"},
		CredentialAccount: "user@corp.example",
		IMAPHost:          "8.8.8.8", IMAPPort: 993,
	})
	if err != nil || binding.IMAPHost != "8.8.8.8" {
		t.Fatalf("NormalizeAccountBinding() = %+v, error = %v", binding, err)
	}
}

func TestNormalizeAccountBindingSkipsProviderCheckWithExplicitHosts(t *testing.T) {
	// Mixed alias domains that would mismatch providers are valid once the
	// binding pins its own endpoints.
	_, err := NormalizeAccountBinding(AccountBinding{
		AccountID:         "ACCOUNT-1",
		SenderAliases:     []string{"a@corp-one.example", "b@corp-two.example"},
		CredentialAccount: "login@corp-three.example",
		IMAPHost:          "imap.example.net", IMAPPort: 993,
	})
	if err != nil {
		t.Fatalf("NormalizeAccountBinding() error = %v", err)
	}
}

func TestResolveTransportHosts(t *testing.T) {
	explicit := &AccountBinding{
		SMTPHost: "smtp.corp.example", SMTPPort: 2525,
		IMAPHost: "imap.corp.example", IMAPPort: 1993,
	}
	t.Run("explicit hosts override provider", func(t *testing.T) {
		smtpHost, smtpPort, imapHost, imapPort, err := ResolveTransportHosts("user@gmail.com", explicit)
		if err != nil || smtpHost != "smtp.corp.example" || smtpPort != 2525 || imapHost != "imap.corp.example" || imapPort != 1993 {
			t.Fatalf("ResolveTransportHosts() = %s:%d %s:%d error=%v", smtpHost, smtpPort, imapHost, imapPort, err)
		}
	})
	t.Run("explicit hosts resolve unsupported domain", func(t *testing.T) {
		smtpHost, smtpPort, imapHost, imapPort, err := ResolveTransportHosts("user@corp.example", explicit)
		if err != nil || smtpHost != "smtp.corp.example" || imapHost != "imap.corp.example" {
			t.Fatalf("ResolveTransportHosts() = %s:%d %s:%d error=%v", smtpHost, smtpPort, imapHost, imapPort, err)
		}
	})
	t.Run("partial override cannot rescue unsupported domain", func(t *testing.T) {
		_, _, _, _, err := ResolveTransportHosts("user@corp.example", &AccountBinding{
			IMAPHost: "imap.corp.example", IMAPPort: 1993,
		})
		if errorCodeForBindingTest(err) != "transport_unsupported_provider" {
			t.Fatalf("ResolveTransportHosts() error = %v, want transport_unsupported_provider", err)
		}
	})
	t.Run("partial override mixes provider fallback", func(t *testing.T) {
		smtpHost, smtpPort, imapHost, imapPort, err := ResolveTransportHosts("user@gmail.com", &AccountBinding{
			IMAPHost: "imap.corp.example", IMAPPort: 1993,
		})
		if err != nil || smtpHost != "smtp.gmail.com" || smtpPort != 587 || imapHost != "imap.corp.example" || imapPort != 1993 {
			t.Fatalf("ResolveTransportHosts() = %s:%d %s:%d error=%v", smtpHost, smtpPort, imapHost, imapPort, err)
		}
	})
	t.Run("nil binding keeps provider table", func(t *testing.T) {
		smtpHost, _, imapHost, _, err := ResolveTransportHosts("user@icloud.com", nil)
		if err != nil || smtpHost != "smtp.mail.me.com" || imapHost != "imap.mail.me.com" {
			t.Fatalf("ResolveTransportHosts() = %s %s error=%v", smtpHost, imapHost, err)
		}
	})
}

func TestAccountDirectOpsSupport(t *testing.T) {
	hosts := &AccountBinding{
		SenderAliases:     []string{"user@corp.example"},
		CredentialAccount: "user@corp.example",
		SMTPHost:          "smtp.corp.example", SMTPPort: 587,
		IMAPHost: "imap.corp.example", IMAPPort: 993,
	}
	for _, tc := range []struct {
		name       string
		account    Account
		binding    *AccountBinding
		supported  bool
		wantReason DirectOpsReason
	}{
		{
			name:       "unbound provider domain",
			account:    Account{EmailAddresses: []string{"user@gmail.com"}},
			supported:  true,
			wantReason: DirectOpsReasonProviderSupported,
		},
		{
			name:       "unbound discovered identity",
			account:    Account{DiscoveredSenderIdentities: []string{"user@icloud.com"}},
			supported:  true,
			wantReason: DirectOpsReasonProviderSupported,
		},
		{
			name:       "unbound unsupported domain",
			account:    Account{EmailAddresses: []string{"user@corp.example"}},
			supported:  false,
			wantReason: DirectOpsReasonUnsupportedProvider,
		},
		{
			name:       "unbound no senders",
			account:    Account{},
			supported:  false,
			wantReason: DirectOpsReasonUnsupportedProvider,
		},
		{
			name:      "bound explicit hosts unsupported domain",
			account:   Account{EmailAddresses: []string{"user@corp.example"}},
			binding:   hosts,
			supported: true, wantReason: DirectOpsReasonBindingHosts,
		},
		{
			name:    "bound provider domain without hosts",
			account: Account{EmailAddresses: []string{"alias@gmail.com"}},
			binding: &AccountBinding{
				SenderAliases:     []string{"alias@gmail.com"},
				CredentialAccount: "login@gmail.com",
			},
			supported: true, wantReason: DirectOpsReasonProviderSupported,
		},
		{
			name:    "bound partial hosts unsupported domain",
			account: Account{EmailAddresses: []string{"user@corp.example"}},
			binding: &AccountBinding{
				SenderAliases:     []string{"user@corp.example"},
				CredentialAccount: "user@corp.example",
				IMAPHost:          "imap.corp.example", IMAPPort: 993,
			},
			supported: false, wantReason: DirectOpsReasonUnsupportedProvider,
		},
		{
			name:    "bound alias set drives candidates",
			account: Account{EmailAddresses: []string{"user@corp.example"}},
			binding: &AccountBinding{
				SenderAliases:     []string{"alias@gmail.com"},
				CredentialAccount: "login@gmail.com",
			},
			supported: true, wantReason: DirectOpsReasonProviderSupported,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			supported, reason := AccountDirectOpsSupport(tc.account, tc.binding)
			if supported != tc.supported || reason != tc.wantReason {
				t.Fatalf("AccountDirectOpsSupport() = %t,%s want %t,%s", supported, reason, tc.supported, tc.wantReason)
			}
		})
	}
}

func errorCodeForBindingTest(err error) string {
	var typed interface{ ErrorCode() string }
	if errors.As(err, &typed) {
		return typed.ErrorCode()
	}
	return ""
}

type memoryAccountBindingStore struct {
	document AccountBindingFile
}

func (s *memoryAccountBindingStore) LoadAccountBindings() (AccountBindingFile, error) {
	return s.document, nil
}

func (s *memoryAccountBindingStore) UpdateAccountBindings(ctx context.Context, update AccountBindingUpdate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	document, err := update(s.document)
	if err != nil {
		return err
	}
	document, err = validateAccountBindingFile(document)
	if err != nil {
		return err
	}
	s.document = document
	return nil
}

func (s *memoryAccountBindingStore) UpsertAccountBinding(binding AccountBinding) error {
	normalized, err := NormalizeAccountBinding(binding)
	if err != nil {
		return err
	}
	return s.UpdateAccountBindings(context.Background(), func(document AccountBindingFile) (AccountBindingFile, error) {
		for index := range document.Bindings {
			if document.Bindings[index].AccountID == normalized.AccountID {
				document.Bindings[index] = normalized
				return document, nil
			}
		}
		document.Bindings = append(document.Bindings, normalized)
		return document, nil
	})
}
