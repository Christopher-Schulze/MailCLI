package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
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

func TestFinishExclusiveOutputCleansEveryFailure(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*testExclusiveOutput)
	}{
		{name: "write", configure: func(output *testExclusiveOutput) {
			output.writeErr = errors.New("write failed")
		}},
		{name: "sync", configure: func(output *testExclusiveOutput) {
			output.syncErr = errors.New("sync failed")
		}},
		{name: "validation", configure: func(output *testExclusiveOutput) {
			output.validateErr = errors.New("validation failed")
		}},
		{name: "close", configure: func(output *testExclusiveOutput) {
			output.closeErr = errors.New("close failed")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output := newTestExclusiveOutput(t)
			test.configure(output)
			err := finishExclusiveOutput(output, []byte("signature"))
			if err == nil || !errors.Is(err, output.expectedErr()) {
				t.Fatalf("finishExclusiveOutput() error = %v", err)
			}
			if _, statErr := os.Stat(output.path); !os.IsNotExist(statErr) {
				t.Fatalf("failed output still exists: %v", statErr)
			}
		})
	}
}

func TestFinishExclusiveOutputReportsCleanupFailure(t *testing.T) {
	output := newTestExclusiveOutput(t)
	writeErr := errors.New("write failed")
	cleanupErr := errors.New("cleanup failed")
	output.writeErr = writeErr
	output.cleanupErr = cleanupErr
	err := finishExclusiveOutput(output, []byte("signature"))
	if !errors.Is(err, writeErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("finishExclusiveOutput() error = %v, want write and cleanup errors", err)
	}
	if _, statErr := os.Stat(output.path); statErr != nil {
		t.Fatalf("cleanup-failure output disappeared unexpectedly: %v", statErr)
	}
}

func TestFinishExclusiveOutputPreservesReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signature")
	output, err := openExclusiveOutput(path, 0o600)
	if err != nil {
		t.Fatalf("openExclusiveOutput() error = %v", err)
	}
	if _, err := output.Write([]byte("signature")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	movedPath := path + ".original"
	if err := os.Rename(path, movedPath); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	if err := os.WriteFile(path, []byte("attacker"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := finishExclusiveOutput(output, []byte("signature")); err == nil {
		t.Fatal("finishExclusiveOutput() error = nil, want replacement detection")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "attacker" {
		t.Fatalf("replacement content = %q, want attacker", content)
	}
}

func TestWriteExclusiveCanRerunAfterFailureCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signature")
	output := newTestExclusiveOutputAtPath(t, path)
	output.writeErr = errors.New("write failed")
	if err := finishExclusiveOutput(output, []byte("signature")); err == nil {
		t.Fatal("finishExclusiveOutput() error = nil, want injected write failure")
	}
	if err := writeExclusive(path, []byte("signature"), 0o600); err != nil {
		t.Fatalf("writeExclusive() rerun error = %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "signature" {
		t.Fatalf("rerun content = %q, want signature", content)
	}
}

type testExclusiveOutput struct {
	file        *os.File
	path        string
	identity    os.FileInfo
	writeErr    error
	syncErr     error
	validateErr error
	closeErr    error
	cleanupErr  error
}

func newTestExclusiveOutput(t *testing.T) *testExclusiveOutput {
	return newTestExclusiveOutputAtPath(t, filepath.Join(t.TempDir(), "output"))
}

func newTestExclusiveOutputAtPath(t *testing.T, path string) *testExclusiveOutput {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	identity, err := file.Stat()
	if err != nil {
		_ = file.Close()
		t.Fatalf("Stat() error = %v", err)
	}
	return &testExclusiveOutput{file: file, path: path, identity: identity}
}

func (output *testExclusiveOutput) Write(payload []byte) (int, error) {
	if output.writeErr != nil {
		return 0, output.writeErr
	}
	return output.file.Write(payload)
}

func (output *testExclusiveOutput) Sync() error {
	if output.syncErr != nil {
		return output.syncErr
	}
	return output.file.Sync()
}

func (output *testExclusiveOutput) Validate(_ int64) error {
	if output.validateErr != nil {
		return output.validateErr
	}
	return nil
}

func (output *testExclusiveOutput) CloseFile() error {
	err := output.file.Close()
	return errors.Join(output.closeErr, err)
}

func (output *testExclusiveOutput) Cleanup() error {
	if output.cleanupErr != nil {
		return output.cleanupErr
	}
	current, err := os.Lstat(output.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !os.SameFile(output.identity, current) {
		return errors.New("output changed")
	}
	return os.Remove(output.path)
}

func (output *testExclusiveOutput) CloseParent() error {
	return nil
}

func (output *testExclusiveOutput) expectedErr() error {
	switch {
	case output.writeErr != nil:
		return output.writeErr
	case output.syncErr != nil:
		return output.syncErr
	case output.validateErr != nil:
		return output.validateErr
	default:
		return output.closeErr
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

func TestReadPrivateKeyRejectsSymlinkParent(t *testing.T) {
	dir := t.TempDir()
	targetDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(targetDirectory, "key.txt"), []byte("data"), 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	parent := filepath.Join(dir, "keys")
	if err := os.Symlink(targetDirectory, parent); err != nil {
		t.Fatalf("Symlink error = %v", err)
	}
	_, err := readPrivateKey(filepath.Join(parent, "key.txt"))
	if err == nil {
		t.Fatal("readPrivateKey error = nil, want symlink-parent rejection error")
	}
}

func TestReadPrivateKeyRejectsReplacedParent(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "keys")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("Mkdir error = %v", err)
	}
	keyPath := filepath.Join(parent, "key.txt")
	if err := os.WriteFile(keyPath, []byte("original"), 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "key.txt"), []byte("replacement"), 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	movedParent := filepath.Join(dir, "keys-original")
	if err := os.Rename(parent, movedParent); err != nil {
		t.Fatalf("Rename error = %v", err)
	}
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatalf("Symlink error = %v", err)
	}
	_, err := readPrivateKey(keyPath)
	if err == nil {
		t.Fatal("readPrivateKey error = nil, want replaced-parent rejection error")
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
	var stderr bytes.Buffer
	if code := run([]string{"keygen", "-private", privatePath}, &keygenStdout, &stderr); code != 0 {
		t.Fatalf("run(keygen) = %d, stderr = %q", code, stderr.String())
	}
	publicEncoded := strings.TrimSpace(keygenStdout.String())

	manifest := []byte("abc123 archive.tar.gz\n")
	if err := os.WriteFile(inputPath, manifest, 0o644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	stderr.Reset()
	if code := run([]string{"sign",
		"-private", privatePath,
		"-input", inputPath,
		"-output", outputPath,
		"-expected-public", publicEncoded,
	}, &bytes.Buffer{}, &stderr); code != 0 {
		t.Fatalf("run(sign) = %d, stderr = %q", code, stderr.String())
	}

	if _, err := os.Lstat(outputPath); err != nil {
		t.Fatalf("signature not written: %v", err)
	}

	stderr.Reset()
	if code := run([]string{"verify",
		"-public", publicEncoded,
		"-input", inputPath,
		"-signature", outputPath,
	}, &bytes.Buffer{}, &stderr); code != 0 {
		t.Fatalf("run(verify) = %d, stderr = %q", code, stderr.String())
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
