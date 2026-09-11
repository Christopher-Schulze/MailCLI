package mail

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"syscall"
)

const (
	DefaultDraftListLimit = 50
	MaximumDraftListLimit = 200
)

type ListDraftsRequest struct {
	Limit  int
	Cursor string
}

type DraftPagination struct {
	Limit      int    `json:"limit"`
	Revision   string `json:"revision"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type DraftPage struct {
	Drafts     []DraftSummary  `json:"drafts"`
	Pagination DraftPagination `json:"page"`
}

// ListDrafts returns one reference-ordered page. Continuation is bound to the
// directory identity and modification time, covering atomic draft/claim
// publication and membership changes. Each page scans names in bounded chunks
// and decodes only its selected records; it does not retain an immutable copy.
func (s *Service) ListDrafts(ctx context.Context, request ListDraftsRequest) (result DraftPage, resultErr error) {
	if err := draftContextError(ctx, "list"); err != nil {
		return DraftPage{}, err
	}
	if request.Limit == 0 {
		request.Limit = DefaultDraftListLimit
	}
	if request.Limit < 1 || request.Limit > MaximumDraftListLimit {
		return DraftPage{}, validationError("draft list limit must be between 1 and 200")
	}
	revision, after, err := decodeDraftListCursor(request.Cursor)
	if err != nil {
		return DraftPage{}, err
	}
	root, err := s.resolveDraftRoot()
	if err != nil {
		return DraftPage{}, err
	}
	pinned, err := os.OpenRoot(root)
	if err != nil {
		return DraftPage{}, fmt.Errorf("open draft directory: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, pinned.Close()) }()
	directory, err := pinned.Open(".")
	if err != nil {
		return DraftPage{}, fmt.Errorf("open draft listing: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	state := &draftStorage{rootName: root, root: pinned, directory: directory}
	result, err = readDraftPage(ctx, state, request.Limit, revision, after)
	return result, classifyDraftContextError(ctx, err, "list")
}

func readDraftPage(ctx context.Context, state *draftStorage, limit int, revision, after string) (DraftPage, error) {
	identity, err := state.directory.Stat()
	if err != nil {
		return DraftPage{}, fmt.Errorf("inspect draft directory: %w", err)
	}
	currentRevision, err := draftListRevision(state.rootName, identity)
	if err != nil {
		return DraftPage{}, err
	}
	if revision != "" && revision != currentRevision {
		return DraftPage{}, draftListChangedError()
	}
	if err := verifyDraftListDirectory(state, identity); err != nil {
		return DraftPage{}, err
	}
	refs, err := selectDraftListRefs(ctx, state.directory, after, limit+1)
	if err != nil {
		return DraftPage{}, err
	}
	page := DraftPage{Drafts: make([]DraftSummary, 0, min(limit, len(refs))), Pagination: DraftPagination{Limit: limit, Revision: currentRevision}}
	if len(refs) > limit {
		refs = refs[:limit]
		page.Pagination.NextCursor = base64.RawURLEncoding.EncodeToString([]byte("dl1\n" + currentRevision + "\n" + refs[len(refs)-1]))
	}
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return DraftPage{}, err
		}
		draft, err := readDraftSummary(ctx, ref, state)
		if err != nil {
			draft = DraftSummary{Ref: ref, StateError: err.Error()}
		}
		page.Drafts = append(page.Drafts, draft)
	}
	if err := ctx.Err(); err != nil {
		return DraftPage{}, err
	}
	if err := verifyDraftListDirectory(state, identity); err != nil {
		return DraftPage{}, err
	}
	return page, nil
}

func selectDraftListRefs(ctx context.Context, directory *os.File, after string, limit int) ([]string, error) {
	refs := make([]string, 0, limit)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, err := directory.ReadDir(256)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("list drafts: %w", err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasPrefix(name, "draft_") || !strings.HasSuffix(name, ".json") {
				continue
			}
			ref := strings.TrimSuffix(name, ".json")
			if ref <= after {
				continue
			}
			index := sort.SearchStrings(refs, ref)
			if index == limit {
				continue
			}
			if len(refs) < limit {
				refs = append(refs, "")
			}
			copy(refs[index+1:], refs[index:len(refs)-1])
			refs[index] = ref
		}
		if errors.Is(err, io.EOF) {
			return refs, nil
		}
	}
}

func draftListRevision(root string, identity os.FileInfo) (string, error) {
	stat, ok := identity.Sys().(*syscall.Stat_t)
	if !ok || !identity.IsDir() {
		return "", errors.New("draft directory identity is unavailable")
	}
	digest := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d:%d:%d", root, stat.Dev, stat.Ino, identity.ModTime().UnixNano()))
	return fmt.Sprintf("%x", digest), nil
}

func verifyDraftListDirectory(state *draftStorage, expected os.FileInfo) error {
	for _, path := range []string{"", state.rootName} {
		var current os.FileInfo
		var err error
		if path == "" {
			current, err = state.directory.Stat()
		} else {
			current, err = os.Stat(path)
		}
		if err != nil || !os.SameFile(expected, current) || !expected.ModTime().Equal(current.ModTime()) {
			return draftListChangedError()
		}
	}
	return nil
}

func decodeDraftListCursor(cursor string) (string, string, error) {
	if cursor == "" {
		return "", "", nil
	}
	invalid := &OperationError{Code: "invalid_cursor", Message: "invalid draft list cursor; restart drafts list without --cursor"}
	if len(cursor) > 512 {
		return "", "", invalid
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if err != nil || !strings.HasPrefix(string(payload), "dl1\n") {
		return "", "", invalid
	}
	revision, after, ok := strings.Cut(string(payload[4:]), "\n")
	if !ok || len(revision) != 64 || strings.Trim(revision, "0123456789abcdef") != "" ||
		!strings.HasPrefix(after, "draft_") || len(after) > 250 || strings.ContainsAny(after, "/\\\x00") {
		return "", "", invalid
	}
	return revision, after, nil
}

func draftListChangedError() error {
	return &OperationError{Code: "invalid_cursor", Message: "draft directory changed during pagination; restart drafts list without --cursor"}
}
