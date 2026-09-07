package mailstore

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestValidateXMLPlistRequiresSingleRoot(t *testing.T) {
	t.Parallel()
	valid := string(validPlistTrailer())
	tests := []struct {
		name    string
		source  string
		wantErr bool
	}{
		{name: "minimal plist", source: `<plist><dict/></plist>`},
		{name: "standard plist", source: valid},
		{name: "namespaced root and attributes", source: `<p:plist xmlns:p="urn:mail" version="1.0" custom="accepted"><p:dict/></p:plist>`},
		{name: "doctype before root", source: `<?xml version="1.0"?><!DOCTYPE plist><plist version="1.0"><dict/></plist>`},
		{name: "wrong root", source: `<wrapper><plist><dict/></plist></wrapper>`, wantErr: true},
		{name: "wrong child", source: `<plist version="1.0"><array/></plist>`, wantErr: true},
		{name: "unsupported version", source: `<plist version="2.0"><dict/></plist>`, wantErr: true},
		{name: "nested plist", source: `<plist><dict><key>nested</key><plist><dict/></plist></dict></plist>`, wantErr: true},
		{name: "duplicate root", source: valid + valid, wantErr: true},
		{name: "trailing text", source: valid + "trailing", wantErr: true},
		{name: "trailing element", source: valid + `<extra/>`, wantErr: true},
		{name: "malformed XML", source: `<plist><dict></plist>`, wantErr: true},
		{name: "invalid encoding", source: `<?xml version="1.0" encoding="ISO-8859-1"?><plist><dict/></plist>`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateXMLPlist(strings.NewReader(test.source))
			if test.wantErr {
				if err == nil {
					t.Fatal("validateXMLPlist() error = nil, want error")
				}
				if errorCodeForTest(err) != "invalid_emlx" {
					t.Fatalf("validateXMLPlist() error = %v, want invalid_emlx", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateXMLPlist() error = %v", err)
			}
		})
	}
}

func TestValidateXMLPlistSizeLimit(t *testing.T) {
	t.Parallel()
	source := string(validPlistTrailer()) + strings.Repeat(" ", int(maximumPlistBytes))
	err := validateXMLPlist(strings.NewReader(source))
	if errorCodeForTest(err) != "invalid_emlx" {
		t.Fatalf("validateXMLPlist() error = %v, want invalid_emlx", err)
	}
}

func FuzzValidateXMLPlist(f *testing.F) {
	f.Add(string(validPlistTrailer()))
	f.Add(`<wrapper><plist><dict/></plist></wrapper>`)
	f.Add(`<plist><dict>`)
	f.Fuzz(func(t *testing.T, source string) {
		if err := validateXMLPlist(strings.NewReader(source)); err != nil && errorCodeForTest(err) != "invalid_emlx" {
			t.Fatalf("validateXMLPlist() error = %v, want invalid_emlx", err)
		}
	})
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
