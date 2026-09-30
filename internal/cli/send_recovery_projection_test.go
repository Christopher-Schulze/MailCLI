package cli

import (
	"encoding/json"
	"reflect"
	"testing"

	"mailcli/internal/mail"
)

func TestSendRecoveryIdentityProjectionAndOutputContract(t *testing.T) {
	identity := &mail.SendRecoveryIdentity{AccountID: "ACCOUNT", Host: "imap.example.com", Port: 993, Username: "sender@example.com"}
	attempt := &mail.SendAttempt{RecoveryIdentity: identity}
	projected := draftSendAttemptProjection(attempt)
	if projected.RecoveryIdentity == nil || *projected.RecoveryIdentity != *identity || projected.RecoveryIdentity == identity {
		t.Fatal("draft projection lost or aliased original recovery target")
	}
	manifest, _ := fixtureOutputContracts(t)
	payload, err := json.Marshal(projected.RecoveryIdentity)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := manifest.OutputDefinitions[outputDefinitionName(reflect.TypeFor[mail.SendRecoveryIdentity]())]
	if !ok {
		t.Fatal("capabilities omit the public send recovery identity contract")
	}
	if err := validateFixtureNode(payload, node, manifest.OutputDefinitions, "recovery_identity"); err != nil {
		t.Fatal(err)
	}
}
