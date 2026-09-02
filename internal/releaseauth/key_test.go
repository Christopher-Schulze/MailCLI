package releaseauth

import (
	"encoding/base64"
	"testing"
)

func TestPublicKeyBase64Decodes(t *testing.T) {
	key, err := base64.StdEncoding.DecodeString(PublicKeyBase64)
	if err != nil {
		t.Fatalf("DecodeString error = %v", err)
	}
	if len(key) != 32 {
		t.Errorf("decoded key length = %d, want 32 (Ed25519 public key)", len(key))
	}
}

func TestPublicKeyBase64IsCanonical(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(mustDecode(t, PublicKeyBase64))
	if encoded != PublicKeyBase64 {
		t.Errorf("re-encoded key = %q, want %q (non-canonical encoding)", encoded, PublicKeyBase64)
	}
}

func mustDecode(t *testing.T, s string) []byte {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("DecodeString error = %v", err)
	}
	return key
}
