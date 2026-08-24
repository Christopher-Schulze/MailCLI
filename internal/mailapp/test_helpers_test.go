package mailapp

import (
	"encoding/json"
	"testing"
)

func mustBridgeJSON(t *testing.T, value bridgeResponse) string {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return string(payload)
}
