package cli

// commandDataFields documents which command owns each JSON field of
// responseData. The flat struct stays the single envelope seam: custom
// marshaling shadows message/draft/attachments with projected views, and the
// envelope layer injects cross-cutting fields, so per-command payload types
// would duplicate both mechanisms without changing observable output.
//
// Two pseudo-owners describe fields no single command emits:
//
//   - envelopeLayerFields: injected by the shared envelope path for every
//     command when the condition applies (store_profile whenever an opened
//     store profile exists, finalization on finalization failure).
//   - projectionLayerFields: emitted by the projection layer for any command
//     that registers output flags (projection view/fields metadata).
//
// Ownership covers success and failure envelopes alike; a field listed for a
// command may appear on that command's error responses when partial evidence
// is retained.
const (
	envelopeLayerOwner   = "envelope-layer"
	projectionLayerOwner = "projection-layer"
)

var commandDataFields = map[string][]string{
	envelopeLayerOwner:         {"finalization", "store_profile"},
	projectionLayerOwner:       {"projection"},
	"capabilities":             {"capabilities"},
	"version":                  {"name", "version"},
	"update":                   {"update_result"},
	"doctor":                   {"checks", "timings"},
	"batch":                    {"batch_result"},
	"accounts.list":            {"accounts", "complete", "identity_coverage_complete"},
	"mailboxes.list":           {"mailboxes"},
	"mailboxes.resolve":        {"mailbox"},
	"messages.list":            {"page"},
	"messages.filter":          {"page"},
	"messages.search":          {"page"},
	"messages.get":             {"content_export", "message"},
	"messages.raw":             {"content_export", "raw_source"},
	"messages.state":           {"state"},
	"messages.thread":          {"thread"},
	"attachments.list":         {"attachments", "content_complete", "content_source", "missing_parts"},
	"attachments.save":         {"saved_attachment"},
	"drafts.create":            {"draft"},
	"drafts.list":              {"drafts", "page"},
	"drafts.inspect":           {"content_export", "draft"},
	"drafts.preview":           {"draft_preview"},
	"drafts.edit":              {"draft"},
	"drafts.handoff":           {"draft_handoff"},
	"drafts.update":            {"draft"},
	"drafts.save":              {"saved_draft"},
	"drafts.open":              {"message"},
	"drafts.adopt":             {"draft"},
	"drafts.send":              {"send_receipt", "send_result"},
	"send.setup":               {"send_setup", "partial_effects"},
	"drafts.reconcile":         {"send_receipt", "send_result"},
	"drafts.discard":           {},
	"drafts.prune":             {"prune"},
	"messages.reply":           {"draft"},
	"messages.forward":         {"draft"},
	"messages.mark":            {"message_state"},
	"messages.move":            {"message_state"},
	"messages.copy":            {"message_state"},
	"messages.delete":          {"delete_result"},
	"sync":                     {"sync_check", "sync_result"},
	"drafts.handoff-reconcile": {"handoff_reconcile"},
}
