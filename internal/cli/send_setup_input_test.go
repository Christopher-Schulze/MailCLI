package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

type setupObservedReader struct {
	io.Reader
	reads int
	err   error
	after func()
}

func (r *setupObservedReader) Read(p []byte) (int, error) {
	r.reads++
	n, err := r.Reader.Read(p)
	if r.after != nil {
		r.after()
	}
	if r.err != nil {
		err = r.err
	}
	return n, err
}

func TestSendSetupValidatesBindingBeforePasswordIO(t *testing.T) {
	for _, mode := range []string{"provider mismatch", "alias limit", "binding limit"} {
		t.Run(mode, func(t *testing.T) {
			store := mail.NewAccountBindingStore(filepath.Join(t.TempDir(), "bindings.json"))
			if err := store.UpdateAccountBindings(context.Background(), func(doc mail.AccountBindingFile) (mail.AccountBindingFile, error) {
				if mode == "alias limit" {
					aliases := make([]string, 64)
					for i := range aliases {
						aliases[i] = fmt.Sprintf("alias%d@icloud.com", i)
					}
					doc.Bindings = []mail.AccountBinding{{AccountID: "ACCOUNT-NEW", SenderAliases: aliases, CredentialAccount: "login@icloud.com"}}
				}
				if mode == "binding limit" {
					for i := 0; i < 128; i++ {
						doc.Bindings = append(doc.Bindings, mail.AccountBinding{AccountID: fmt.Sprintf("ACCOUNT-%d", i), SenderAliases: []string{"known@icloud.com"}, CredentialAccount: "login@icloud.com"})
					}
				}
				return doc, nil
			}); err != nil {
				t.Fatal(err)
			}
			ref, err := mailref.EncodeAccount("ACCOUNT-NEW")
			if err != nil {
				t.Fatal(err)
			}
			reader := &setupObservedReader{Reader: strings.NewReader("PRIVATE_PASSWORD\n")}
			credentials := newStubSetupCredentials()
			previousCredentials, previousStdin := sendSetupCredentials, sendSetupStdin
			sendSetupCredentials = func() transport.CredentialStore { return credentials }
			sendSetupStdin = reader
			t.Cleanup(func() { sendSetupCredentials, sendSetupStdin = previousCredentials, previousStdin })
			args := []string{"--from", "new@icloud.com", "--account", ref, "--json"}
			if mode == "provider mismatch" {
				args = append(args, "--credential-account", "login@gmail.com")
			}
			var stdout, stderr bytes.Buffer
			code := runSendSetup(context.Background(), args, &stdout, &stderr, nil, store, nil)
			if code == 0 || reader.reads != 0 || len(credentials.stored) != 0 {
				t.Fatalf("invalid plan touched password IO: code=%d reads=%d stored=%d output=%s", code, reader.reads, len(credentials.stored), &stdout)
			}
			assertSendSetupNoEffectError(t, &stdout)
		})
	}
}

func TestSendSetupRejectsPasswordReadFailureAndOversize(t *testing.T) {
	for _, test := range []struct {
		name, input string
		err         error
	}{
		{"read failure with data", "PRIVATE_PASSWORD", errors.New("input failure")},
		{"oversized line", strings.Repeat("x", 4096) + "\n", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			credentials := newStubSetupCredentials()
			previousCredentials, previousStdin := sendSetupCredentials, sendSetupStdin
			sendSetupCredentials = func() transport.CredentialStore { return credentials }
			sendSetupStdin = &setupObservedReader{Reader: strings.NewReader(test.input), err: test.err}
			t.Cleanup(func() { sendSetupCredentials, sendSetupStdin = previousCredentials, previousStdin })
			var stdout, stderr bytes.Buffer
			code := runSendSetup(context.Background(), []string{"--from", "alice@icloud.com", "--json"}, &stdout, &stderr, nil, setupTestBindings(t), setupAccountLister(t, "alice@icloud.com"))
			if code == 0 || len(credentials.stored) != 0 || strings.Contains(stdout.String()+stderr.String(), "PRIVATE_PASSWORD") {
				t.Fatalf("invalid password input was stored or exposed: code=%d stored=%d output=%s", code, len(credentials.stored), &stdout)
			}
			assertSendSetupNoEffectError(t, &stdout)
		})
	}
}

func TestSendSetupCancellationPreventsCredentialMutation(t *testing.T) {
	for _, mode := range []string{"store", "remove", "during read"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			credentials := newStubSetupCredentials()
			credentials.stored["alice@icloud.com"] = "original"
			reader := &setupObservedReader{Reader: strings.NewReader("PRIVATE_PASSWORD\n")}
			if mode == "during read" {
				reader.after = cancel
			} else {
				cancel()
			}
			previousCredentials, previousStdin := sendSetupCredentials, sendSetupStdin
			sendSetupCredentials = func() transport.CredentialStore { return credentials }
			sendSetupStdin = reader
			t.Cleanup(func() { sendSetupCredentials, sendSetupStdin = previousCredentials, previousStdin })
			args := []string{"--from", "alice@icloud.com", "--json"}
			if mode == "remove" {
				args = append(args, "--remove")
			}
			var stdout, stderr bytes.Buffer
			code := runSendSetup(ctx, args, &stdout, &stderr, nil, setupTestBindings(t), setupAccountLister(t, "alice@icloud.com"))
			var response envelope
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || code != 1 || response.Error == nil || response.Error.Code != "operation_canceled" || credentials.stored["alice@icloud.com"] != "original" {
				t.Fatalf("canceled setup = code %d, error %v, response %s, credential changed %t", code, err, &stdout, credentials.stored["alice@icloud.com"] != "original")
			}
			if mode != "during read" && reader.reads != 0 {
				t.Fatal("canceled setup consumed password input")
			}
			assertSendSetupNoEffectError(t, &stdout)
		})
	}
}

func TestSendSetupPipeCancellationPreservesBorrowedInput(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	})
	fd, err := sendPasswordInputDescriptor(reader)
	if err != nil {
		t.Fatal(err)
	}
	before, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, "partial"); err != nil {
		t.Fatal(err)
	}
	credentials := newStubSetupCredentials()
	previousCredentials, previousStdin := sendSetupCredentials, sendSetupStdin
	sendSetupCredentials = func() transport.CredentialStore { return credentials }
	sendSetupStdin = reader
	t.Cleanup(func() { sendSetupCredentials, sendSetupStdin = previousCredentials, previousStdin })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var stdout, stderr bytes.Buffer
	started := time.Now()
	code := runSendSetup(ctx, []string{"--from", "alice@icloud.com", "--json"}, &stdout, &stderr, nil, setupTestBindings(t), setupAccountLister(t, "alice@icloud.com"))
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || code != 1 || response.Error == nil || response.Error.Code != "operation_timeout" || len(credentials.stored) != 0 {
		t.Fatalf("pipe cancellation = code %d, error %v, response %s", code, err, &stdout)
	}
	if time.Since(started) > time.Second {
		t.Fatal("pipe cancellation did not stop the input wait promptly")
	}
	after, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil || before != after {
		t.Fatalf("borrowed descriptor flags changed: %d -> %d, error %v", before, after, err)
	}
	if _, err := reader.Stat(); err != nil {
		t.Fatalf("borrowed input closed: %v", err)
	}
	assertSendSetupNoEffectError(t, &stdout)
}

func TestSendPasswordLineBoundariesAndCauses(t *testing.T) {
	failure := errors.New("read failed")
	for _, test := range []struct {
		name  string
		input io.Reader
		want  string
		cause error
	}{
		{"EOF without newline", strings.NewReader("secret"), "secret", nil},
		{"exact limit at EOF", strings.NewReader(strings.Repeat("x", 4096)), strings.Repeat("x", 4096), nil},
		{"exact limit with newline", strings.NewReader(strings.Repeat("x", 4095) + "\n"), strings.Repeat("x", 4095), nil},
		{"partial data and read error", &setupObservedReader{Reader: strings.NewReader("secret"), err: failure}, "", failure},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := readSendPasswordLine(context.Background(), test.input)
			if got != test.want || !errors.Is(err, test.cause) {
				t.Fatalf("line length=%d, want=%d, error=%v", len(got), len(test.want), err)
			}
		})
	}
	reader := strings.NewReader("secret\r\nSECOND\n")
	got, err := readSendPasswordLine(context.Background(), reader)
	rest, readErr := io.ReadAll(reader)
	if got != "secret" || err != nil || readErr != nil || string(rest) != "SECOND\n" {
		t.Fatalf("line consumed subsequent input: remaining=%q, error=%v, read=%v", rest, err, readErr)
	}
}

func TestSendSetupCancellationAfterStoreRetainsPartialEffect(t *testing.T) {
	store := mail.NewAccountBindingStore(filepath.Join(t.TempDir(), "bindings.json"))
	ref, err := mailref.EncodeAccount("ACCOUNT-NEW")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	credentials := newStubSetupCredentials()
	previousCredentials, previousStdin := sendSetupCredentials, sendSetupStdin
	sendSetupCredentials = func() transport.CredentialStore { return credentials }
	sendSetupStdin = strings.NewReader("secret\n")
	t.Cleanup(func() { sendSetupCredentials, sendSetupStdin = previousCredentials, previousStdin })
	var stdout, stderr bytes.Buffer
	code := runSendSetup(ctx, []string{"--from", "alice@icloud.com", "--account", ref, "--json"}, &stdout, &stderr, func(string) { cancel() }, store, nil)
	var response struct {
		Data struct {
			PartialEffects []sendSetupPartialEffect `json:"partial_effects"`
		} `json:"data"`
		Error *errorData `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || code != 1 || response.Error == nil || response.Error.Code != "operation_canceled" || credentials.stored["alice@icloud.com"] != "secret" {
		t.Fatalf("post-store cancellation = code %d, error %v, response %s", code, err, &stdout)
	}
	if len(response.Data.PartialEffects) != 2 || response.Data.PartialEffects[0].Status != mail.AccountBindingPublicationComplete || response.Data.PartialEffects[1].Status != mail.AccountBindingPublicationNone {
		t.Fatalf("post-store effects = %+v", response.Data.PartialEffects)
	}
	document, err := store.LoadAccountBindings()
	if err != nil || len(document.Bindings) != 0 {
		t.Fatalf("canceled binding was published: count=%d, error=%v", len(document.Bindings), err)
	}
}
