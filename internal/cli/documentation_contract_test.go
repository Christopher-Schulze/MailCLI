package cli

import (
	"bytes"
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
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
	assertCapabilitySemantics(t, mustCapabilities(t))
	assertDocumentationClaims(t, artifacts)
	assertHelpClaims(t, help)
	assertSharedDocumentationBounds(t)
	assertQualifiedMailAppClaim(t, artifacts)
}

func TestCatalogCursorDocumentationMatchesRuntimeContract(t *testing.T) {
	documentation := strings.Join(strings.Fields(readRepositoryFile(t, "docs/documentation.md")), " ")
	for _, claim := range []string{
		"Message-list cursors remain store/mailbox-bound",
		"filter and search cursors remain store/query-bound",
		"draft cursors remain directory-revision-bound",
		"thread cursors retain their conversation binding under `data.thread`",
		"account, mailbox, and attachment catalog cursors are versioned and bind the command, scope, and ordered stable identities",
	} {
		if !strings.Contains(documentation, claim) {
			t.Errorf("docs/documentation.md omits cursor contract %q", claim)
		}
	}
}

func TestHistoricalSaveRecoveryDocumentationMatchesRemovedCommand(t *testing.T) {
	manifest := mustCapabilities(t)
	if len(manifest.Commands) != 39 || manifest.DraftSavePolicy.SafeRecoveryCommand != "mailcli drafts reconcile --ref <DRAFT_REF> --json" {
		t.Fatalf("published inventory=%d policy=%+v", len(manifest.Commands), manifest.DraftSavePolicy)
	}
	for _, command := range manifest.Commands {
		if command.ID == "drafts.save" {
			t.Fatal("unsupported save command remains published")
		}
	}
	for _, path := range []string{"README.md", "docs/documentation.md", "skills/mailcli/SKILL.md", "skills/mailcli/references/native-handoff.md", "skills/mailcli/references/sending.md"} {
		content := readRepositoryFile(t, path)
		if !strings.Contains(content, "drafts reconcile") && !strings.Contains(content, "drafts.reconcile") {
			t.Errorf("%s omits supported historical recovery", path)
		}
		for _, obsolete := range []string{"mailcli drafts save --ref", "reconcile-only `drafts save`", "`drafts.save`", "minutes for `drafts save`"} {
			if strings.Contains(content, obsolete) {
				t.Errorf("%s still instructs an unsupported operation: %q", path, obsolete)
			}
		}
	}
}

func TestCapabilityDependencyDocumentationMatchesRuntimeContract(t *testing.T) {
	versions := []struct {
		path    string
		version string
	}{
		{path: "README.md", version: "nested `data.capabilities.schema_version` is 2"},
		{path: "docs/documentation.md", version: "`data.capabilities.schema_version:2`"},
		{path: "skills/mailcli/SKILL.md", version: "capabilities schema 2"},
	}
	for _, test := range versions {
		content := readRepositoryFile(t, test.path)
		for _, required := range []string{test.version, "dependencies"} {
			if !strings.Contains(content, required) {
				t.Errorf("%s omits capability contract detail %q", test.path, required)
			}
		}
		for _, removed := range []string{"mail_app_dependency", "credential_dependencies", "network_dependencies"} {
			if strings.Contains(content, removed) {
				t.Errorf("%s retains removed capability field %q", test.path, removed)
			}
		}
	}
	documentation := readRepositoryFile(t, "docs/documentation.md")
	for _, condition := range []string{
		"if-local-source-incomplete", "if-enrichment-source-incomplete", "if-local-attachment-bytes-unavailable",
		"if-local-store-unavailable", "if-batch-item-requires-imap", "if-sync-check",
		"if-sync-default", "if-doctor-live", "if-send", "if-smtp-accepted",
		"if-transport-claim-needs-imap-reconciliation",
	} {
		if !strings.Contains(documentation, condition) {
			t.Errorf("docs/documentation.md omits dependency condition %q", condition)
		}
	}
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
	for _, claim := range []string{`"view":"plain"`, "Reads default metadata", "--max-bytes", "replay_allowed:false", "item IDs and states"} {
		if !strings.Contains(guide, claim) {
			t.Errorf("batch output guide omits %q", claim)
		}
	}
	for _, path := range []string{"docs/documentation.md", "skills/mailcli/references/output-and-recovery.md"} {
		content := readRepositoryFile(t, path)
		for _, claim := range []string{"data.required_bytes", "data.limit_bytes", "data.measured", "lower_bound"} {
			if !strings.Contains(content, claim) {
				t.Errorf("%s omits structured output-size claim %q", path, claim)
			}
		}
	}
	if !strings.Contains(guide, `"id":"meta","ref":"REF"`) {
		t.Error("skill batch-read guide omits the implicit metadata-default example")
	}
}

func TestCIRunnerDocumentationMatchesWorkflow(t *testing.T) {
	workflow := strings.ReplaceAll(readRepositoryFile(t, ".github/workflows/ci.yml"), "\r\n", "\n")
	paragraph := ciDocumentationParagraph(t, readRepositoryFile(t, "docs/documentation.md"))
	_, triggers, found := strings.Cut(workflow, "\non:\n")
	events, _, ended := strings.Cut(triggers, "\npermissions:\n")
	if !found || !ended || strings.TrimSpace(events) != "workflow_dispatch:" {
		t.Error("online CI must start only by manual dispatch")
	}
	for _, claim := range []string{"manual `workflow_dispatch`", "no automatic push or pull-request runs", "final integrated local full proof"} {
		if !strings.Contains(paragraph, claim) {
			t.Errorf("CI documentation omits local/manual policy %q", claim)
		}
	}
	runners := ciWorkflowRunners(t, workflow)
	if len(runners) == 0 {
		t.Fatal("CI workflow declares no runner label")
	}
	for _, runner := range runners {
		if !strings.Contains(paragraph, "`"+runner+"`") {
			t.Errorf("CI documentation paragraph omits runner %q", runner)
		}
	}
	commands := ciRunCommandPattern.FindAllStringSubmatch(workflow, -1)
	if len(commands) == 0 {
		t.Fatal("CI workflow runs no Go command")
	}
	for _, command := range commands {
		if !strings.Contains(paragraph, "`"+command[1]+"`") {
			t.Errorf("CI documentation paragraph omits command %q", command[1])
		}
	}
	if strings.Contains(paragraph, "ARM64") && !strings.Contains(workflow, `test "$(uname -m)" = arm64`) {
		t.Error("CI documentation claims ARM64 lanes but the workflow does not assert arm64")
	}
	if len(runners) != 1 || runners[0] != "macos-26" {
		t.Errorf("CI must declare one supported ARM64 runner, got %v", runners)
	}
	if !strings.Contains(workflow, "run: scripts/tests/test.sh --full") || !strings.Contains(paragraph, "`scripts/tests/test.sh --full`") {
		t.Error("CI and its documentation must use the supported full verification entry point")
	}
	pin := regexp.MustCompile(`(?m)^go ([0-9]+\.[0-9]+\.[0-9]+)$`).FindStringSubmatch(readRepositoryFile(t, "go.mod"))
	if len(pin) != 2 || !strings.Contains(workflow, "GOTOOLCHAIN: go"+pin[1]) {
		t.Error("CI toolchain must match the exact go.mod pin")
	}
}

func TestPublicDocumentationOmitsOwnerLocalTaskWorkflow(t *testing.T) {
	// Only user-facing docs are checked. Scripts and tests may need local task paths
	// as inputs when exercising task-history and CI tooling.
	paths := []string{"README.md", "docs/documentation.md", "skills/mailcli/SKILL.md"}
	references, err := filepath.Glob(filepath.Join(repositoryRoot(t), "skills/mailcli/references/*.md"))
	if err != nil {
		t.Fatalf("list skill references: %v", err)
	}
	for _, reference := range references {
		relative, err := filepath.Rel(repositoryRoot(t), reference)
		if err != nil {
			t.Fatalf("relative skill reference path: %v", err)
		}
		paths = append(paths, relative)
	}
	ownerLocalMarkers := []string{
		strings.Join([]string{"docs", "tasks.md"}, "/"),
		strings.Join([]string{"docs", "tasks", ""}, "/"),
		strings.Join([]string{"docs", "tasks", "done", ""}, "/"),
		"AGENTS" + ".local.md",
		strings.Join([]string{"manage", "write", "lease"}, "-") + ".sh",
		"private-proof",
		strings.Join([]string{"report", "task", "ci"}, "-") + ".sh",
		strings.Join([]string{"export", "task", "history"}, "-") + ".sh",
		"ci_record",
		"ci_run_window",
	}
	for _, path := range paths {
		content := readRepositoryFile(t, path)
		for _, marker := range ownerLocalMarkers {
			if strings.Contains(content, marker) {
				t.Errorf("%s exposes owner-local task workflow marker %q", path, marker)
			}
		}
	}
}

func TestPublicGoCommentsOmitPrivateTaskPaths(t *testing.T) {
	count := 0
	for _, path := range publicGoSourcePaths(t, repositoryRoot(t)) {
		if path == "" {
			continue
		}
		count++
		if err := validatePublicGoComments(path, readRepositoryFile(t, path)); err != nil {
			t.Error(err)
		}
	}
	if count == 0 {
		t.Fatal("tracked Go comment inventory is empty")
	}
	t.Logf("checked comments in %d tracked Go sources, including tests", count)
}

// publicGoSourcePaths lists tracked Go files in a Git checkout and falls back
// to a module walk for plain source trees (for example `git archive` output).
func publicGoSourcePaths(t *testing.T, root string) []string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, "git", "ls-files", "-z", "--", "*.go")
		command.Dir = root
		output, err := command.Output()
		if err != nil {
			t.Fatalf("inventory tracked Go sources: %v", err)
		}
		return strings.Split(string(output), "\x00")
	}
	skipped := map[string]bool{".git": true, "bin": true, "dist": true, "graphify-out": true, filepath.Join("docs", "tasks"): true}
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() && skipped[relative] {
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.HasSuffix(relative, ".go") {
			paths = append(paths, filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("inventory Go sources: %v", err)
	}
	return paths
}

func validatePublicGoComments(filename, source string) error {
	file, err := parser.ParseFile(token.NewFileSet(), filename, source, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("parse public Go source %s: %w", filename, err)
	}
	privatePath := strings.Join([]string{"docs", "tasks", ""}, "/")
	for _, comments := range file.Comments {
		for _, comment := range comments.List {
			if strings.Contains(comment.Text, privatePath) {
				return fmt.Errorf("%s contains a private task-history link in a public comment", filename)
			}
		}
	}
	return nil
}

func TestPublicGoCommentValidationDistinguishesCommentsFromInputs(t *testing.T) {
	privatePath := strings.Join([]string{"docs", "tasks", "done", "fixture.md"}, "/")
	for _, test := range []struct {
		name   string
		source string
		valid  bool
	}{
		{name: "inline evidence", source: "package fixture\n// Reads hydrate via IMAP.\nvar value = 1", valid: true},
		{name: "required tooling input", source: "package fixture\nvar path = \"" + privatePath + "\"", valid: true},
		{name: "line comment", source: "package fixture\n// See " + privatePath},
		{name: "block comment", source: "package fixture\n/* See " + privatePath + " */"},
		{name: "Go directive", source: "package fixture\n//go:generate cat " + privatePath},
		{name: "test-file comment", source: "package fixture\n// Evidence: " + privatePath + "\nvar value = 1"},
		{name: "invalid source", source: "not Go source"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validatePublicGoComments("fixture_test.go", test.source); (err == nil) != test.valid {
				t.Fatalf("comment validation = %v, want valid=%t", err, test.valid)
			}
		})
	}
}

var (
	ciRunnerPattern       = regexp.MustCompile(`(?m)^[ \t]*runs-on:[ \t]*["']?([A-Za-z0-9._-]+)["']?[ \t]*(?:#.*)?$`)
	ciMatrixRunnerPattern = regexp.MustCompile(`(?m)^[ \t]*runs-on:[ \t]*\$\{\{[ \t]*matrix\.([A-Za-z0-9_-]+)[ \t]*\}\}`)
	ciRunCommandPattern   = regexp.MustCompile(`(?m)^[ \t]*(?:-[ \t]+)?run:[ \t]*(go [^#\n]*?)[ \t]*(?:#.*)?$`)
)

func ciWorkflowRunners(t *testing.T, workflow string) []string {
	t.Helper()
	var runners []string
	for _, match := range ciRunnerPattern.FindAllStringSubmatch(workflow, -1) {
		runners = append(runners, match[1])
	}
	for _, match := range ciMatrixRunnerPattern.FindAllStringSubmatch(workflow, -1) {
		values := regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(match[1]) + `:[ \t]*\[([^\]]*)\]`).FindStringSubmatch(workflow)
		if values == nil {
			t.Fatalf("CI matrix key %q has no inline label list", match[1])
		}
		for _, value := range strings.Split(values[1], ",") {
			runners = append(runners, strings.Trim(strings.TrimSpace(value), `"'`))
		}
	}
	return runners
}

func ciDocumentationParagraph(t *testing.T, documentation string) string {
	t.Helper()
	for _, paragraph := range strings.Split(documentation, "\n\n") {
		if strings.Contains(paragraph, "`.github/workflows/ci.yml`") {
			return strings.Join(strings.Fields(paragraph), " ")
		}
	}
	t.Fatal("product documentation has no paragraph describing .github/workflows/ci.yml")
	return ""
}

func assertCapabilitySemantics(t *testing.T, manifest capabilityManifest) {
	t.Helper()
	send := findCapability(t, manifest, "drafts.send")
	if send.EffectClass != "smtp-send" || send.StoreDependency != "draft-store" ||
		!slices.Equal(send.Dependencies, []commandDependency{
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfSend},
			{Kind: dependencyKindNetwork, Target: dependencyTargetSMTP, Condition: dependencyConditionIfSend},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfSMTPAccepted},
		}) || !containsAll(send.ResultStates, "sent", "sent_mirror_pending") {
		t.Fatalf("drafts.send capability = %+v", send)
	}
	reconcile := findCapability(t, manifest, "drafts.reconcile")
	if reconcile.EffectClass != "local-write+imap-write" ||
		reconcile.StoreDependency != "draft-store+mail-store-if-baseline" ||
		!slices.Equal(reconcile.Dependencies, []commandDependency{
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfTransportClaimNeedsIMAPReconciliation},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfTransportClaimNeedsIMAPReconciliation},
		}) ||
		!containsAll(reconcile.ResultStates, "sent", "sent_mirror_pending", "outcome_unknown") {
		t.Fatalf("drafts.reconcile capability = %+v", reconcile)
	}
}

func assertDocumentationClaims(t *testing.T, artifacts map[string]string) {
	t.Helper()
	for name, content := range artifacts {
		content = strings.Join(strings.Fields(content), " ")
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
		// The README is a landing page; detailed recovery contracts live in the
		// manual and the skill, which both stay checked here.
		if name == "README.md" {
			continue
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
		"Details: mailcli <command> --help",
		"Contracts: mailcli capabilities --for <id> --schemas --outputs --json",
	} {
		if !strings.Contains(help, claim) {
			t.Errorf("top-level help omits %q contract: %s", claim, help)
		}
	}
	manual := strings.Join(strings.Fields(readRepositoryFile(t, "docs/documentation.md")), " ")
	for _, claim := range []string{
		"bypasses Mail.app entirely",
		"Mutations (`messages mark`, `messages move`, `messages copy`, `messages delete`) execute over IMAP directly",
		"Scripted native composition remains disabled before baseline capture",
	} {
		if !strings.Contains(manual, claim) {
			t.Errorf("linked manual omits execution-boundary contract %q", claim)
		}
	}
}

func readOperationalDocumentation(t *testing.T) map[string]string {
	t.Helper()
	paths := []string{"README.md", "docs/documentation.md", "skills/mailcli/SKILL.md"}
	artifacts := make(map[string]string, len(paths))
	for _, path := range paths {
		artifacts[path] = readRepositoryFile(t, path)
		if path == "skills/mailcli/SKILL.md" {
			if err := validateSkillReferences(filepath.Join(repositoryRoot(t), "skills/mailcli")); err != nil {
				t.Fatal(err)
			}
			for _, guide := range []string{"reading", "sending"} {
				artifacts[path] += "\n" + readRepositoryFile(t, "skills/mailcli/references/"+guide+".md")
			}
		}
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
	{name: "IMAP LIST cumulative response byte cap", expected: imapclient.MaxListOperationResponseBytes, unit: "bytes",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `LIST uses a (\d+) MiB cumulative wire-response limit`, 1<<20),
		}},
	{name: "IMAP LIST logical response line limit", expected: int64(imapclient.MaxListOperationResponseLines), unit: "lines",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `(\d[\d,]*) untagged logical response lines`, 1),
		}},
	{name: "IMAP LIST mailbox limit", expected: int64(imapclient.MaxListOperationMailboxes), unit: "mailboxes",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `(\d[\d,]*) mailboxes per LIST operation`, 1),
		}},
	{name: "message page limit", expected: mail.MaximumPageLimit, unit: "messages",
		checks: []documentedBoundCheck{
			boundCheck("README.md", "`--limit` values from 1 through ([\\d,]+)", 1),
			boundCheck("docs/documentation.md", "`--limit` values from 1 through ([\\d,]+)", 1),
			boundCheck("docs/documentation.md", `limits list pages to (\d+) items`, 1),
			boundCheck("docs/documentation.md", "`--limit` accepts 1 through (\\d+)", 1),
			boundCheck("skills/mailcli/references/reading.md", `limit 1\.\.(\d+)`, 1),
		}},
	{name: "default list page size", expected: mail.DefaultPageLimit, unit: "items",
		checks: []documentedBoundCheck{
			boundCheck("README.md", "list commands default to ([\\d,]+) items", 1),
			boundCheck("docs/documentation.md", `List commands default to (\d+) items`, 1),
			boundCheck("skills/mailcli/references/reading.md", `default (\d+), limit`, 1),
			boundCheck("skills/mailcli/references/drafts.md", `default (\d+)/limit`, 1),
		}},
	{name: "draft list page limit", expected: mail.MaximumDraftListLimit, unit: "drafts",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `summaries and accepts 1 through (\d+)`, 1),
			boundCheck("skills/mailcli/references/drafts.md", `limit 1\.\.(\d+)`, 1),
		}},
	{name: "draft list default page size", expected: mail.DefaultDraftListLimit, unit: "drafts",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `defaults to (\d+) summaries`, 1),
			boundCheck("skills/mailcli/references/drafts.md", `default (\d+)/limit`, 1),
		}},
	{name: "search default candidate bound", expected: mail.DefaultSearchMaxMessages, unit: "messages",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", "`--max-messages` defaults to ([\\d,]+)", 1),
		}},
	{name: "search maximum candidate bound", expected: mail.MaximumSearchMaxMessages, unit: "messages",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", "`--max-messages`[^.\n]*capped at ([\\d,]+)", 1),
		}},
	{name: "search default byte budget", expected: mail.DefaultSearchMaxBytes, unit: "bytes",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", "`--max-scan-bytes` defaults to ([0-9]+) GiB", 1<<30),
		}},
	{name: "search maximum byte budget", expected: mail.MaximumSearchMaxBytes, unit: "bytes",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", "`--max-scan-bytes`[^.\n]*capped at ([0-9]+) GiB", 1<<30),
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
			boundCheck("docs/documentation.md", `(\d+) MiB message cap`, 1<<20),
			boundCheck("docs/documentation.md", `through the (\d+) MiB maximum`, 1<<20),
			boundCheck("docs/documentation.md", `raw fallback to (\d+) MiB`, 1<<20),
			boundCheck("docs/documentation.md", `same (\d+) MiB cap`, 1<<20),
		}},
	{name: "external attachment directory entry limit", expected: 10_000, unit: "entries",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `discovery is bounded per directory to ([\d,]+) entries`, 1),
			boundCheck("skills/mailcli/references/reading.md", `Directory bounds: ([\d,]+) entries`, 1),
		}},
	{name: "external attachment ambiguity candidate limit", expected: 128, unit: "candidates",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `entries, ([\d,]+) hashed ambiguity candidates`, 1),
			boundCheck("skills/mailcli/references/reading.md", `entries/([\d,]+) ambiguity hashes`, 1),
		}},
	{name: "external attachment cumulative hash input", expected: 1 << 30, unit: "bytes",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `candidates, and ([\d]+) GiB cumulative hash input`, 1<<30),
			boundCheck("skills/mailcli/references/reading.md", `hashes/([\d]+) GiB hash input`, 1<<30),
		}},
	{name: "recovery spool cap", expected: 1 << 30, unit: "bytes",
		checks: []documentedBoundCheck{
			boundCheck("README.md", `recovery spool is bounded to (\d+) GiB`, 1<<30),
			boundCheck("docs/documentation.md", `recovery spool is bounded to (\d+) GiB`, 1<<30),
		}},
	{name: "structured input cap", expected: mail.MaximumBatchInputBytes, unit: "bytes",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `capped at (\d+) MiB`, 1<<20),
			boundCheck("docs/documentation.md", `at most (\d+) MiB`, 1<<20),
			boundCheck("skills/mailcli/references/output-and-recovery.md", `object <=(\d+) MiB`, 1<<20),
			boundCheck("skills/mailcli/references/drafts.md", `one object <=(\d+) MiB`, 1<<20),
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
			boundCheck("docs/documentation.md", `default is (\w+) authenticated`, 1),
		}},
	{name: "IMAP pool maximum", expected: imapclient.MaximumConnectionsPerAccount, unit: "connections",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `one through (\w+)`, 1),
		}},
	{name: "IMAP mutation lock wait", expected: 30, unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `waits up to (\d+) seconds`, 1),
			boundCheck("skills/mailcli/references/mutations.md", `account lock: (\d+) s`, 1),
		}},
	{name: "installer lock wait", expected: 30, unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("scripts/release/install.sh", `lockf -s -t (\d+)`, 1),
			boundCheck("docs/documentation.md", `wait at most (\d+) seconds`, 1),
			boundCheck("skills/mailcli/references/setup.md", `Direct wait <=(\d+) s`, 1),
		}},
	{name: "preflight doctor cache", expected: 300, unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("scripts/utils/mailcli-preflight.sh", `DOCTOR_TTL_SECONDS=(\d+)`, 1),
			boundCheck("docs/documentation.md", `(\w+)-minute freshness`, 60),
			boundCheck("skills/mailcli/references/setup.md", `cache <=(\d+) s`, 1),
		}},
	{name: "transport command budget", expected: int64(transport.TransferCommandBudget / time.Second), unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `final replies use a (\d+)-second`, 1),
			boundCheck("docs/documentation.md", `(\d+) seconds plus one second`, 1),
		}},
	{name: "transport transfer cap", expected: int64(transport.TransferBudgetCap / time.Second), unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `capped at (\d+) minutes`, 60),
			boundCheck("skills/mailcli/references/sending.md", `deadline <=(\d+) min`, 60),
		}},
	{name: "draft lock wait", expected: 2, unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `(\w+)-second BSD`, 1),
		}},
	{name: "CLI read budget", expected: int64(readTimeout / time.Second), unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `(\d+)-second read budget`, 1),
		}},
	{name: "hydration command window", expected: int64(hydrationReadBudget() / time.Second), unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `hydration command window is (\d+) seconds`, 1),
		}},
	{name: "maximum raw hydration FETCH budget", expected: int64(transport.TransferBudgetForSize(mail.MaximumRawSourceBytes) / time.Second), unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `maximum computes to a (\d+)-second FETCH budget`, 1),
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
	{name: "draft prune operation budget", expected: int64(draftPruneTimeout / time.Second), unit: "seconds",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", "([\\w]+) minutes for `drafts prune`", 60),
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
			boundCheck("skills/mailcli/references/drafts.md", `each (\d+) MiB`, 1<<20),
		}},
	{name: "draft content node limit", expected: 65536, unit: "nodes",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `at most ([\d,]+) nodes`, 1),
			boundCheck("skills/mailcli/references/drafts.md", `([\d,]+) nodes`, 1),
		}},
	{name: "draft content depth limit", expected: 512, unit: "levels",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `nodes and (\d+) levels`, 1),
			boundCheck("skills/mailcli/references/drafts.md", `nodes/(\d+) levels`, 1),
		}},
	{name: "draft link label budget", expected: 4 * mail.MaximumDraftBodyBytes, unit: "bytes",
		checks: []documentedBoundCheck{
			boundCheck("docs/documentation.md", `limited to (\d+) MiB across`, 1<<20),
			boundCheck("skills/mailcli/references/drafts.md", `link labels (\d+) MiB`, 1<<20),
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
				if strings.HasSuffix(check.path, ".md") {
					content = strings.Join(strings.Fields(content), " ")
				}
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
