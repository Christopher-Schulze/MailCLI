package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mailcli/internal/releaseauth"
)

const maximumSigningInputBytes = 1024 * 1024

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		if _, err := fmt.Fprintln(stderr, "Usage: mailcli-release-sign <keygen|sign|verify> [options]"); err != nil {
			return 1
		}
		return 2
	}
	var err error
	switch args[0] {
	case "keygen":
		err = runKeygen(args[1:], stdout, stderr)
	case "sign":
		err = runSign(args[1:], stderr)
	case "verify":
		err = runVerify(args[1:], stderr)
	default:
		if _, err := fmt.Fprintf(stderr, "unknown command %q\n", args[0]); err != nil {
			return 1
		}
		return 2
	}
	if err != nil {
		if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
			return 1
		}
		return 1
	}
	return 0
}

func runKeygen(args []string, stdout io.Writer, stderr io.Writer) error {
	flags := flag.NewFlagSet("keygen", flag.ContinueOnError)
	flags.SetOutput(stderr)
	privatePath := flags.String("private", "", "absolute private-key output path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !filepath.IsAbs(*privatePath) {
		return fmt.Errorf("keygen requires an absolute --private path")
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate Ed25519 key: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(privateKey) + "\n"
	if err := writeExclusive(*privatePath, []byte(encoded), 0o600); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}
	_, err = fmt.Fprintln(stdout, base64.StdEncoding.EncodeToString(publicKey))
	return err
}

func runSign(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("sign", flag.ContinueOnError)
	flags.SetOutput(stderr)
	privatePath := flags.String("private", "", "absolute private-key path")
	inputPath := flags.String("input", "", "absolute manifest path")
	outputPath := flags.String("output", "", "absolute signature output path")
	expectedPublic := flags.String("expected-public", releaseauth.PublicKeyBase64, "expected base64 public key")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !filepath.IsAbs(*privatePath) || !filepath.IsAbs(*inputPath) || !filepath.IsAbs(*outputPath) {
		return fmt.Errorf("sign requires absolute --private, --input, and --output paths")
	}
	privateKey, err := readPrivateKey(*privatePath)
	if err != nil {
		return err
	}
	defer clearPrivateKey(privateKey)
	if err := validatePublicKey(privateKey, *expectedPublic); err != nil {
		return err
	}
	manifest, err := readBoundedFile(*inputPath, maximumSigningInputBytes)
	if err != nil {
		return fmt.Errorf("read signing input: %w", err)
	}
	signature := ed25519.Sign(privateKey, manifest)
	encoded := base64.StdEncoding.EncodeToString(signature) + "\n"
	if err := writeExclusive(*outputPath, []byte(encoded), 0o644); err != nil {
		return fmt.Errorf("write signature: %w", err)
	}
	return nil
}

func validatePublicKey(privateKey ed25519.PrivateKey, expected string) error {
	decoded, err := decodePublicKey(expected)
	if err != nil {
		return err
	}
	actual, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok || !actual.Equal(decoded) {
		return fmt.Errorf("private key does not match the expected release public key")
	}
	return nil
}

func runVerify(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	publicValue := flags.String("public", releaseauth.PublicKeyBase64, "base64 public key")
	inputPath := flags.String("input", "", "absolute manifest path")
	signaturePath := flags.String("signature", "", "absolute signature path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !filepath.IsAbs(*inputPath) || !filepath.IsAbs(*signaturePath) {
		return fmt.Errorf("verify requires absolute --input and --signature paths")
	}
	publicKey, err := decodePublicKey(*publicValue)
	if err != nil {
		return err
	}
	manifest, err := readBoundedFile(*inputPath, maximumSigningInputBytes)
	if err != nil {
		return fmt.Errorf("read signing input: %w", err)
	}
	encoded, err := readBoundedFile(*signaturePath, 1024)
	if err != nil {
		return fmt.Errorf("read signature: %w", err)
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(publicKey, manifest, signature) {
		return fmt.Errorf("signature does not match the release public key")
	}
	return nil
}

func decodePublicKey(value string) (ed25519.PublicKey, error) {
	decoded, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("expected public key is not valid base64 Ed25519 data")
	}
	return ed25519.PublicKey(decoded), nil
}

func readPrivateKey(path string) (ed25519.PrivateKey, error) {
	file, info, err := openPrivateKeyFile(path)
	if err != nil {
		return nil, fmt.Errorf("open private key: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		_ = file.Close()
		return nil, fmt.Errorf("private key must be a regular file accessible only by its owner")
	}
	payload, err := readBoundedOpenFile(file, 1024)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	encoded := bytes.TrimSpace(payload)
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(encoded)))
	decodedLength, err := base64.StdEncoding.Strict().Decode(decoded, encoded)
	decoded = decoded[:decodedLength]
	clearBytes(payload)
	if err != nil || len(decoded) != ed25519.PrivateKeySize {
		clearBytes(decoded)
		return nil, fmt.Errorf("private key is not a valid base64 Ed25519 private key")
	}
	privateKey := make(ed25519.PrivateKey, len(decoded))
	copy(privateKey, decoded)
	clearBytes(decoded)
	return privateKey, nil
}

func readBoundedFile(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return readBoundedOpenFile(file, maximum)
}

func readBoundedOpenFile(file *os.File, maximum int64) ([]byte, error) {
	payload, readErr := io.ReadAll(io.LimitReader(file, maximum+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		clearBytes(payload)
		return nil, errors.Join(readErr, closeErr)
	}
	if int64(len(payload)) > maximum {
		clearBytes(payload)
		return nil, fmt.Errorf("file exceeds %d bytes", maximum)
	}
	return payload, nil
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func clearPrivateKey(privateKey ed25519.PrivateKey) {
	clearBytes(privateKey)
}

type exclusiveOutput interface {
	Write([]byte) (int, error)
	Sync() error
	Validate(int64) error
	CloseFile() error
	Cleanup() error
	CloseParent() error
}

func writeExclusive(path string, payload []byte, mode os.FileMode) error {
	output, err := openExclusiveOutput(path, mode)
	if err != nil {
		return err
	}
	return finishExclusiveOutput(output, payload)
}

func finishExclusiveOutput(output exclusiveOutput, payload []byte) error {
	written, writeErr := output.Write(payload)
	var resultErr error
	if writeErr != nil {
		resultErr = writeErr
	}
	if written != len(payload) {
		resultErr = errors.Join(resultErr, io.ErrShortWrite)
	}
	if resultErr == nil {
		resultErr = output.Sync()
	}
	if resultErr == nil {
		resultErr = output.Validate(int64(len(payload)))
	}
	closeErr := output.CloseFile()
	if resultErr == nil && closeErr == nil {
		return output.CloseParent()
	}
	cleanupErr := output.Cleanup()
	parentCloseErr := output.CloseParent()
	return errors.Join(resultErr, closeErr, cleanupErr, parentCloseErr)
}
