package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

func TestHydrationRecoveryPreservesParsedInvocation(t *testing.T) {
	for _, test := range []struct {
		name, family string
		options      []string
		export       bool
	}{
		{"plain", "messages", []string{"--view", "plain"}, false},
		{"full links budget", "messages", []string{"--view", "full", "--links", "none", "--max-bytes", "65536"}, false},
		{"custom excerpt", "messages", []string{"--fields", "header_fields,excerpt", "--excerpt-length", "37"}, false},
		{"positional ref", "messages", []string{"msg_ref", "--view=plain", "--links=host"}, false},
		{"pre export", "messages", []string{"--view", "plain"}, true},
		{"draft plain", "drafts", []string{"--view", "plain", "--max-bytes", "65536"}, false},
		{"draft fields", "drafts", []string{"--fields", "content,headers"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			tail := []string{"--ref", "msg_ref"}
			tail = append(tail, test.options...)
			exportPath := filepath.Join(t.TempDir(), "body.txt")
			if test.export {
				tail = append(tail, "--export", exportPath)
			}
			tail = append(tail, "--json")
			subcommand := "get"
			if test.family == "drafts" {
				subcommand = "open"
			}
			prefix := []string{test.family, subcommand}
			failure := &transport.TransportError{Code: transport.CodeIMAPFetchFailed, Err: io.EOF}
			message := failedHydrationMessage()
			message.Hydration.Remote.Code = transport.CodeIMAPFetchFailed
			service := mail.NewService(hydrationMessageGateway{message: message, err: failure})
			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), service, append(slices.Clone(prefix), tail...), &stdout, &stderr); code != 1 || stderr.Len() != 0 {
				t.Fatalf("failure code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
			}
			var response envelope
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Error == nil || response.Error.Guidance == nil || !slices.Equal(response.Error.Guidance.Recovery.Args, tail) || response.Next == nil || response.Next.Do != "retry" || !slices.Equal(response.Next.Args, tail) {
				t.Fatalf("recovery lost intent: %s", &stdout)
			}
			if test.export {
				if _, err := os.Lstat(exportPath); !os.IsNotExist(err) {
					t.Fatalf("failed hydration created export: %v", err)
				}
			}
			message.ContentComplete, message.Hydration = true, nil
			service = mail.NewService(hydrationMessageGateway{message: message})
			stdout.Reset()
			if code := Run(context.Background(), service, append(slices.Clone(prefix), response.Next.Args...), &stdout, &stderr); code != 0 || stderr.Len() != 0 {
				t.Fatalf("generated arguments rejected: code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
			}
		})
	}
}

func TestCanceledHydrationDoesNotSuggestReplay(t *testing.T) {
	message := failedHydrationMessage()
	message.Hydration.Remote.Code = transport.CodeIMAPFetchFailed
	service := mail.NewService(hydrationMessageGateway{message: message, err: &transport.TransportError{Code: transport.CodeIMAPFetchFailed, Err: context.Canceled}})
	var stdout, stderr bytes.Buffer
	if Run(context.Background(), service, []string{"messages", "get", "--ref", "msg_ref", "--view", "plain", "--json"}, &stdout, &stderr) != 1 {
		t.Fatal("canceled hydration succeeded")
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Next == nil || response.Next.Do != "stop" || response.Next.Command != "" || len(response.Next.Args) != 0 || stderr.Len() != 0 {
		t.Fatalf("canceled recovery: %s", &stdout)
	}
}
