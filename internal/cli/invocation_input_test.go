package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"mailcli/internal/mail"
)

func TestInvocationInputCancellationBeforeDispatch(t *testing.T) {
	for _, mode := range []string{"create", "update", "reply", "forward", "body", "batch"} {
		t.Run(mode, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			original := os.Stdin
			os.Stdin = reader
			writerClosed := false
			t.Cleanup(func() {
				os.Stdin = original
				if !writerClosed {
					if err := writer.Close(); err != nil {
						t.Error(err)
					}
				}
				if err := reader.Close(); err != nil {
					t.Error(err)
				}
			})
			root := t.TempDir()
			gateway := &jsonInputGateway{}
			service := mail.NewServiceWithDraftRoot(gateway, root)
			args := []string{"drafts", "create", "--input", "-", "--json"}
			payload := `{"to":[{"address":"recipient@example.com"}],"body":"Body"}`
			switch mode {
			case "update":
				draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{To: []mail.Recipient{{Address: "recipient@example.com"}}, Body: "Before"}})
				if err != nil {
					t.Fatal(err)
				}
				args = []string{"drafts", "update", "--ref", draft.Ref, "--expected-revision", draft.Revision, "--input", "-", "--json"}
			case "reply", "forward":
				args = []string{"messages", mode, "--ref", jsonInputSourceRef(t), "--input", "-", "--json"}
			case "body":
				args = []string{"drafts", "create", "--body-file", "-", "--json"}
				payload = "Body"
			case "batch":
				args = []string{"batch", "--input", "-", "--json"}
				payload = `{"operation":"mark","items":[{"id":"one","ref":"` + jsonInputSourceRef(t) + `","read":true}]}`
			}
			if _, err := writer.WriteString(payload); err != nil {
				t.Fatal(err)
			}
			before := jsonInputDirectoryState(t, root)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var stdout, stderr bytes.Buffer
			done := make(chan int, 1)
			go func() { done <- Run(ctx, service, args, &stdout, &stderr) }()
			select {
			case <-done:
				t.Fatal("input unexpectedly completed before EOF or cancellation")
			case <-time.After(25 * time.Millisecond):
			}
			cancel()
			var code int
			select {
			case code = <-done:
				writerClosed = true
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			case <-time.After(500 * time.Millisecond):
				writerClosed = true
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				<-done
				t.Fatal("cancellation left invocation blocked on an open pipe")
			}
			var response envelope
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || code != 1 || stderr.Len() != 0 || response.Error == nil || response.Error.Code != "operation_canceled" {
				t.Fatalf("cancellation result: code=%d, response=%+v, parse error=%v", code, response.Error, err)
			}
			if gateway.calls.Load() != 0 || !reflect.DeepEqual(before, jsonInputDirectoryState(t, root)) {
				t.Fatal("canceled input started gateway work or changed draft storage")
			}
			if _, err := reader.Stat(); err != nil {
				t.Fatalf("invocation closed borrowed stdin: %v", err)
			}
		})
	}
}

func TestInvocationInputDelayedPipePreservesContent(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdin
	os.Stdin = reader
	t.Cleanup(func() {
		os.Stdin = original
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	type result struct {
		payload []byte
		err     error
	}
	done := make(chan result, 1)
	go func() {
		payload, err := readInvocationInput(ctx, "-", 64)
		done <- result{payload, err}
	}()
	select {
	case got := <-done:
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
		t.Fatalf("input ended while waiting: %v", got.err)
	case <-time.After(150 * time.Millisecond):
	}
	_, writeErr := writer.WriteString("delayed input")
	closeErr := writer.Close()
	got := <-done
	if err := errors.Join(writeErr, closeErr, got.err); err != nil || string(got.payload) != "delayed input" {
		t.Fatalf("delayed pipe: payload=%q, err=%v", got.payload, err)
	}
}

func TestInvocationInputRegularFilesAndUnsafePaths(t *testing.T) {
	root := t.TempDir()
	regular := filepath.Join(root, "regular")
	if err := os.WriteFile(regular, []byte("regular bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "symlink")
	if err := os.Symlink(regular, symlink); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, path string
		invalid    bool
	}{
		{name: "regular", path: regular}, {name: "regular symlink", path: symlink},
		{name: "directory", path: root, invalid: true}, {name: "FIFO", path: fifo, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			payload, err := readInvocationInput(ctx, test.path, 64)
			if !test.invalid {
				if err != nil || string(payload) != "regular bytes" {
					t.Fatalf("regular input: %q, %v", payload, err)
				}
				return
			}
			var invalid *commandError
			if !errors.As(err, &invalid) || invalid.code != "invalid_input" || ctx.Err() != nil {
				t.Fatalf("unsafe input did not fail promptly: %v", err)
			}
		})
	}
}

func TestInvocationInputPreservesCancellationAndReadFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readInvocationInput(ctx, "/must-not-be-opened", 64); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(reader.Close(), writer.Close()); err != nil {
		t.Fatal(err)
	}
	original := os.Stdin
	os.Stdin = reader
	t.Cleanup(func() { os.Stdin = original })
	if _, err := readInvocationInput(context.Background(), "-", 64); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed input error lost: %v", err)
	}
}
