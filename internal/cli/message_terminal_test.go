package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

func TestTerminalTextControlPolicy(t *testing.T) {
	for _, multiline := range []bool{false, true} {
		t.Run(fmt.Sprintf("multiline=%t", multiline), func(t *testing.T) {
			for character := rune(0); character <= 0x9f; character++ {
				if character > 0x1f && character < 0x7f {
					continue
				}
				want := " "
				if multiline && (character == '\n' || character == '\t') {
					want = string(character)
				}
				if got := terminalText(string(character), multiline); got != want {
					t.Fatalf("U+%04X = %q, want %q", character, got, want)
				}
			}
			if got := terminalText("左\u2028中\u2029右", multiline); got != "左 中 右" {
				t.Fatalf("separators = %q", got)
			}
			const ordinary = "Jörg e\u0301 日本語 👩‍💻 العربية"
			if got := terminalText(ordinary, multiline); got != ordinary {
				t.Fatalf("Unicode changed: %q", got)
			}
		})
	}
}

func terminalDetailMessage() mail.Message {
	const value = "Jörg\x1b[2J\x07\r\n\t\u009b\u2028\u2029末"
	return mail.Message{
		Summary: mail.MessageSummary{
			Ref: value, Sender: value, Subject: value, DateReceived: value,
		},
		To:  []mail.Recipient{{Name: value, Address: value}, {Address: value}},
		CC:  []mail.Recipient{{Name: value, Address: value}},
		BCC: []mail.Recipient{{Name: value, Address: value}},
		Content: "日本語\n\t👩‍💻\n\n" +
			"\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\\n" +
			"\x1b]52;c;YQ==\x07\rchanged\b\u009b2J\u009d52;c;Yg==\u009c終",
		ContentSource: value,
		MissingParts:  []string{value, value},
		Hydration: &mail.HydrationDiagnostic{
			State: mail.HydrationState(value), AttemptedSource: value,
			Local:       &mail.HydrationCause{Code: value, Message: value},
			Remote:      &mail.HydrationCause{Code: value, Message: value},
			Remediation: value,
		},
	}
}

func TestMessageDetailsSanitizeEveryFieldWithoutChangingMessage(t *testing.T) {
	message := terminalDetailMessage()
	before, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := writeMessage(&output, message); err != nil {
		t.Fatal(err)
	}
	const clean = "Jörg [2J       末"
	want := strings.Join([]string{
		"Ref: " + clean, "From: " + clean,
		"To: " + clean + " <" + clean + ">, " + clean,
		"CC: " + clean + " <" + clean + ">",
		"BCC: " + clean + " <" + clean + ">",
		"Subject: " + clean, "Date received: " + clean, "Attachments: 0",
		"Content source: " + clean, "Content complete: false",
		"Missing parts: " + clean + ", " + clean,
		"Hydration state: " + clean, "Hydration source: " + clean,
		"Hydration local: " + clean + ": " + clean,
		"Hydration remote: " + clean + ": " + clean,
		"Hydration remediation: " + clean, "",
		"日本語\n\t👩‍💻\n\n ]8;;https://example.com \\link ]8;; \\\n" +
			" ]52;c;YQ==  changed  2J 52;c;Yg== 終",
	}, "\n")
	if output.String() != want {
		t.Fatalf("details = %q, want %q", output.String(), want)
	}
	after, err := json.Marshal(message)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("render mutated message: before=%s after=%s error=%v", before, after, err)
	}
}

func TestMessageDetailRoutesPreserveJSONAndSanitizeHumanErrors(t *testing.T) {
	routes := []struct {
		name string
		args []string
	}{
		{"get", []string{"messages", "get", "--ref", "msg_ref"}},
		{"open", []string{"drafts", "open", "--message", "msg_ref"}},
	}
	for _, route := range routes {
		for _, state := range []string{"complete", "partial", "unavailable"} {
			for _, jsonOutput := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/json=%t", route.name, state, jsonOutput), func(t *testing.T) {
					message := terminalDetailMessage()
					var readErr error
					wantCode := 0
					if state != "complete" {
						readErr = &testCodedError{code: "imap_timeout", message: "read\x1b[2J\rfailed\x07"}
						wantCode = 1
					}
					if state != "partial" {
						message.Hydration = nil
					}
					args := append([]string(nil), route.args...)
					if jsonOutput {
						args = append(args, "--json", "--view", "full")
					}
					var stdout, stderr bytes.Buffer
					service := mail.NewService(hydrationMessageGateway{message: message, err: readErr})
					code := Run(context.Background(), service, args, &stdout, &stderr)
					if code != wantCode {
						t.Fatalf("exit=%d stdout=%q stderr=%q", code, &stdout, &stderr)
					}
					if jsonOutput {
						checkTerminalMessageJSON(t, stdout.Bytes(), stderr.String(), message, state)
						return
					}
					for _, output := range []string{stdout.String(), stderr.String()} {
						if strings.ContainsAny(output, "\x1b\x07\r\b\u009b\u009d\u009c\u2028\u2029") {
							t.Fatalf("unsafe human output = %q", output)
						}
					}
					if state != "unavailable" && !strings.Contains(stdout.String(), "日本語\n\t👩‍💻\n\n") {
						t.Fatalf("body layout lost: %q", &stdout)
					}
					if state == "partial" && !strings.Contains(stderr.String(), "incomplete; Jörg [2J       末") {
						t.Fatalf("hydration diagnostic lost: %q", &stderr)
					}
					if state == "unavailable" && stderr.String() != "read [2J failed \n" {
						t.Fatalf("read diagnostic = %q", &stderr)
					}
				})
			}
		}
	}
}

func checkTerminalMessageJSON(t *testing.T, output []byte, stderr string, message mail.Message, state string) {
	t.Helper()
	var response envelope
	if err := json.Unmarshal(output, &response); err != nil || stderr != "" {
		t.Fatalf("JSON error=%v stderr=%q output=%q", err, stderr, output)
	}
	if response.Data.Message == nil || !reflect.DeepEqual(*response.Data.Message, message) {
		t.Fatalf("JSON message changed: %+v, want %+v", response.Data.Message, message)
	}
	if state == "unavailable" && (response.Error == nil || response.Error.Message != "read\x1b[2J\rfailed\x07") {
		t.Fatalf("JSON error changed: %+v", response.Error)
	}
}

type terminalFailWriter struct {
	calls  int
	failAt int
}

func (w *terminalFailWriter) Write(value []byte) (int, error) {
	w.calls++
	if w.calls == w.failAt {
		return 0, io.ErrClosedPipe
	}
	return len(value), nil
}

func TestMessageDetailsPropagateEveryWriteFailure(t *testing.T) {
	for position := 1; position <= 18; position++ {
		t.Run(fmt.Sprintf("write-%d", position), func(t *testing.T) {
			writer := &terminalFailWriter{failAt: position}
			err := writeMessage(writer, terminalDetailMessage())
			if !errors.Is(err, io.ErrClosedPipe) || writer.calls != position {
				t.Fatalf("write=%d calls=%d error=%v", position, writer.calls, err)
			}
		})
	}
}
