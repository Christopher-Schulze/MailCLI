package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

func rawByteTestRefs(t *testing.T) []string {
	t.Helper()
	server, err := mailref.EncodeServer(mailref.Server{AccountID: "account", MailboxPath: []string{"INBOX"}, UIDValidity: 7, UID: 42})
	if err != nil {
		t.Fatal(err)
	}
	return []string{"msg_ref", server}
}

func TestRawJSONRejectsInvalidUTF8WithoutRepair(t *testing.T) {
	for _, ref := range rawByteTestRefs(t) {
		for _, body := range []string{"\xff", "\xc0\xaf", "\xed\xa0\x80", "\xe2\x82", "\x80"} {
			t.Run(ref+"/"+body, func(t *testing.T) {
				raw := "Subject: private source\r\n\r\n" + body
				code, output, stderr := runProjectionCommand(t, &projectionGateway{raw: raw}, "messages", "raw", "--ref", ref, "--json")
				var response envelope
				if err := json.Unmarshal([]byte(output), &response); err != nil {
					t.Fatal(err)
				}
				if code != 1 || stderr != "" || response.OK || response.Error == nil || response.Error.Code != "raw_source_invalid_utf8" || response.Data.RawSource != nil {
					t.Fatalf("invalid raw bytes silently repaired: code=%d, output=%s, stderr=%s", code, output, stderr)
				}
				if response.Error.Guidance.EffectCertainty != mail.EffectNone || response.Error.Guidance.ReplayAllowed || response.Next == nil || response.Next.Do != "fix_input" || !strings.Contains(response.Error.Guidance.Recovery.Instruction, "--export") || strings.Contains(output, "private source") {
					t.Fatalf("refusal lacks byte-preserving recovery: %s", output)
				}
			})
		}
	}
}

func TestRawJSONRoundTripsValidUTF8Exactly(t *testing.T) {
	for _, ref := range rawByteTestRefs(t) {
		for _, raw := range []string{"", "Subject: Jörg 🌍\r\n\r\n李\u2028\u2029�", "Content-Type: text/plain\r\n\r\n\x00\t\n"} {
			code, output, stderr := runProjectionCommand(t, &projectionGateway{raw: raw}, "messages", "raw", "--ref", ref, "--json")
			var response envelope
			if err := json.Unmarshal([]byte(output), &response); err != nil || code != 0 || stderr != "" || response.Data.RawSource == nil || *response.Data.RawSource != raw {
				t.Fatalf("valid UTF-8 source changed: error=%v, code=%d, output=%s", err, code, output)
			}
		}
	}
}

func TestRawNonUTF8StreamingAndExportRemainExact(t *testing.T) {
	for _, ref := range rawByteTestRefs(t) {
		raw := "Content-Type: application/octet-stream\r\n\r\n\xff\x80\x00\r\n"
		gateway := &projectionGateway{raw: raw}
		code, output, stderr := runProjectionCommand(t, gateway, "messages", "raw", "--ref", ref)
		if code != 0 || stderr != "" || !bytes.Equal([]byte(output), []byte(raw)) {
			t.Fatalf("stream changed bytes: code=%d, output=%q, stderr=%s", code, output, stderr)
		}
		path := filepath.Join(t.TempDir(), "raw.eml")
		code, output, stderr = runProjectionCommand(t, gateway, "messages", "raw", "--ref", ref, "--export", path, "--json")
		if code != 0 || stderr != "" {
			t.Fatalf("byte-exact export failed: code=%d, output=%s, stderr=%s", code, output, stderr)
		}
		assertExportFile(t, path, []byte(raw), output)
		code, _, _ = runProjectionCommand(t, gateway, "messages", "raw", "--ref", ref, "--export", path, "--json")
		retained, err := os.ReadFile(path)
		if code == 0 || err != nil || string(retained) != raw {
			t.Fatalf("existing export overwritten: code=%d, error=%v, content=%q", code, err, retained)
		}
	}
}
