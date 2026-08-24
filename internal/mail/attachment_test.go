package mail

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type attachmentGateway struct {
	gatewayStub
	content []byte
}

func (g *attachmentGateway) SaveAttachmentTo(_ context.Context, _ string, _ string, path string) error {
	return os.WriteFile(path, g.content, 0o644)
}

func TestSaveAttachmentPublishesExactPrivateFile(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "report.pdf")
	service := NewService(&attachmentGateway{content: []byte("exact attachment")})

	saved, err := service.SaveAttachment(context.Background(), SaveAttachmentRequest{
		MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
	})
	if err != nil {
		t.Fatalf("SaveAttachment() error = %v", err)
	}
	if saved.Size != 16 || saved.SHA256 != "c766525e1fc9ed7b91e950f7a23d94e932c1de4886d933096a51c3a48c66fda0" {
		t.Fatalf("saved = %+v", saved)
	}
	content, err := os.ReadFile(output)
	if err != nil || string(content) != "exact attachment" {
		t.Fatalf("saved content = %q, error = %v", content, err)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved mode = %v, error = %v", info.Mode().Perm(), err)
	}
}

func TestSaveAttachmentNeverOverwrites(t *testing.T) {
	output := filepath.Join(t.TempDir(), "existing.txt")
	if err := os.WriteFile(output, []byte("keep"), 0o600); err != nil {
		t.Fatalf("seed output: %v", err)
	}
	service := NewService(&attachmentGateway{content: []byte("replace")})
	_, err := service.SaveAttachment(context.Background(), SaveAttachmentRequest{
		MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
	})
	if err == nil {
		t.Fatal("SaveAttachment() error = nil")
	}
	content, readErr := os.ReadFile(output)
	if readErr != nil || string(content) != "keep" {
		t.Fatalf("existing content = %q, error = %v", content, readErr)
	}
}
