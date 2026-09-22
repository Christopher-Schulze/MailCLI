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
