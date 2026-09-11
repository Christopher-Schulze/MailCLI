package mail

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// DraftRevisionConflict identifies the content that invalidated a reviewed
// operation. CandidatePath is populated only by an editor retaining local work.
type DraftRevisionConflict struct {
	Ref              string `json:"ref"`
	ExpectedRevision string `json:"expected_revision"`
	CurrentRevision  string `json:"current_revision"`
	CandidatePath    string `json:"candidate_path,omitempty"`
}

func (e *DraftRevisionConflict) Error() string {
	message := fmt.Sprintf("draft %s changed since review; current revision: %s; inspect the current content before retrying", e.Ref, e.CurrentRevision)
	if e.CandidatePath != "" {
		message += "; editor candidate retained at " + e.CandidatePath
	}
	return message
}

func (e *DraftRevisionConflict) ErrorCode() string {
	return "draft_revision_conflict"
}

func requireDraftRevision(ref, expected, current string) error {
	if expected == "" {
		return validationError("expected revision is required; inspect the draft and supply its revision")
	}
	if current == "" {
		return &OperationError{Code: "draft_revision_unavailable", Message: "legacy send evidence has no reviewed revision; use drafts inspect or drafts reconcile"}
	}
	if expected != current {
		return &DraftRevisionConflict{Ref: ref, ExpectedRevision: expected, CurrentRevision: current}
	}
	return nil
}

func validStoredDraftRevision(revision string) bool {
	if revision == "" {
		return true // Legacy claims and receipts remain inspectable/reconcilable.
	}
	digest, ok := strings.CutPrefix(revision, "draft-review-v1:")
	if !ok || len(digest) != sha256.Size*2 || digest != strings.ToLower(digest) {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

// refreshDraftRevision accepts already validated canonical draft content.
// JSON string framing and explicit collection lengths prevent ambiguous field
// boundaries, including embedded NULs and movement between recipient roles.
func refreshDraftRevision(draft *Draft) error {
	parts := []string{
		"draft-review-v1", draft.Ref, string(draft.Kind), draft.AccountRef, draft.From,
		draft.SourceRef, strconv.FormatBool(draft.ReplyAll), draft.SourceMessageID, draft.SourceReferences,
		draft.Subject, string(draft.BodyFormat), draft.BodySource, draft.Body, draft.BodyHTML,
	}
	for _, recipients := range [][]Recipient{draft.To, draft.CC, draft.BCC} {
		parts = append(parts, strconv.Itoa(len(recipients)))
		for _, recipient := range recipients {
			parts = append(parts, recipient.Name, recipient.Address)
		}
	}
	parts = append(parts, strconv.Itoa(len(draft.Attachments)))
	for _, attachment := range draft.Attachments {
		parts = append(parts, attachment.Path, strconv.FormatInt(attachment.Size, 10), attachment.SHA256)
	}
	hash := sha256.New()
	if err := json.NewEncoder(hash).Encode(parts); err != nil {
		return fmt.Errorf("encode draft revision: %w", err)
	}
	draft.Revision = "draft-review-v1:" + hex.EncodeToString(hash.Sum(nil))
	return nil
}
