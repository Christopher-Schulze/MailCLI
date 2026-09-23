package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSkillDocumentationSelfContained(t *testing.T) {
	roots := []string{filepath.Join("..", "..", "skills", "mailcli")}
	if installed := os.Getenv("MAILCLI_TEST_SKILL_DIRECTORY"); installed != "" {
		roots = append(roots, installed)
	}
	for _, root := range roots {
		t.Run(root, func(t *testing.T) {
			if err := validateSkillReferences(root); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSkillRouterMatchesCapabilityInventory(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "skills", "mailcli", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(content), "## Choose the command\n")
	end := strings.Index(string(content), "\n## Choose the execution boundary")
	if start < 0 || end <= start {
		t.Fatal("skill command router is missing")
	}
	idPattern := regexp.MustCompile("`([a-z]+(?:[.-][a-z]+)*)`")
	routed := make(map[string]int)
	for _, line := range strings.Split(string(content)[start:end], "\n") {
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "| Intent ") ||
			strings.HasPrefix(line, "| --- ") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) != 5 {
			t.Fatalf("malformed skill router row: %q", line)
		}
		for _, match := range idPattern.FindAllStringSubmatch(cells[2], -1) {
			routed[match[1]]++
		}
	}
	for _, command := range capabilities().Commands {
		if command.ID == "capabilities" { // This is the router's preflight, not a routed action.
			continue
		}
		if routed[command.ID] != 1 {
			t.Errorf("skill router contains command %q %d times; want once", command.ID, routed[command.ID])
		}
		delete(routed, command.ID)
	}
	for id := range routed {
		t.Errorf("skill router names unpublished command %q", id)
	}
}

// Skill guides use inline relative Markdown file links without fragments.
// Reject unsupported link forms instead of silently passing unverified targets.
func validateSkillReferences(root string) error {
	link := regexp.MustCompile(`\[[^\]\n]+\]\(([^)\n]+)\)`)
	reachable := map[string]bool{"SKILL.md": true}
	queue := []string{"SKILL.md"}
	for len(queue) > 0 {
		path := queue[0]
		queue = queue[1:]
		content, err := readSkillDocument(root, path)
		if err != nil {
			return err
		}
		for _, match := range link.FindAllStringSubmatch(string(content), -1) {
			target := match[1]
			if filepath.IsAbs(target) || strings.ContainsAny(target, "#:<> ") || filepath.Ext(target) != ".md" {
				return fmt.Errorf("%s: unsupported skill link %q; use a relative Markdown file", path, target)
			}
			target = filepath.Clean(filepath.Join(filepath.Dir(path), target))
			if !filepath.IsLocal(target) {
				return fmt.Errorf("%s: skill link escapes the package: %q", path, match[1])
			}
			if !reachable[target] {
				reachable[target] = true
				queue = append(queue, target)
			}
		}
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err == nil && filepath.Ext(path) == ".md" && !reachable[relative] {
			return fmt.Errorf("unreachable skill guide: %s", relative)
		}
		return err
	})
}

func readSkillDocument(root, path string) ([]byte, error) {
	current := root
	for _, component := range strings.Split(path, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, fmt.Errorf("skill document %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("skill document uses a symlink: %s", path)
		}
	}
	return os.ReadFile(current)
}

func TestSkillReferencesRejectBrokenPackages(t *testing.T) {
	for _, test := range []struct {
		name   string
		link   string
		orphan bool
		valid  bool
	}{
		{name: "complete", link: "references/read.md", valid: true},
		{name: "missing", link: "references/absent.md"},
		{name: "escaping", link: "../outside.md"},
		{name: "absolute", link: "/tmp/outside.md"},
		{name: "unsupported anchor", link: "references/read.md#missing"},
		{name: "orphan", link: "references/read.md", orphan: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "references"), 0700); err != nil {
				t.Fatal(err)
			}
			files := map[string]string{"SKILL.md": "[Read](" + test.link + ")", "references/read.md": "# Read"}
			if test.orphan {
				files["references/orphan.md"] = "# Orphan"
			}
			for path, content := range files {
				if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := validateSkillReferences(root); (err == nil) != test.valid {
				t.Fatalf("validateSkillReferences = %v, want valid=%t", err, test.valid)
			}
		})
	}
}
