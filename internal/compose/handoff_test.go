package compose

import (
	"encoding/json"
	"testing"
)

func TestParseHandoffResponseSuccess(t *testing.T) {
	raw := `{"ok":true,"opened":true,"mail_application":"com.apple.mail"}`
	result, err := parseHandoffResponse(raw)
	if err != nil {
		t.Fatalf("parseHandoffResponse error = %v", err)
	}
	if !result.Opened {
		t.Error("Opened = false, want true")
	}
	if result.MailApplication != "com.apple.mail" {
		t.Errorf("MailApplication = %q, want com.apple.mail", result.MailApplication)
	}
}

func TestParseHandoffResponseSuccessNoOpen(t *testing.T) {
	raw := `{"ok":true,"opened":false}`
	result, err := parseHandoffResponse(raw)
	if err != nil {
		t.Fatalf("parseHandoffResponse error = %v", err)
	}
	if result.Opened {
		t.Error("Opened = true, want false")
	}
}

func TestParseHandoffResponseError(t *testing.T) {
	raw := `{"ok":false,"code":"compose_not_default_mailto","message":"Mail.app is not the default email application"}`
	_, err := parseHandoffResponse(raw)
	if err == nil {
		t.Fatal("parseHandoffResponse error = nil, want compose error")
	}
	composeErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if composeErr.Code != "compose_not_default_mailto" {
		t.Errorf("Code = %q, want compose_not_default_mailto", composeErr.Code)
	}
	if composeErr.Message != "Mail.app is not the default email application" {
		t.Errorf("Message = %q, want default mailto message", composeErr.Message)
	}
}

func TestParseHandoffResponseInvalidJSON(t *testing.T) {
	_, err := parseHandoffResponse("not json")
	if err == nil {
		t.Fatal("parseHandoffResponse error = nil, want json decode error")
	}
}

func TestParseHandoffResponseEmptyString(t *testing.T) {
	_, err := parseHandoffResponse("")
	if err == nil {
		t.Fatal("parseHandoffResponse error = nil, want json decode error on empty input")
	}
}

func TestErrorError(t *testing.T) {
	e := &Error{Code: "test_code", Message: "test message"}
	got := e.Error()
	if got != "test message" {
		t.Errorf("Error() = %q, want test message", got)
	}
}

func TestErrorErrorCode(t *testing.T) {
	e := &Error{Code: "compose_failed", Message: "failed"}
	if got := e.ErrorCode(); got != "compose_failed" {
		t.Errorf("ErrorCode() = %q, want compose_failed", got)
	}
}

func TestRequestJSONRoundtrip(t *testing.T) {
	req := Request{
		Recipients:  []string{"recipient@example.com"},
		Subject:     "Test Subject",
		PlainBody:   "Hello world",
		HTMLBody:    "<p>Hello world</p>",
		Attachments: []string{"/tmp/file.pdf"},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal error = %v", err)
	}
	if len(data) == 0 {
		t.Fatal("marshaled data is empty")
	}
}
