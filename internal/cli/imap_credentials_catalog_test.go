package cli

import (
	"slices"
	"strings"
	"testing"

	"mailcli/internal/keychain"
	"mailcli/internal/mail"
)

func TestIMAPCredentialCatalogAndRepairGuidance(t *testing.T) {
	wantedCodes := []string{"imap_credentials_missing", "keychain_load_failed", "keychain_unsupported", "account_identity_missing", "account_disabled", "account_reference_corrupt", "account_binding_stale", "transport_unsupported_provider"}
	for _, command := range []string{"messages.new", "messages.list", "messages.filter", "messages.search", "messages.get", "messages.raw", "attachments.list", "attachments.save", "drafts.open", "drafts.adopt", "messages.state", "messages.mark", "messages.move", "messages.copy", "messages.delete", "sync", "batch"} {
		t.Run(command, func(t *testing.T) {
			code, _, response := captureCapabilitiesJSON(t, "--for", command, "--errors", strings.Join(wantedCodes, ","), "--json")
			if code != 0 {
				t.Fatalf("scoped discovery failed: %+v", response.Error)
			}
			for _, wanted := range wantedCodes {
				found := false
				for _, entry := range response.Data.Capabilities.ErrorCodes {
					if entry.Code == "smtp_credentials_missing" {
						t.Fatal("IMAP command claims SMTP-only missing credential failure")
					}
					if entry.Code == wanted {
						found = slices.Equal(entry.Commands, []string{command}) && len(entry.Guidance) > 0
					}
				}
				if !found {
					t.Fatalf("scoped catalog lacks reachable failure %s", wanted)
				}
			}
			if code, _, rejected := captureCapabilitiesJSON(t, "--for", command, "--errors", "smtp_credentials_missing", "--json"); code != 2 || rejected.Error == nil || rejected.Error.Code != "invalid_argument" {
				t.Fatal("IMAP command accepts SMTP-only credential error lookup")
			}
			for _, test := range []struct{ code, why string }{
				{"imap_credentials_missing", "send setup"},
				{keychain.CodeLoadFailed, "Keychain access"},
				{keychain.CodeUnsupported, "build with Keychain support"},
			} {
				failure := newErrorData(command, responseData{}, &mail.OperationError{Code: test.code, Message: "credential boundary failed"})
				next := failureNextAction(failure, nil)
				if next.Do != "ask_user" || !strings.Contains(next.Why, test.why) || failure.Guidance.EffectCertainty != mail.EffectNone {
					t.Fatalf("%s repair changed: %+v guidance=%+v", test.code, next, failure.Guidance)
				}
			}
		})
	}
	group := catalogGroup(t, "smtp_credentials_missing", "drafts.send")
	if group.Next != "ask_user" || group.EffectCertainty != mail.EffectNone {
		t.Fatalf("SMTP missing credential semantics changed: %+v", group)
	}
}
