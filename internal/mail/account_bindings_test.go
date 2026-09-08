package mail

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
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

func (s *memoryAccountBindingStore) UpsertAccountBinding(binding AccountBinding) error {
	normalized, err := NormalizeAccountBinding(binding)
	if err != nil {
		return err
	}
	for index := range s.document.Bindings {
		if s.document.Bindings[index].AccountID == normalized.AccountID {
			s.document.Bindings[index] = normalized
			return nil
		}
	}
	s.document.Bindings = append(s.document.Bindings, normalized)
	return nil
}
