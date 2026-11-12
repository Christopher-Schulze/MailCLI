package mailstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/cli"
	"mailcli/internal/mail"
)

// These are generated capability fixtures, not captures from a newer Mail
// installation. They prove the adapter's policy and reads, not Apple's semantics.
func TestSchemaProfileCompatibilityPolicy(t *testing.T) {
	for _, test := range []struct {
		name      string
		version   string
		minor     string
		framework string
		mutation  string
		accepted  bool
		verified  bool
	}{
		{"verified legacy", "4", "74003", "3826.700.81", "", true, true},
		{"legacy writer update", "4", "74003", "9999.1", "", true, false},
		{"reported compatible profile", "4", "90006", "3901.200.71.1.16", "", true, false},
		{"unknown minor", "4", "99999", "3901.200.71.1.16", "", false, false},
		{"unknown generation", "5", "90006", "3901.200.71.1.16", "", false, false},
		{"unknown compatible writer", "4", "90006", "3901.200.71.1.17", "", false, false},
		{"crossed legacy writer", "4", "90006", "3826.700.81", "", false, false},
		{"missing compatible writer", "4", "90006", "", "", false, false},
		{"missing column", "4", "90006", "3901.200.71.1.16", "ALTER TABLE server_messages DROP COLUMN forwarded", false, false},
		{"missing index", "4", "90006", "3901.200.71.1.16", "DROP INDEX labels_mailbox", false, false},
		{"invalid UUID", "4", "90006", "3901.200.71.1.16", "UPDATE properties SET value='invalid' WHERE key='UUID'", false, false},
		{"duplicate property", "4", "90006", "3901.200.71.1.16", "INSERT INTO properties(key,value) VALUES('minor_version','90006')", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), envelopeIndexName)
			writer := openTestWriter(t, path)
			createTestSchema(t, writer, "")
			for key, value := range map[string]string{
				"version": test.version, "minor_version": test.minor, "last_write_framework_version": test.framework,
			} {
				if _, err := writer.ExecContext(ctx, "UPDATE properties SET value=? WHERE key=?", value, key); err != nil {
					closeTestResourceNow(t, writer, "profile writer")
					t.Fatal(err)
				}
			}
			if test.mutation != "" {
				if _, err := writer.ExecContext(ctx, test.mutation); err != nil {
					closeTestResourceNow(t, writer, "profile writer")
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			reader, err := openReadOnlyDatabase(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			closeTestResource(t, reader, "profile reader")
			capability, err := validateSchema(ctx, reader)
			if !test.accepted {
				if nestedErrorCode(err) != "unsupported_mail_store_schema" {
					t.Fatalf("accepted unsupported profile: capability=%+v error=%v", capability, err)
				}
				return
			}
			if err != nil || capability.ProfileVerified != test.verified || capability.Fingerprint == "" {
				t.Fatalf("capability=%+v error=%v", capability, err)
			}
			if _, err := reader.ExecContext(ctx, "UPDATE properties SET value='5' WHERE key='version'"); err == nil {
				t.Fatal("accepted profile permits a database write")
			}
		})
	}
}

func TestCompatibleProfileAdapterReadsAndDisclosure(t *testing.T) {
	ctx := context.Background()
	fixture := createSearchFixtureData(t, 0, true)
	writer := openTestWriter(t, filepath.Join(fixture.mailRoot, "V10", "MailData", envelopeIndexName))
	for key, value := range map[string]string{
		"minor_version": "90006", "last_write_framework_version": "3901.200.71.1.16",
	} {
		if _, err := writer.ExecContext(ctx, "UPDATE properties SET value=? WHERE key=?", value, key); err != nil {
			closeTestResourceNow(t, writer, "compatible fixture writer")
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	client := NewClient(ctx, nil, Config{MailRoot: fixture.mailRoot, ActiveAccountURLs: fixture.activeAccountURLs}, mail.SendTransport{})
	closeTestResource(t, client, "compatible profile client")
	if client.store == nil {
		t.Fatalf("compatible profile rejected: %v", client.storeErr)
	}
	profile, known := client.StoreProfile()
	if !known || !profile.Unverified() || profile.FrameworkVersion != "3901.200.71.1.16" {
		t.Fatalf("compatibility presented as verified: %+v", profile)
	}
	page, err := client.ListMessages(ctx, mail.ListMessagesRequest{MailboxRef: fixture.inboxRef, Limit: 20})
	if err != nil || len(page.Messages) != 3 {
		t.Fatalf("listing=%+v error=%v", page, err)
	}
	ref := messageRefWithSubject(t, page.Messages, "Status Update")
	message, err := client.GetMessage(ctx, ref)
	if err != nil || !message.ContentComplete || !strings.Contains(message.Content, "needle beta") {
		t.Fatalf("message=%+v error=%v", message, err)
	}
	raw, err := client.GetRawSource(ctx, ref)
	if err != nil || !strings.Contains(raw, "Message-ID: <102@example.com>") {
		t.Fatalf("raw read: %v; raw=%q", err, raw)
	}
	query, err := mail.PrepareQuery(mail.Query{Text: "needle beta", AccountRef: fixture.accountRef, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	found, err := client.SearchMessages(ctx, query)
	if err != nil || len(found.Messages) != 1 || found.Messages[0].Summary.Ref != ref || !found.Coverage.Complete || !found.Coverage.SourcesComplete {
		t.Fatalf("body search=%+v error=%v", found, err)
	}
	service := mail.NewService(client)
	for _, args := range [][]string{
		{"doctor"}, {"mailboxes", "list", "--limit", "1"}, {"messages", "list", "--limit", "1"},
		{"messages", "filter", "--read", "true"}, {"messages", "search", "--query", "needle beta"},
		{"messages", "get", "--ref", ref, "--view", "plain"}, {"attachments", "list", "--ref", ref},
		{"messages", "raw", "--ref", ref},
	} {
		t.Run(strings.Join(args[:1], " ")+"/"+strings.Join(args[1:], " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			status := cli.Run(ctx, service, append(append([]string(nil), args...), "--json"), &stdout, &stderr)
			var response struct {
				OK   bool `json:"ok"`
				Data struct {
					Profile mail.StoreProfile `json:"store_profile"`
				} `json:"data"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || status != 0 || !response.OK || stderr.Len() != 0 || response.Data.Profile != profile {
				t.Fatalf("status=%d output=%s stderr=%s decode=%v", status, &stdout, &stderr, err)
			}
		})
	}
}

func TestProfileAttachmentExportFidelity(t *testing.T) {
	for _, compatible := range []bool{false, true} {
		for _, external := range []bool{false, true} {
			t.Run(fmt.Sprintf("compatible=%t/external=%t", compatible, external), func(t *testing.T) {
				ctx := context.Background()
				fixture := createSearchFixtureData(t, 0, true)
				if compatible {
					writer := openTestWriter(t, filepath.Join(fixture.mailRoot, "V10", "MailData", envelopeIndexName))
					if _, err := writer.ExecContext(ctx, `UPDATE properties SET value=CASE key
						WHEN 'minor_version' THEN '90006' ELSE '3901.200.71.1.16' END
						WHERE key IN ('minor_version','last_write_framework_version')`); err != nil {
						closeTestResourceNow(t, writer, "attachment profile writer")
						t.Fatal(err)
					}
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
				}
				client := NewClient(ctx, nil, Config{MailRoot: fixture.mailRoot, ActiveAccountURLs: fixture.activeAccountURLs}, mail.SendTransport{})
				closeTestResource(t, client, "attachment profile client")
				if client.store == nil {
					t.Fatalf("profile refused: %v", client.storeErr)
				}
				profile, known := client.StoreProfile()
				if !known || profile.Unverified() != compatible {
					t.Fatalf("profile=%+v known=%t", profile, known)
				}
				page, err := client.ListMessages(ctx, mail.ListMessagesRequest{MailboxRef: fixture.inboxRef, Limit: 10})
				if err != nil {
					t.Fatal(err)
				}
				ref := messageRefWithSubject(t, page.Messages, "Quarterly Report")
				original := []byte("attachment\x00\xff\xfe\r\nbytes")
				encoded := base64.StdEncoding.EncodeToString(original)
				if external {
					encoded = ""
				}
				raw := []byte("From: Alice <alice@example.com>\r\nMessage-ID: <101@example.com>\r\n" +
					"Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
					"--b\r\nContent-Type: text/plain; format=flowed; delsp=yes\r\n\r\ninteroper \r\nability\r\n" +
					"--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=invoice.pdf\r\n" +
					fmt.Sprintf("X-Apple-Content-Length: %d\r\n", len(original)) +
					"Content-Transfer-Encoding: base64\r\n\r\n" + encoded + "\r\n--b--\r\n")
				writeFixtureEMLX(t, client.store, 101, "imap://"+testAccountID+"/Archive", raw)
				if external {
					resolved, err := client.store.resolveMessage(ctx, ref)
					if err != nil {
						t.Fatal(err)
					}
					directory, err := client.store.attachmentDirectory(resolved, "2")
					if err != nil {
						t.Fatal(err)
					}
					if err := os.MkdirAll(directory, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(directory, "invoice.pdf"), original, 0600); err != nil {
						t.Fatal(err)
					}
				}
				service := mail.NewService(client)
				var stdout, stderr bytes.Buffer
				status := cli.Run(ctx, service, []string{"attachments", "list", "--ref", ref, "--json"}, &stdout, &stderr)
				type attachmentResponse struct {
					OK   bool `json:"ok"`
					Data struct {
						Attachments []mail.Attachment    `json:"attachments"`
						Complete    bool                 `json:"content_complete"`
						Profile     mail.StoreProfile    `json:"store_profile"`
						Saved       mail.SavedAttachment `json:"saved_attachment"`
					} `json:"data"`
				}
				var response attachmentResponse
				if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || status != 0 || !response.OK || stderr.Len() != 0 || len(response.Data.Attachments) != 1 {
					t.Fatalf("list status=%d output=%s stderr=%s decode=%v", status, &stdout, &stderr, err)
				}
				attachment := response.Data.Attachments[0]
				if attachment.ID != "2" || attachment.Name != "invoice.pdf" || attachment.MIMEType == nil || *attachment.MIMEType != "application/pdf" ||
					!attachment.Downloaded || !attachment.SizeKnown || attachment.Size != int64(len(original)) || response.Data.Complete == external || response.Data.Profile != profile {
					t.Fatalf("metadata=%+v complete=%t profile=%+v", attachment, response.Data.Complete, response.Data.Profile)
				}
				outputPath := filepath.Join(t.TempDir(), "export.pdf")
				stdout.Reset()
				response = attachmentResponse{}
				status = cli.Run(ctx, service, []string{"attachments", "save", "--ref", ref, "--attachment", attachment.ID, "--output", outputPath, "--json"}, &stdout, &stderr)
				if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || status != 0 || !response.OK || stderr.Len() != 0 {
					t.Fatalf("save status=%d output=%s stderr=%s decode=%v", status, &stdout, &stderr, err)
				}
				saved, err := os.ReadFile(outputPath)
				digest := sha256.Sum256(original)
				if err != nil || !bytes.Equal(saved, original) || response.Data.Saved.Path != outputPath || response.Data.Saved.Size != int64(len(original)) ||
					response.Data.Saved.SHA256 != hex.EncodeToString(digest[:]) || response.Data.Profile != profile {
					t.Fatalf("saved=%+v bytes=%x error=%v", response.Data.Saved, saved, err)
				}
				storedRaw, err := client.GetRawSource(ctx, ref)
				if err != nil || !bytes.Equal([]byte(storedRaw), raw) {
					t.Fatalf("raw MIME changed: error=%v", err)
				}
				message, err := client.GetMessage(ctx, ref)
				if err != nil || message.Content != "interoperability" || message.ContentComplete == external {
					t.Fatalf("normalized body=%q complete=%t error=%v", message.Content, message.ContentComplete, err)
				}
				query, err := mail.PrepareQuery(mail.Query{Text: "interoperability", AccountRef: fixture.accountRef, MailboxRef: fixture.inboxRef, Limit: 10})
				if err != nil {
					t.Fatal(err)
				}
				found, err := client.SearchMessages(ctx, query)
				if err != nil || len(found.Messages) != 1 || found.Messages[0].Summary.Ref != ref || !found.Coverage.SourcesComplete || !found.Coverage.Complete {
					t.Fatalf("flowed search=%+v error=%v", found, err)
				}
			})
		}
	}
}
