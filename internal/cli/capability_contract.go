package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

var contractDigest = sync.OnceValues(computeContractSHA256)

// computeContractSHA256 identifies the complete published contract: every
// command with its parameter and output schema, the shared $defs and the
// limits. It is independent of the requested view and of the release version.
func computeContractSHA256() (string, error) {
	manifest, err := capabilities()
	if err != nil {
		return "", err
	}
	if err := attachOutputSchemas(manifest.Commands); err != nil {
		return "", err
	}
	if err := publishOutputDefinitions(&manifest); err != nil {
		return "", err
	}
	return contractSHA256Of(manifest)
}

// contractSHA256Of hashes the canonical CLI JSON of a full manifest with its
// version and digest fields cleared.
func contractSHA256Of(manifest capabilityManifest) (string, error) {
	manifest.Version = ""
	manifest.ContractSHA256 = ""
	payload, err := marshalCLIJSON(manifest)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
