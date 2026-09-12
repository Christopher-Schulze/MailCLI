package mail

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func TestDraftOperationBudgetScalesWithBytes(t *testing.T) {
	if got := draftOperationBudget(0); got != transport.TransferCommandBudget {
		t.Fatalf("draftOperationBudget(0) = %v, want %v", got, transport.TransferCommandBudget)
	}
	mib := int64(1 << 20)
	if got := draftOperationBudget(mib); got != transport.TransferCommandBudget+time.Second {
		t.Fatalf("draftOperationBudget(1MiB) = %v, want %v", got, transport.TransferCommandBudget+time.Second)
	}
	if got := draftOperationBudget(1 << 62); got != transport.TransferBudgetCap {
		t.Fatalf("draftOperationBudget(huge) = %v, want cap %v", got, transport.TransferBudgetCap)
	}
	if draftOperationBudget(512<<20) <= draftOperationBudget(0) {
		t.Fatal("draftOperationBudget does not scale with attachment bytes")
	}
}

func TestDraftAttachmentPathBytesSumsRegularFiles(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.bin")
	second := filepath.Join(dir, "b.bin")
	if err := os.WriteFile(first, make([]byte, 100), 0o600); err != nil {
		t.Fatalf("write first: %v", err)
	}
	if err := os.WriteFile(second, make([]byte, 50), 0o600); err != nil {
		t.Fatalf("write second: %v", err)
	}
	paths := []string{first, second, filepath.Join(dir, "missing.bin"), dir}
	if got := draftAttachmentPathBytes(paths); got != 150 {
		t.Fatalf("draftAttachmentPathBytes() = %d, want 150", got)
	}
	if got := draftAttachmentPathBytes(nil); got != 0 {
		t.Fatalf("draftAttachmentPathBytes(nil) = %d, want 0", got)
	}
}
