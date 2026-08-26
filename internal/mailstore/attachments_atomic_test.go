package mailstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExternalAttachmentOperationsRejectSelectedFileReplacement(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Store, externalAttachment, string) error
	}{
		{
			name: "hash",
			run: func(store *Store, selected externalAttachment, _ string) error {
				_, err := store.hashStoreFile(selected)
				return err
			},
		},
		{
			name: "copy",
			run: func(store *Store, selected externalAttachment, output string) error {
				return store.copyExternalAttachment(selected, output)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			rootDirectory, err := openVersionDirectory(root)
			if err != nil {
				t.Fatalf("openVersionDirectory() error = %v", err)
			}
			closeTestResource(t, rootDirectory, "test root")
			store := &Store{versionRoot: root, versionDirectory: rootDirectory}
			path := filepath.Join(root, "attachment.bin")
			if err := os.WriteFile(path, []byte("reviewed bytes"), 0o600); err != nil {
				t.Fatalf("write selected attachment: %v", err)
			}
			identity, err := os.Lstat(path)
			if err != nil {
				t.Fatalf("inspect selected attachment: %v", err)
			}
			selected := externalAttachment{Path: path, Size: identity.Size(), identity: identity}
			if err := os.Rename(path, filepath.Join(root, "original.bin")); err != nil {
				t.Fatalf("move selected attachment: %v", err)
			}
			if err := os.WriteFile(path, []byte("replaced bytes"), 0o600); err != nil {
				t.Fatalf("write replacement attachment: %v", err)
			}
			output := filepath.Join(root, "output.bin")
			if err := test.run(store, selected, output); errorCodeForTest(err) != "store_changed" {
				t.Fatalf("operation error = %v, want store_changed", err)
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("output exists after rejected replacement: %v", err)
			}
		})
	}
}
