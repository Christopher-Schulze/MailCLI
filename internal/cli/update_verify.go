package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"debug/macho"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
)

func verifyReleaseChecksum(archiveName string, archive []byte, checksums []byte) error {
	expected, err := checksumForArchive(archiveName, string(checksums))
	if err != nil {
		return updateFailure("update_checksum_invalid", "%v", err)
	}
	actual := sha256.Sum256(archive)
	if !bytes.Equal(actual[:], expected) {
		return updateFailure("update_checksum_mismatch", "release archive checksum does not match SHA256SUMS")
	}
	return nil
}

func verifyReleaseSignature(checksums []byte, encodedSignature []byte, publicKey ed25519.PublicKey) error {
	if len(publicKey) != ed25519.PublicKeySize {
		return updateFailure("update_signature_invalid", "pinned Ed25519 release key is invalid")
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(string(encodedSignature)))
	if err != nil || len(signature) != ed25519.SignatureSize {
		return updateFailure("update_signature_invalid", "release signature is not valid base64 Ed25519 data")
	}
	if !ed25519.Verify(publicKey, checksums, signature) {
		return updateFailure("update_signature_invalid", "SHA256SUMS signature does not match the pinned release key")
	}
	return nil
}

func parseReleasePublicKey(encoded string) (ed25519.PublicKey, error) {
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("expected a base64 Ed25519 public key")
	}
	return ed25519.PublicKey(decoded), nil
}

func checksumForArchive(archiveName string, checksums string) ([]byte, error) {
	var matched string
	for _, line := range strings.Split(checksums, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != archiveName {
			continue
		}
		if matched != "" {
			return nil, fmt.Errorf("SHA256SUMS contains duplicate entries for %s", archiveName)
		}
		matched = fields[0]
	}
	if matched == "" {
		return nil, fmt.Errorf("SHA256SUMS does not contain %s", archiveName)
	}
	decoded, err := hex.DecodeString(matched)
	if err != nil || len(decoded) != sha256.Size {
		return nil, fmt.Errorf("SHA256SUMS contains an invalid digest for %s", archiveName)
	}
	return decoded, nil
}

func verifyReleaseBinary(ctx context.Context, binaryPath string, expectedVersion string) error {
	file, err := macho.Open(binaryPath)
	if err != nil {
		return fmt.Errorf("open Mach-O binary: %w", err)
	}
	cpu := file.Cpu
	closeErr := file.Close()
	if closeErr != nil {
		return closeErr
	}
	if cpu != macho.CpuArm64 {
		return fmt.Errorf("release binary architecture is %s, want arm64", cpu)
	}
	if output, err := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--strict", binaryPath).CombinedOutput(); err != nil {
		return fmt.Errorf("verify code signature: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return verifyBinaryVersion(ctx, binaryPath, expectedVersion)
}

func verifyInstalledBinary(ctx context.Context, binaryPath string, expectedVersion string) error {
	return verifyBinaryVersion(ctx, binaryPath, expectedVersion)
}

func verifyBinaryVersion(ctx context.Context, binaryPath string, expectedVersion string) error {
	output, err := exec.CommandContext(ctx, binaryPath, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("run installed binary: %w: %s", err, strings.TrimSpace(string(output)))
	}
	want := "mailcli " + expectedVersion
	if strings.TrimSpace(string(output)) != want {
		return fmt.Errorf("version output is %q, want %q", strings.TrimSpace(string(output)), want)
	}
	return nil
}
