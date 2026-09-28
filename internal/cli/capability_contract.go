package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

var contractDigest = sync.OnceValues(computeContractSHA256)

// computeContractSHA256 identifies the complete published contract: every
// command with its parameter and output schema, the shared $defs, the error
// catalog and the limits. It is independent of the requested view and of the release version.
func computeContractSHA256() (string, error) {
	manifest, err := fullContractManifest()
	if err != nil {
		return "", err
	}
	return contractSHA256Of(manifest)
}

// fullContractManifest is the unscoped manifest with every output schema,
// shared $defs and the complete error catalog attached.
func fullContractManifest() (capabilityManifest, error) {
	manifest, err := capabilities()
	if err != nil {
		return capabilityManifest{}, err
	}
	if err := attachOutputSchemas(manifest.Commands); err != nil {
		return capabilityManifest{}, err
	}
	manifest.ErrorCodes = errorCatalogFor(manifest.Commands)
	if err := publishOutputDefinitions(&manifest); err != nil {
		return capabilityManifest{}, err
	}
	return manifest, nil
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
