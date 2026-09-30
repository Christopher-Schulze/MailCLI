package mailstore

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExcerptCacheRequiresOneBoundedDocument(t *testing.T) {
	t.Parallel()
	const valid = `{"v":1,"excerpt":"hello","complete":true}`
	for _, test := range []struct {
		name, payload string
		want          bool
		expired       bool
	}{
		{name: "valid", payload: valid, want: true},
		{name: "whitespace", payload: valid + "\n\t ", want: true},
		{name: "trailing garbage", payload: valid + "garbage"},
		{name: "second document", payload: valid + `{}`},
		{name: "missing version", payload: `{}`},
		{name: "unsupported version", payload: `{"v":2,"excerpt":"hello","complete":true}`},
		{name: "oversized", payload: valid + strings.Repeat(" ", maximumExcerptCacheEntryBytes)},
		{name: "expired", payload: valid, expired: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache := excerptCache{dir: t.TempDir()}
			path := cache.path("entry")
			if err := os.WriteFile(path, []byte(test.payload), 0o600); err != nil {
				t.Fatal(err)
			}
			if test.expired {
				old := time.Now().Add(-excerptCacheTTL - time.Hour)
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
			}
			entry, ok := cache.load("entry")
			if ok != test.want || (ok && (entry.Excerpt != "hello" || !entry.Complete)) {
				t.Fatalf("load = %+v, %t; want hit=%t", entry, ok, test.want)
			}
		})
	}
}

func TestExcerptCacheRejectsSymlink(t *testing.T) {
	t.Parallel()
	cache := excerptCache{dir: t.TempDir()}
	target := filepath.Join(t.TempDir(), "outside.json")
	const payload = `{"v":1,"excerpt":"outside","complete":true}`
	if err := os.WriteFile(target, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, cache.path("entry")); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.load("entry"); ok {
		t.Fatal("followed a cache-entry symlink")
	}
	retained, err := os.ReadFile(target)
	if err != nil || string(retained) != payload {
		t.Fatalf("symlink target changed: %q, %v", retained, err)
	}
}

func TestExcerptCacheRejectsFIFOWithoutWaiting(t *testing.T) {
	t.Parallel()
	cache := excerptCache{dir: t.TempDir()}
	path := cache.path("entry")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	var hit bool
	assertFIFOReadReturnsPromptly(t, path, func() { _, hit = cache.load("entry") })
	if hit {
		t.Fatal("FIFO became a cache hit")
	}
}

// A broken blocking opener is unblocked and joined before the test ends.
func assertFIFOReadReturnsPromptly(t *testing.T, path string, read func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		read()
	}()
	select {
	case <-done:
		return
	case <-time.After(time.Second):
		t.Error("FIFO open waited for a writer instead of rejecting the file")
	}
	writer, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("FIFO reader did not exit after being unblocked")
	}
}
