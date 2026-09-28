package cli

import (
	"encoding/json"
	"regexp"
	"testing"
)

func TestContractSHA256IdentifiesTheFullContract(t *testing.T) {
	expected, err := computeContractSHA256()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(expected) {
		t.Fatalf("contract_sha256 = %q", expected)
	}
	again, err := computeContractSHA256()
	if err != nil || again != expected {
		t.Fatalf("contract_sha256 is not deterministic: %q vs %q (%v)", again, expected, err)
	}
	for _, flags := range [][]string{
		{"--json"}, {"--outputs", "--json"}, {"--limits", "--json"},
		{"--for", "messages.search", "--json"}, {"--for", "sync", "--schemas", "--outputs", "--json"},
	} {
		code, output, response := captureCapabilitiesJSON(t, flags...)
		if code != 0 || response.Data.Capabilities == nil || response.Data.Capabilities.ContractSHA256 != expected {
			t.Fatalf("capabilities %v: code=%d digest mismatch: %s", flags, code, output)
		}
	}

	// The digest covers the published --outputs contract, independent of the version.
	_, output, _ := captureCapabilitiesJSON(t, "--outputs", "--json")
	var published struct {
		Data struct {
			Capabilities capabilityManifest `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(output, &published); err != nil {
		t.Fatal(err)
	}
	full, err := capabilities()
	if err != nil {
		t.Fatal(err)
	}
	if err := attachOutputSchemas(full.Commands); err != nil {
		t.Fatal(err)
	}
	if err := publishOutputDefinitions(&full); err != nil {
		t.Fatal(err)
	}
	full.Version = "9.9.9"
	if digest, err := contractSHA256Of(full); err != nil || digest != expected {
		t.Fatalf("version changed the contract digest: %q, %v", digest, err)
	}
	full.Limits.MaximumBatchItems++
	if digest, err := contractSHA256Of(full); err != nil || digest == expected {
		t.Fatalf("a changed limit kept the contract digest: %q, %v", digest, err)
	}
	full.Limits.MaximumBatchItems--
	full.Commands[0].Confirmation += "-changed"
	if digest, err := contractSHA256Of(full); err != nil || digest == expected {
		t.Fatalf("a changed command contract kept the digest: %q, %v", digest, err)
	}
}
