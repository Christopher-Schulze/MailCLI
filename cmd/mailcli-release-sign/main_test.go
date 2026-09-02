package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/releaseauth"
)

func TestDecodePublicKeyValid(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey error = %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(public)
	decoded, err := decodePublicKey(encoded)
	if err != nil {
		t.Fatalf("decodePublicKey error = %v", err)
	}
	if !decoded.Equal(public) {
		t.Error("decoded key does not match original")
	}
}

func TestDecodePublicKeyInvalidBase64(t *testing.T) {
	_, err := decodePublicKey("not-valid-base64!!!")
	if err == nil {
		t.Fatal("decodePublicKey error = nil, want invalid base64 error")
	}
}

func TestDecodePublicKeyWrongLength(t *testing.T) {
	short := base64.StdEncoding.EncodeToString([]byte("too short"))
	_, err := decodePublicKey(short)
	if err == nil {
		t.Fatal("decodePublicKey error = nil, want wrong length error")
	}
}

func TestReadBoundedFileWithinLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "input.txt")
	content := []byte("hello world")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	got, err := readBoundedFile(path, 1024)
	if err != nil {
		t.Fatalf("readBoundedFile error = %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content = %q, want %q", got, content)
	}
}

func TestReadBoundedFileExceedsLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oversize.txt")
	content := make([]byte, 100)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	_, err := readBoundedFile(path, 50)
	if err == nil {
		t.Fatal("readBoundedFile error = nil, want exceeds limit error")
	}
}

func TestReadBoundedFileEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	got, err := readBoundedFile(path, 1024)
	if err != nil {
		t.Fatalf("readBoundedFile error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("content length = %d, want 0", len(got))
	}
}

func TestReadBoundedFileMissing(t *testing.T) {
	_, err := readBoundedFile("/nonexistent/path/file.txt", 1024)
	if err == nil {
		t.Fatal("readBoundedFile error = nil, want file not found error")
	}
}

func TestWriteExclusiveCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.txt")
	payload := []byte("test content")
	if err := writeExclusive(path, payload, 0o600); err != nil {
		t.Fatalf("writeExclusive error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("content = %q, want %q", got, payload)
	}
}

func TestWriteExclusiveRejectsExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	err := writeExclusive(path, []byte("new"), 0o600)
	if err == nil {
		t.Fatal("writeExclusive error = nil, want file exists error")
	}
}

func TestValidatePublicKeyMatch(t *testing.T) {
	public, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey error = %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(public)
	if err := validatePublicKey(privateKey, encoded); err != nil {
		t.Fatalf("validatePublicKey error = %v", err)
	}
}

func TestValidatePublicKeyMismatch(t *testing.T) {
	public1, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey error = %v", err)
	}
	_, privateKey2, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey error = %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(public1)
	err = validatePublicKey(privateKey2, encoded)
	if err == nil {
		t.Fatal("validatePublicKey error = nil, want mismatch error")
	}
}

func TestReadPrivateKeyRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("data"), 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink error = %v", err)
	}
	_, err := readPrivateKey(link)
	if err == nil {
		t.Fatal("readPrivateKey error = nil, want symlink rejection error")
	}
}

func TestReadPrivateKeyRejectsGroupReadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.txt")
	if err := os.WriteFile(path, []byte("data"), 0o640); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	_, err := readPrivateKey(path)
	if err == nil {
		t.Fatal("readPrivateKey error = nil, want permission rejection error")
	}
}

func TestReadPrivateKeyRejectsInvalidContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.txt")
	if err := os.WriteFile(path, []byte("not-a-key"), 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	_, err := readPrivateKey(path)
	if err == nil {
		t.Fatal("readPrivateKey error = nil, want invalid key error")
	}
}

func TestReadPrivateKeyValidRoundtrip(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey error = %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "key.txt")
	encoded := base64.StdEncoding.EncodeToString(privateKey) + "\n"
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	got, err := readPrivateKey(path)
	if err != nil {
		t.Fatalf("readPrivateKey error = %v", err)
	}
	if !got.Equal(privateKey) {
		t.Error("read key does not match original")
	}
}

func TestRunNoArgs(t *testing.T) {
	code := run(nil, os.Stdout, os.Stderr)
	if code != 2 {
		t.Errorf("run(nil) = %d, want 2", code)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	code := run([]string{"unknown"}, os.Stdout, os.Stderr)
	if code != 2 {
		t.Errorf("run(unknown) = %d, want 2", code)
	}
}

func TestRunKeygenWritesPrivateKeyAndPrintsPublic(t *testing.T) {
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "private.key")
	var stdout bytes.Buffer
	err := runKeygen([]string{"-private", privatePath}, &stdout, os.Stderr)
	if err != nil {
		t.Fatalf("runKeygen error = %v", err)
	}
	info, err := os.Lstat(privatePath)
	if err != nil {
		t.Fatalf("private key not written: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("private key mode = %o, want 0600", info.Mode().Perm())
	}
	publicEncoded := strings.TrimSpace(stdout.String())
	if publicEncoded == "" {
		t.Fatal("public key not printed to stdout")
	}
	public, err := decodePublicKey(publicEncoded)
	if err != nil {
		t.Fatalf("printed public key invalid: %v", err)
	}
	if len(public) != ed25519.PublicKeySize {
		t.Errorf("public key length = %d, want %d", len(public), ed25519.PublicKeySize)
	}
}

func TestRunKeygenRejectsRelativePath(t *testing.T) {
	err := runKeygen([]string{"-private", "relative/path"}, os.Stdout, os.Stderr)
	if err == nil {
		t.Fatal("runKeygen error = nil, want relative path rejection")
	}
}

func TestRunKeygenRejectsExtraArgs(t *testing.T) {
	err := runKeygen([]string{"-private", "/tmp/key", "extra"}, os.Stdout, os.Stderr)
	if err == nil {
		t.Fatal("runKeygen error = nil, want extra args rejection")
	}
}

func TestRunSignAndVerifyRoundtrip(t *testing.T) {
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "private.key")
	inputPath := filepath.Join(dir, "SHA256SUMS")
	outputPath := filepath.Join(dir, "SHA256SUMS.sig")

	var keygenStdout bytes.Buffer
	if err := runKeygen([]string{"-private", privatePath}, &keygenStdout, os.Stderr); err != nil {
		t.Fatalf("runKeygen error = %v", err)
	}
	publicEncoded := strings.TrimSpace(keygenStdout.String())

	manifest := []byte("abc123 archive.tar.gz\n")
	if err := os.WriteFile(inputPath, manifest, 0o644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	if err := runSign([]string{
		"-private", privatePath,
		"-input", inputPath,
		"-output", outputPath,
		"-expected-public", publicEncoded,
	}, os.Stderr); err != nil {
		t.Fatalf("runSign error = %v", err)
	}

	if _, err := os.Lstat(outputPath); err != nil {
		t.Fatalf("signature not written: %v", err)
	}

	if err := runVerify([]string{
		"-public", publicEncoded,
		"-input", inputPath,
		"-signature", outputPath,
	}, os.Stderr); err != nil {
		t.Fatalf("runVerify error = %v", err)
	}
}

func TestRunSignRejectsWrongPublicKey(t *testing.T) {
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "private.key")
	inputPath := filepath.Join(dir, "input.txt")
	outputPath := filepath.Join(dir, "sig.txt")

	var keygenStdout bytes.Buffer
	if err := runKeygen([]string{"-private", privatePath}, &keygenStdout, os.Stderr); err != nil {
		t.Fatalf("runKeygen error = %v", err)
	}
	correctPublic := strings.TrimSpace(keygenStdout.String())

	otherPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey error = %v", err)
	}
	wrongPublic := base64.StdEncoding.EncodeToString(otherPublic)

	if err := os.WriteFile(inputPath, []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	err = runSign([]string{
		"-private", privatePath,
		"-input", inputPath,
		"-output", outputPath,
		"-expected-public", wrongPublic,
	}, os.Stderr)
	if err == nil {
		t.Fatal("runSign error = nil, want public key mismatch")
	}
	_ = correctPublic
}

func TestRunVerifyRejectsTamperedManifest(t *testing.T) {
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "private.key")
	inputPath := filepath.Join(dir, "input.txt")
	outputPath := filepath.Join(dir, "sig.txt")

	var keygenStdout bytes.Buffer
	if err := runKeygen([]string{"-private", privatePath}, &keygenStdout, os.Stderr); err != nil {
		t.Fatalf("runKeygen error = %v", err)
	}
	publicEncoded := strings.TrimSpace(keygenStdout.String())

	if err := os.WriteFile(inputPath, []byte("original content"), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	if err := runSign([]string{
		"-private", privatePath,
		"-input", inputPath,
		"-output", outputPath,
		"-expected-public", publicEncoded,
	}, os.Stderr); err != nil {
		t.Fatalf("runSign error = %v", err)
	}

	if err := os.WriteFile(inputPath, []byte("tampered content"), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	if err := runVerify([]string{
		"-public", publicEncoded,
		"-input", inputPath,
		"-signature", outputPath,
	}, os.Stderr); err == nil {
		t.Fatal("runVerify error = nil, want tamper detection")
	}
}

func TestRunVerifyRejectsMissingInput(t *testing.T) {
	dir := t.TempDir()
	err := runVerify([]string{
		"-public", releaseauth.PublicKeyBase64,
		"-input", filepath.Join(dir, "nonexistent.txt"),
		"-signature", filepath.Join(dir, "also-nonexistent.txt"),
	}, os.Stderr)
	if err == nil {
		t.Fatal("runVerify error = nil, want missing file error")
	}
}

func TestRunSignRejectsRelativePaths(t *testing.T) {
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "private.key")
	var keygenStdout bytes.Buffer
	if err := runKeygen([]string{"-private", privatePath}, &keygenStdout, os.Stderr); err != nil {
		t.Fatalf("runKeygen error = %v", err)
	}
	publicEncoded := strings.TrimSpace(keygenStdout.String())

	err := runSign([]string{
		"-private", "relative/key",
		"-input", filepath.Join(dir, "input.txt"),
		"-output", filepath.Join(dir, "sig.txt"),
		"-expected-public", publicEncoded,
	}, os.Stderr)
	if err == nil {
		t.Fatal("runSign error = nil, want relative path rejection")
	}
}
