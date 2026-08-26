package mailstore

import (
	"io"
	"testing"
)

func closeTestResource(t testing.TB, resource io.Closer, description string) {
	t.Helper()
	t.Cleanup(func() {
		if err := resource.Close(); err != nil {
			t.Errorf("close %s: %v", description, err)
		}
	})
}

func closeTestResourceNow(t testing.TB, resource io.Closer, description string) {
	t.Helper()
	if err := resource.Close(); err != nil {
		t.Fatalf("close %s: %v", description, err)
	}
}
