package mailstore

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMessageBucket(t *testing.T) {
	t.Parallel()
	tests := []struct {
		rowID int64
		want  []string
	}{
		{rowID: 1},
		{rowID: 999},
		{rowID: 1000, want: []string{"1"}},
		{rowID: 12345, want: []string{"2", "1"}},
		{rowID: 73598, want: []string{"3", "7"}},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("row-%d", test.rowID), func(t *testing.T) {
			t.Parallel()
			if got := messageBucket(test.rowID); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("messageBucket(%d) = %#v, want %#v", test.rowID, got, test.want)
			}
		})
	}
}

func TestValidateEMLXFrameExposesOnlyDeclaredSource(t *testing.T) {
	t.Parallel()
	source := []byte("From: sender@example.com\nSubject: Test\n\nBody\n")
	path := writeTestEMLX(t, source, validPlistTrailer())
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	closeTestResource(t, file, "EMLX fixture")
	info, err := file.Stat()
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	length, err := validateEMLXFrame(file, info.Size())
	if err != nil {
		t.Fatalf("validateEMLXFrame() error = %v", err)
	}
	got, err := io.ReadAll(io.NewSectionReader(file, emlxPrefixBytes, length))
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if !reflect.DeepEqual(got, source) {
		t.Fatalf("RFC source = %q, want %q", got, source)
	}
}

func TestValidateEMLXFrameRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content []byte
	}{
		{name: "short", content: []byte("1\n")},
		{name: "invalid length", content: append([]byte("abc       \n"), validPlistTrailer()...)},
		{name: "short source", content: append([]byte("999       \nshort"), validPlistTrailer()...)},
		{name: "invalid trailer", content: append([]byte("1         \nx"), []byte("not plist")...)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "test.emlx")
			if err := os.WriteFile(path, test.content, 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			closeTestResource(t, file, "EMLX fixture")
			info, err := file.Stat()
			if err != nil {
				t.Fatalf("Stat() error = %v", err)
			}
			if _, err := validateEMLXFrame(file, info.Size()); err == nil {
				t.Fatal("validateEMLXFrame() error = nil")
			}
		})
	}
}

func TestValidatePathWithoutSymlinksRejectsSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	if err := validatePathWithoutSymlinks(root, link); err == nil {
		t.Fatal("validatePathWithoutSymlinks() error = nil")
	}
}

func writeTestEMLX(t *testing.T, source []byte, trailer []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.emlx")
	framed := append([]byte(fmt.Sprintf("%-10d\n", len(source))), source...)
	framed = append(framed, trailer...)
	if err := os.WriteFile(path, framed, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func validPlistTrailer() []byte {
	return []byte(`<?xml version="1.0"?><plist version="1.0"><dict/></plist>`)
}
