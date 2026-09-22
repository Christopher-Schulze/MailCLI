package mail

import (
	"strings"
	"testing"
)

func TestMissingCredentialsErrorForSameAccount(t *testing.T) {
	err := missingCredentialsErrorFor("user@gmail.com", "user@gmail.com")
	if errorCode(err) != "smtp_credentials_missing" {
		t.Fatalf("error code = %q, want smtp_credentials_missing", errorCode(err))
	}
	if !strings.Contains(err.Error(), "send setup --from user@gmail.com") {
		t.Fatalf("error message missing setup guidance: %v", err)
	}
	if strings.Contains(err.Error(), "--credential-account") {
		t.Fatalf("same-account error must not name a separate credential account: %v", err)
	}
}

func TestMissingCredentialsErrorForSeparateCredentialAccount(t *testing.T) {
	err := missingCredentialsErrorFor("alias@example.com", "user@gmail.com")
	if errorCode(err) != "smtp_credentials_missing" {
		t.Fatalf("error code = %q, want smtp_credentials_missing", errorCode(err))
	}
	if !strings.Contains(err.Error(), "user@gmail.com") {
		t.Fatalf("error message must name the credential account: %v", err)
	}
	if !strings.Contains(err.Error(), "--credential-account user@gmail.com") {
		t.Fatalf("error message missing credential-account guidance: %v", err)
	}
}

func TestNewMessageIDUsesParsedSenderDomain(t *testing.T) {
	id, err := newMessageID("user@gmail.com")
	if err != nil {
		t.Fatalf("newMessageID failed: %v", err)
	}
	if !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, "@gmail.com>") {
		t.Fatalf("message id = %q, want <hex>@gmail.com", id)
	}
}

func TestNewMessageIDRejectsSenderWithoutDomain(t *testing.T) {
	for _, sender := range []string{"", "noatsign", "user@"} {
		if id, err := newMessageID(sender); err == nil {
			t.Fatalf("newMessageID(%q) = %q, want error", sender, id)
		}
	}
}
