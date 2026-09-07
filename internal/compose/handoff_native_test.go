package compose

import (
	"context"
	"strings"
	"testing"
)

func TestHandoffSuccessWithoutAppKit(t *testing.T) {
	original := invokeNativeCompose
	t.Cleanup(func() { invokeNativeCompose = original })
	invokeNativeCompose = func(payload string) (string, error) {
		if !strings.Contains(payload, "a@example.com") {
			t.Fatalf("payload = %s, want recipient", payload)
		}
		return `{"ok":true,"opened":true,"mail_application":"com.apple.mail"}`, nil
	}

	result, err := Handoff(context.Background(), Request{Recipients: []string{"a@example.com"}})
	if err != nil {
		t.Fatalf("Handoff() error = %v", err)
	}
	if !result.Opened || result.MailApplication != "com.apple.mail" {
		t.Errorf("Handoff() = %#v", result)
	}
}

func TestHandoffEscapesEmbeddedNULBeforeNativeCall(t *testing.T) {
	original := invokeNativeCompose
	t.Cleanup(func() { invokeNativeCompose = original })
	invokeNativeCompose = func(payload string) (string, error) {
		if strings.Contains(payload, "\x00") {
			t.Fatal("native payload contains an embedded NUL")
		}
		if !strings.Contains(payload, `\u0000`) {
			t.Fatalf("payload = %s, want JSON NUL escape", payload)
		}
		return `{"ok":true,"opened":true}`, nil
	}

	if _, err := Handoff(context.Background(), Request{Subject: "before\x00after"}); err != nil {
		t.Fatalf("Handoff() error = %v", err)
	}
}

func TestHandoffNativeErrorWithoutAppKit(t *testing.T) {
	original := invokeNativeCompose
	t.Cleanup(func() { invokeNativeCompose = original })
	invokeNativeCompose = func(string) (string, error) {
		return "", context.DeadlineExceeded
	}

	if _, err := Handoff(context.Background(), Request{Recipients: []string{"a@example.com"}}); err != context.DeadlineExceeded {
		t.Errorf("Handoff() error = %v, want deadline", err)
	}
}

func TestHandoffNativeFailureJSONWithoutAppKit(t *testing.T) {
	original := invokeNativeCompose
	t.Cleanup(func() { invokeNativeCompose = original })
	invokeNativeCompose = func(string) (string, error) {
		return `{"ok":false,"code":"compose_failed","message":"native failed"}`, nil
	}

	_, err := Handoff(context.Background(), Request{Recipients: []string{"a@example.com"}})
	if err == nil {
		t.Fatal("Handoff() error = nil")
	}
	composeErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if composeErr.Code != "compose_failed" {
		t.Errorf("Code = %q", composeErr.Code)
	}
}
