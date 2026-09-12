package mail

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
)

func draftSelectionFixture(t testing.TB, count int) (string, []string) {
	t.Helper()
	root := t.TempDir()
	refs := make([]string, count)
	for index := count - 1; index >= 0; index-- {
		refs[index] = fmt.Sprintf("draft_%024d", index)
		if err := os.WriteFile(filepath.Join(root, refs[index]+".json"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, refs
}

func readSelection(t testing.TB, root, after string, limit int) []string {
	t.Helper()
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	refs, selectionErr := selectDraftListRefs(context.Background(), directory, after, limit)
	if err := errors.Join(selectionErr, directory.Close()); err != nil {
		t.Fatal(err)
	}
	return refs
}

func TestDraftSelectionWindowsMatchSortedReferences(t *testing.T) {
	for _, count := range []int{0, 32, 10000} {
		root, refs := draftSelectionFixture(t, count)
		for _, name := range []string{"other.json", "draft_wrong.txt", "draft_other.json.bak"} {
			if err := os.WriteFile(filepath.Join(root, name), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Mkdir(filepath.Join(root, "draft_directory.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, after := range []string{"", fmt.Sprintf("draft_%024d", count/2), fmt.Sprintf("draft_%024d", max(0, count-2))} {
			for _, limit := range []int{1, 51, 201} {
				t.Run(fmt.Sprintf("%d/%s/%d", count, after, limit), func(t *testing.T) {
					start := sort.Search(len(refs), func(i int) bool { return refs[i] > after })
					want := refs[start:min(start+limit, len(refs))]
					if got := readSelection(t, root, after, limit); !slices.Equal(got, want) {
						t.Fatalf("selection differs: got %v, want %v", got, want)
					}
				})
			}
		}
	}
}

func TestDraftSelectionCancellationStopsBeforeDirectoryExhaustion(t *testing.T) {
	root, _ := draftSelectionFixture(t, 10000)
	for _, trigger := range []int{1, 2} {
		t.Run(fmt.Sprintf("context-check-%d", trigger), func(t *testing.T) {
			directory, err := os.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := directory.Close(); err != nil {
					t.Error(err)
				}
			}()
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &draftListBoundaryContext{Context: base, trigger: trigger, action: cancel}
			refs, err := selectDraftListRefs(ctx, directory, "", 51)
			if !errors.Is(err, context.Canceled) || len(refs) != 0 {
				t.Fatalf("canceled selection returned a page: %v %v", refs, err)
			}
			remaining, err := directory.ReadDir(1)
			if err != nil || len(remaining) != 1 {
				t.Fatalf("cancellation was only checked after exhausting the directory: %v", err)
			}
		})
	}
}

func BenchmarkDraftReferenceSelection(b *testing.B) {
	for _, count := range []int{0, 32, 10000} {
		root, refs := draftSelectionFixture(b, count)
		for _, window := range []struct {
			name  string
			after string
		}{
			{"first", ""},
			{"middle", fmt.Sprintf("draft_%024d", count/2)},
			{"last", fmt.Sprintf("draft_%024d", max(0, count-2))},
		} {
			for _, limit := range []int{1, 51, 201} {
				b.Run(fmt.Sprintf("records-%d/%s/limit-%d", count, window.name, limit), func(b *testing.B) {
					start := sort.Search(len(refs), func(i int) bool { return refs[i] > window.after })
					want := refs[start:min(start+limit, len(refs))]
					if got := readSelection(b, root, window.after, limit); !slices.Equal(got, want) {
						b.Fatal("incorrect selected refs before timing")
					}
					b.ReportAllocs()
					b.ResetTimer()
					for range b.N {
						if got := readSelection(b, root, window.after, limit); !slices.Equal(got, want) {
							b.Fatal("selected refs changed during timing")
						}
					}
				})
			}
		}
	}
}
