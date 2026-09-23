package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
	"mailcli/internal/transport/imapclient"
)

func TestOperationalDocumentationMatchesRuntimeContracts(t *testing.T) {
	artifacts := readOperationalDocumentation(t)
	help := renderTopLevelHelp(t)
	assertCapabilitySemantics(t, capabilities())
	assertDocumentationClaims(t, artifacts)
	assertHelpClaims(t, help)
	assertSharedDocumentationBounds(t)
	assertQualifiedMailAppClaim(t, artifacts)
}

func TestBatchOutputDocumentationMatchesRuntimeContract(t *testing.T) {
	paths := []string{
		"README.md", "docs/documentation.md", "skills/mailcli/SKILL.md",
		"skills/mailcli/references/output-and-recovery.md",
	}
	for _, path := range paths {
		content := strings.ToLower(readRepositoryFile(t, path))
		for _, stale := range []string{
			"batch read currently returns full messages",
			"batch read currently emits full messages",
			"batch has no output projection or byte gate",
		} {
			if strings.Contains(content, stale) {
				t.Errorf("%s contains stale batch output contract %q", path, stale)
			}
		}
	}
	guide := readRepositoryFile(t, "skills/mailcli/references/output-and-recovery.md")
	for _, claim := range []string{"view:\"metadata\"", "--max-bytes", "replay_allowed:false", "item IDs and states"} {
		if !strings.Contains(guide, claim) {
			t.Errorf("batch output guide omits %q", claim)
		}
	}
	skill := readRepositoryFile(t, "skills/mailcli/SKILL.md")
	if !strings.Contains(skill, `"view":"metadata"`) {
		t.Error("skill batch-read guidance omits the metadata-default example")
	}
}

func assertCapabilitySemantics(t *testing.T, manifest capabilityManifest) {
	t.Helper()
	send := findCapability(t, manifest, "drafts.send")
	if send.EffectClass != "smtp-send" || send.StoreDependency != "draft-store" ||
		send.MailAppDependency != "none" || !containsAll(send.ResultStates, "sent", "sent_mirror_pending") {
		t.Fatalf("drafts.send capability = %+v", send)
	}
	reconcile := findCapability(t, manifest, "drafts.reconcile")
	if reconcile.EffectClass != "local-write+imap-write" ||
		reconcile.StoreDependency != "draft-store+mail-store-if-baseline" ||
		reconcile.MailAppDependency != "none" ||
		!containsAll(reconcile.ResultStates, "sent", "sent_mirror_pending", "outcome_unknown") {
		t.Fatalf("drafts.reconcile capability = %+v", reconcile)
	}
}

func assertDocumentationClaims(t *testing.T, artifacts map[string]string) {
	t.Helper()
	for name, content := range artifacts {
		lower := strings.ToLower(content)
		for _, stale := range []string{
			"keep the last uid",
			"keeping the last uid",
			"keep the last-uid behavior",
			"keeping the last-uid behavior",
			"last verified uid",
		} {
			if strings.Contains(lower, stale) {
				t.Errorf("%s contains stale Message-ID selection claim %q", name, stale)
			}
		}
		for _, claim := range []struct {
			name string
			text string
		}{
			{name: "ambiguous identity", text: "imap_ambiguous_message_id"},
			{name: "partial content state", text: "content_complete"},
			{name: "partial content evidence", text: "missing_parts"},
			{name: "direct recovery", text: "drafts reconcile"},
			{name: "accepted SMTP evidence", text: "submission_accepted"},
			{name: "Sent-copy evidence", text: "sent_copy_observed"},
		} {
			if !strings.Contains(content, claim.text) {
				t.Errorf("%s omits %s contract (%q)", name, claim.name, claim.text)
			}
		}
	}
}

func assertHelpClaims(t *testing.T, help string) {
	t.Helper()
	for _, claim := range []string{
		"Mail.app",
		"Direct SMTP send and IMAP mutations work without Mail.app",
		"Mail 16 scripted draft save remains disabled",
	} {
		if !strings.Contains(help, claim) {
			t.Errorf("top-level help omits %q contract: %s", claim, help)
		}
	}
}

func readOperationalDocumentation(t *testing.T) map[string]string {
	t.Helper()
	paths := []string{"README.md", "docs/documentation.md", "skills/mailcli/SKILL.md"}
	artifacts := make(map[string]string, len(paths))
	for _, path := range paths {
		artifacts[path] = readRepositoryFile(t, path)
	}
	return artifacts
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate the contract test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
}

func readRepositoryFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(repositoryRoot(t), path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

func renderTopLevelHelp(t *testing.T) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), newTestService(), []string{"help"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("top-level help code = %d, stderr = %q", code, stderr.String())
	}
	return stdout.String()
}

func findCapability(t *testing.T, manifest capabilityManifest, id string) commandCapability {
	t.Helper()
	for _, command := range manifest.Commands {
		if command.ID == id {
			return command
		}
	}
	t.Fatalf("capability %q is missing", id)
	return commandCapability{}
}

func containsAll(values []string, wanted ...string) bool {
	for _, value := range wanted {
		found := false
		for _, candidate := range values {
			if candidate == value {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// documentedBound pins one operational bound to the same normalized value on
// every surface that states it. Each check carries one capture group holding
// the numeric token (digits with optional thousand separators or an English
// word number) and a unit scale; every match in the file must equal expected.
// A drifted value, a removed statement, or a rephrased number all fail the
// gate. Bounds backed only by unexported constants carry a literal expected
// value; bounds implemented in scripts are extracted from the script itself.
type documentedBoundCheck struct {
	path    string
	pattern *regexp.Regexp
	scale   int64
}

type documentedBound struct {
	name     string
	expected int64
	unit     string
	checks   []documentedBoundCheck
}

func boundCheck(path, pattern string, scale int64) documentedBoundCheck {
	return documentedBoundCheck{path: path, pattern: regexp.MustCompile(pattern), scale: scale}
}

var documentedWordNumbers = map[string]int64{
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
}

var documentedBounds = []documentedBound{
	{name: "message page limit", expected: mail.MaximumPageLimit, unit: "messages",
		checks: []documentedBoundCheck{
			boundCheck("README.md", `page sizes from 1 through (\d+)`, 1),
			boundCheck("docs/documentation.md", `page sizes from 1 through (\d+)`, 1),
			boundCheck("docs/documentation.md", `limits pages to (\d+) messages`, 1),
			boundCheck("docs/documentation.md", `accepts 1 through (\d+) like list pages`, 1),
			boundCheck("skills/mailcli/references/reading.md", `bounded to (\d+)`, 1),
		}},
	{name: "draft list page limit", expected: mail.MaximumDraftListLimit, unit: "drafts",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `summaries and accepts 1 through (\d+)`, 1),
			boundCheck("skills/mailcli/references/drafts.md", `accepts 1 through (\d+)`, 1),
		}},
	{name: "draft list default page size", expected: mail.DefaultDraftListLimit, unit: "drafts",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `defaults to (\d+) summaries`, 1),
			boundCheck("skills/mailcli/references/drafts.md", `defaults to (\d+) entries`, 1),
		}},
	{name: "search default candidate bound", expected: mail.DefaultSearchMaxMessages, unit: "messages",
		checks: []documentedBoundCheck{
			boundCheck("README.md", "`--max-messages` defaults to ([\\d,]+)", 1),
			boundCheck("docs/documentation.md", "`--max-messages` defaults to ([\\d,]+)", 1),
		}},
	{name: "search maximum candidate bound", expected: mail.MaximumSearchMaxMessages, unit: "messages",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", "`--max-messages`[^.\n]*capped at ([\\d,]+)", 1),
		}},
	{name: "search default byte budget", expected: mail.DefaultSearchMaxBytes, unit: "bytes",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", "`--max-bytes` defaults to ([0-9]+) GiB", 1<<30),
		}},
	{name: "search maximum byte budget", expected: mail.MaximumSearchMaxBytes, unit: "bytes",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", "`--max-bytes`[^.\n]*capped at ([0-9]+) GiB", 1<<30),
		}},
	{name: "search scan window cap", expected: 64, unit: "candidates",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `exceeds (\d+) candidates`, 1),
		}},
	{name: "envelope default byte budget", expected: defaultJSONOutputBytes, unit: "bytes",
		checks: []documentedBoundCheck{
			boundCheck("README.md", "`--max-bytes` defaults to ([0-9]+) MiB", 1<<20),
			boundCheck("docs/documentation.md", "`--max-bytes` defaults to ([0-9]+) MiB", 1<<20),
		}},
	{name: "raw source cap", expected: mail.MaximumRawSourceBytes, unit: "bytes",
		checks: []documentedBoundCheck{
			boundCheck("README.md", `second (\d+) MiB string`, 1<<20),
			boundCheck("docs/documentation.md", `(\d+) MiB message cap`, 1<<20),
			boundCheck("docs/documentation.md", `through the (\d+) MiB maximum`, 1<<20),
			boundCheck("docs/documentation.md", `raw fallback to (\d+) MiB`, 1<<20),
			boundCheck("docs/documentation.md", `same (\d+) MiB cap`, 1<<20),
		}},
	{name: "recovery spool cap", expected: 1 << 30, unit: "bytes",
		checks: []documentedBoundCheck{
			boundCheck("README.md", `recovery spool is bounded to (\d+) GiB`, 1<<30),
		}},
	{name: "structured input cap", expected: mail.MaximumBatchInputBytes, unit: "bytes",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `capped at (\d+) MiB`, 1<<20),
			boundCheck("docs/documentation.md", `at most (\d+) MiB`, 1<<20),
			boundCheck("skills/mailcli/references/output-and-recovery.md", `up to (\d+) MiB`, 1<<20),
			boundCheck("skills/mailcli/references/drafts.md", `one object up to (\d+) MiB`, 1<<20),
		}},
	{name: "batch item limit", expected: mail.MaximumBatchItems, unit: "items",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `(\d+) items, and`, 1),
		}},
	{name: "batch maximum concurrency", expected: mail.MaximumBatchConcurrency, unit: "workers",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `(\w+) workers \(default`, 1),
		}},
	{name: "batch default concurrency", expected: mail.DefaultBatchConcurrency, unit: "workers",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `\(default (\w+)\)`, 1),
		}},
	{name: "IMAP pool default", expected: imapclient.DefaultMaxConnectionsPerAccount, unit: "connections",
		checks: []documentedBoundCheck{
			boundCheck("README.md", `default is (\w+) authenticated`, 1),
			boundCheck("docs/documentation.md", `default is (\w+) authenticated`, 1),
		}},
	{name: "IMAP pool maximum", expected: imapclient.MaximumConnectionsPerAccount, unit: "connections",
		checks: []documentedBoundCheck{
			boundCheck("README.md", `one through (\w+)`, 1),
			boundCheck("docs/documentation.md", `one through (\w+)`, 1),
		}},
	{name: "IMAP mutation lock wait", expected: 30, unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `waits up to (\d+) seconds`, 1),
			boundCheck("skills/mailcli/references/mutations.md", `bounded (\d+)-second lock`, 1),
		}},
	{name: "installer lock wait", expected: 30, unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("scripts/release/install.sh", `lockf -s -t (\d+)`, 1),
			boundCheck("docs/documentation.md", `wait at most (\d+) seconds`, 1),
			boundCheck("skills/mailcli/references/setup.md", `wait at most (\d+) seconds`, 1),
		}},
	{name: "preflight doctor cache", expected: 300, unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("scripts/utils/mailcli-preflight.sh", `DOCTOR_TTL_SECONDS=(\d+)`, 1),
			boundCheck("README.md", `for (\w+) minutes`, 60),
			boundCheck("docs/documentation.md", `(\w+)-minute freshness`, 60),
			boundCheck("skills/mailcli/SKILL.md", `at most (\d+) seconds`, 1),
		}},
	{name: "transport command budget", expected: int64(transport.TransferCommandBudget / time.Second), unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("README.md", `protocol phases use (\d+) seconds`, 1),
			boundCheck("docs/documentation.md", `final replies use a (\d+)-second`, 1),
			boundCheck("docs/documentation.md", `— (\d+) seconds plus one second`, 1),
		}},
	{name: "transport transfer cap", expected: int64(transport.TransferBudgetCap / time.Second), unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("README.md", `capped at (\d+) minutes`, 60),
			boundCheck("docs/documentation.md", `capped at (\d+) minutes`, 60),
			boundCheck("skills/mailcli/references/sending.md", `capped at (\d+) minutes`, 60),
		}},
	{name: "draft lock wait", expected: 2, unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `(\w+)-second BSD`, 1),
		}},
	{name: "CLI read budget", expected: int64(readTimeout / time.Second), unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `(\d+)-second read budget`, 1),
		}},
	{name: "Mail access gate wait", expected: 2, unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `capped at (\w+) seconds`, 1),
		}},
	{name: "bridge cleanup grace", expected: 15, unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `(\d+)-second cleanup grace`, 1),
		}},
	{name: "mailbox catalog cache", expected: 300, unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `bounded to (\w+) minutes`, 60),
			boundCheck("docs/documentation.md", `expire after (\w+) minutes`, 60),
		}},
	{name: "plutil child bound", expected: 15, unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `child process to (\d+) seconds`, 1),
		}},
	{name: "handoff dispatch deadline", expected: 10, unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `fixed (\d+)-second deadline`, 1),
		}},
	{name: "draft send operation budget", expected: 900, unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", "([\\d]+) minutes for `drafts send`", 60),
		}},
	{name: "draft save operation budget", expected: 120, unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", "([\\w]+) minutes for `drafts save`", 60),
		}},
	{name: "draft edit operation budget", expected: 15, unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", "([\\d]+) seconds for `drafts edit`", 1),
		}},
	{name: "sender identity default scan", expected: mail.DefaultSenderIdentityScanLimit, unit: "messages",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `defaulting to ([\d,]+) messages`, 1),
			boundCheck("docs/documentation.md", `default of ([\d,]+)`, 1),
			boundCheck("docs/documentation.md", `default limit is ([\d,]+)`, 1),
		}},
	{name: "sender identity maximum scan", expected: mail.MaximumSenderIdentityScanLimit, unit: "messages",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `maximum of ([\d,]+)`, 1),
			boundCheck("docs/documentation.md", `hard ([\d,]+) maximum`, 1),
		}},
	{name: "draft body cap", expected: mail.MaximumDraftBodyBytes, unit: "bytes",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `each bounded at (\d+) MiB`, 1<<20),
			boundCheck("docs/documentation.md", `bodies to (\d+) MiB`, 1<<20),
			boundCheck("skills/mailcli/references/drafts.md", `to (\d+) MiB each`, 1<<20),
		}},
	{name: "draft content node limit", expected: 65536, unit: "nodes",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `at most ([\d,]+) nodes`, 1),
			boundCheck("skills/mailcli/references/drafts.md", `([\d,]+) nodes`, 1),
		}},
	{name: "draft content depth limit", expected: 512, unit: "levels",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `nodes and (\d+) levels`, 1),
			boundCheck("skills/mailcli/references/drafts.md", `nodes and (\d+) levels`, 1),
		}},
	{name: "draft link label budget", expected: 4 * mail.MaximumDraftBodyBytes, unit: "bytes",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `limited to (\d+) MiB across`, 1<<20),
			boundCheck("skills/mailcli/references/drafts.md", `link-label text to (\d+) MiB`, 1<<20),
		}},
	{name: "received HTML source limit", expected: 16 << 20, unit: "bytes",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `(\d+) MiB source limit`, 1<<20),
		}},
	{name: "received HTML token limit", expected: 262144, unit: "tokens",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `([\d,]+)-token`, 1),
		}},
	{name: "received HTML node limit", expected: 262144, unit: "nodes",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `limit of ([\d,]+) nodes`, 1),
		}},
	{name: "draft subject cap", expected: mail.MaximumDraftSubjectBytes, unit: "bytes",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `subjects to (\d+) KiB`, 1<<10),
		}},
	{name: "draft recipient limit", expected: mail.MaximumDraftRecipients, unit: "recipients",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `recipients to (\d+)`, 1),
		}},
	{name: "draft attachment limit", expected: mail.MaximumDraftAttachments, unit: "attachments",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `attachments to (\d+)`, 1),
		}},
	{name: "draft attachment byte cap", expected: mail.MaximumDraftAttachmentBytes, unit: "bytes",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `attachment bytes to (\d+) MiB`, 1<<20),
		}},
	{name: "prune stale threshold", expected: 30, unit: "days",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `older than (\d+) days`, 1),
			boundCheck("docs/documentation.md", `(\d+)-day retention`, 1),
		}},
}

func assertSharedDocumentationBounds(t *testing.T) {
	t.Helper()
	contents := make(map[string]string)
	for _, bound := range documentedBounds {
		for _, check := range bound.checks {
			content, ok := contents[check.path]
			if !ok {
				content = readRepositoryFile(t, check.path)
				contents[check.path] = content
			}
			matches := check.pattern.FindAllStringSubmatch(content, -1)
			if len(matches) == 0 {
				t.Errorf("%s omits the %s bound (%q matches nothing)", check.path, bound.name, check.pattern)
				continue
			}
			for _, match := range matches {
				value, parsed := parseDocumentedNumber(match[1])
				if !parsed {
					t.Fatalf("%s captures non-numeric %s value %q", check.path, bound.name, match[1])
				}
				if got := value * check.scale; got != bound.expected {
					t.Errorf("%s documents %s as %d %s, runtime contract is %d %s",
						check.path, bound.name, got, bound.unit, bound.expected, bound.unit)
				}
			}
		}
	}
}

func parseDocumentedNumber(token string) (int64, bool) {
	cleaned := strings.ReplaceAll(token, ",", "")
	if value, err := strconv.ParseInt(cleaned, 10, 64); err == nil {
		return value, true
	}
	value, ok := documentedWordNumbers[strings.ToLower(cleaned)]
	return value, ok
}

// The Mail.app work claim must stay qualified: direct reads and mutations add
// no work to the Mail process, but the bounded Apple Events integrations
// (targeted fallback reads, sync, live doctor, visible handoff) still address
// it. An absolute "zero work" claim would hide those callers.
func assertQualifiedMailAppClaim(t *testing.T, artifacts map[string]string) {
	t.Helper()
	for name, content := range artifacts {
		if strings.Contains(content, "zero work to the Mail.app process") {
			t.Errorf("%s repeats the unqualified Mail.app 'zero work' claim", name)
		}
	}
	readme := artifacts["README.md"]
	for _, qualified := range []string{
		"add no work to the Mail.app process",
		"bounded integrations",
	} {
		if !strings.Contains(readme, qualified) {
			t.Errorf("README.md lost the qualified Mail.app claim %q", qualified)
		}
	}
}
