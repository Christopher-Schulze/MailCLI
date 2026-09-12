package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

func TestDraftPartialUpdateFormatPreservesReviewedContent(t *testing.T) {
	for _, format := range []mail.DraftBodyFormat{mail.DraftBodyPlain, mail.DraftBodyHTML, mail.DraftBodyMarkdown} {
		for _, mode := range []string{"flags", "json"} {
			for _, change := range []string{"omitted", "same", "different", "replacement", "empty", "stale", "invalid"} {
				t.Run(string(format)+"/"+mode+"/"+change, func(t *testing.T) {
					service := mail.NewServiceWithDraftRoot(nil, t.TempDir())
					attachment := filepath.Join(t.TempDir(), "kept.txt")
					if err := os.WriteFile(attachment, []byte("kept"), 0o600); err != nil {
						t.Fatal(err)
					}
					input := mail.DraftInput{
						To: []mail.Recipient{{Address: "to@example.com"}}, CC: []mail.Recipient{{Address: "cc@example.com"}},
						BCC: []mail.Recipient{{Address: "bcc@example.com"}}, Subject: "Before", BodyFormat: format,
						Body: "<p>HTML</p>\n\n**Markdown**", Attachments: []string{attachment},
					}
					draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: input})
					if err != nil {
						t.Fatal(err)
					}
					args := partialFormatArguments(t, mode, change, draft)
					var stdout, stderr bytes.Buffer
					code := Run(context.Background(), service, args, &stdout, &stderr)
					stored, err := service.GetDraft(draft.Ref)
					if err != nil {
						t.Fatal(err)
					}
					if change == "different" || change == "stale" || change == "invalid" {
						if code == 0 || !reflect.DeepEqual(draft, stored) {
							t.Fatalf("rejected input changed state: code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
						}
						if change == "different" && !strings.Contains(stdout.String(), "requires a new body") {
							t.Fatalf("missing format-change guidance: %s", &stdout)
						}
						if change == "stale" && !strings.Contains(stdout.String(), `"code":"draft_revision_conflict"`) {
							t.Fatalf("revision protection lost: %s", &stdout)
						}
						return
					}
					if code != 0 || stderr.Len() != 0 || stored.Subject != "After" || stored.Revision == draft.Revision {
						t.Fatalf("partial update failed: code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
					}
					if !reflect.DeepEqual(stored.To, draft.To) || !reflect.DeepEqual(stored.CC, draft.CC) ||
						!reflect.DeepEqual(stored.BCC, draft.BCC) || !reflect.DeepEqual(stored.Attachments, draft.Attachments) {
						t.Fatal("partial update changed recipients or attachments")
					}
					switch change {
					case "omitted", "same":
						if stored.BodyFormat != draft.BodyFormat || stored.Body != draft.Body || stored.BodySource != draft.BodySource || stored.BodyHTML != draft.BodyHTML {
							t.Fatal("same-format update changed a stored body representation")
						}
					case "replacement":
						if stored.BodyFormat == draft.BodyFormat || stored.Body != "Replacement" {
							t.Fatalf("explicit source not converted: %+v", stored)
						}
					case "empty":
						if stored.Body != "" || stored.BodySource != "" || stored.BodyHTML != "" {
							t.Fatal("explicit empty body did not clear content")
						}
					}
				})
			}
		}
	}
}

func partialFormatArguments(t *testing.T, mode, change string, draft mail.Draft) []string {
	t.Helper()
	format := draft.BodyFormat
	if change == "different" || change == "replacement" {
		format = mail.DraftBodyPlain
		if draft.BodyFormat == mail.DraftBodyPlain {
			format = mail.DraftBodyMarkdown
		}
	}
	if change == "invalid" {
		format = "unsupported"
	}
	revision := draft.Revision
	if change == "stale" {
		revision = "stale"
	}
	args := []string{"drafts", "update", "--ref", draft.Ref, "--expected-revision", revision, "--json"}
	body := ""
	if change == "replacement" {
		body = "Replacement"
	}
	if mode == "flags" {
		args = append(args, "--subject", "After")
		if change != "omitted" {
			args = append(args, "--format", " "+strings.ToUpper(string(format))+" ")
		}
		if change == "replacement" || change == "empty" {
			args = append(args, "--body", body)
		}
		return args
	}
	fields := map[string]string{"subject": "After"}
	if change != "omitted" {
		fields["body_format"] = string(format)
	}
	if change == "replacement" || change == "empty" {
		fields["body"] = body
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "patch.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return append(args, "--input", path)
}

func TestPartialUpdatePublishedFormatContract(t *testing.T) {
	schema := decodeTestCommandSchema(t, schemaForCommand("drafts.update"))
	body := jsonFieldByName(schema.JSONInput.Fields, "body")
	if body == nil || body.Required {
		t.Fatal("partial update schema requires a body")
	}
	if schemaHasConstraint(schema, "one_of") || !schemaHasConstraint(schema, "conditional") {
		t.Fatal("partial update schema lost conditional format requirements")
	}
	for _, constraint := range schema.JSONInput.Constraints {
		if constraint.Kind == "required" {
			t.Fatalf("partial update publishes unconditional required fields: %+v", constraint)
		}
	}
	create := decodeTestCommandSchema(t, schemaForCommand("drafts.create"))
	if body := jsonFieldByName(create.JSONInput.Fields, "body"); body == nil || !body.Required {
		t.Fatal("creation body requirement changed with update semantics")
	}
}

func TestPartialUpdatePreservesExistingJSONFormatDefaults(t *testing.T) {
	for _, value := range []string{`""`, "null"} {
		input, err := decodeDraftInputMode(strings.NewReader(`{"subject":"After","body_format":`+value+`}`), false)
		if err != nil {
			t.Fatal(err)
		}
		for _, format := range []mail.DraftBodyFormat{mail.DraftBodyPlain, mail.DraftBodyHTML, mail.DraftBodyMarkdown} {
			current := mail.Draft{Body: "Body", BodyFormat: format, BodySource: "Source"}
			if format == mail.DraftBodyPlain {
				current.BodySource = ""
			}
			merged, err := mergeDraftUpdateInput(current, input)
			if format == mail.DraftBodyPlain {
				if err != nil || merged.Body != "Body" {
					t.Fatalf("existing plain default rejected: %+v %v", merged, err)
				}
			} else if err == nil {
				t.Fatal("rich-to-default-plain change accepted without source")
			}
		}
	}
}
