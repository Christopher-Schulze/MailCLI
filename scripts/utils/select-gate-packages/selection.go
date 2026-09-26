package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

type packageError struct {
	Err string
}

type listedModule struct {
	Path      string
	Version   string
	GoVersion string
	Dir       string
	Main      bool
	Replace   *listedModule
}

type listedPackage struct {
	Dir          string
	ImportPath   string
	Name         string
	GoFiles      []string
	ForTest      string
	Imports      []string
	TestImports  []string
	XTestImports []string
	Module       *listedModule
	Incomplete   bool
	Error        *packageError
	DepsErrors   []*packageError
}

type checkPlan struct {
	Packages      []string
	ShellChecks   []string
	Documentation bool
	LintAll       bool
	VerifyModules bool
	ChangedPaths  []string
}

func selectChecks(root string, baseline, staged []listedPackage, rawPaths []byte) (checkPlan, error) {
	plan := checkPlan{Packages: []string{}, ShellChecks: []string{}, ChangedPaths: []string{}}
	seeds := make(map[string]bool)
	checks := make(map[string]bool)
	for _, path := range strings.Split(string(rawPaths), "\x00") {
		if path == "" {
			continue
		}
		if filepath.IsAbs(path) || filepath.Clean(path) != path || path == ".." || strings.HasPrefix(path, "../") {
			return plan, fmt.Errorf("changed path escapes the product: %q", path)
		}
		plan.ChangedPaths = append(plan.ChangedPaths, path)
		seedOwner(root, path, baseline, seeds)
		seedOwner(root, path, staged, seeds)
		routePath(path, &plan, checks)
	}
	if plan.VerifyModules {
		seedModuleChanges(baseline, staged, seeds)
	}
	plan.Packages = reverseClosure(baseline, staged, seeds)
	for check := range checks {
		plan.ShellChecks = append(plan.ShellChecks, check)
	}
	sort.Strings(plan.ShellChecks)
	sort.Strings(plan.ChangedPaths)
	return plan, nil
}

func ordinaryPackage(pkg listedPackage) bool {
	if pkg.ForTest != "" {
		return false
	}
	if pkg.Name == "main" && strings.HasSuffix(pkg.ImportPath, ".test") {
		for _, file := range pkg.GoFiles {
			if filepath.IsAbs(file) {
				return false
			}
		}
	}
	return true
}

func localPackage(pkg listedPackage) bool {
	return ordinaryPackage(pkg) && pkg.Module != nil && pkg.Module.Main
}

func seedOwner(root, path string, packages []listedPackage, seeds map[string]bool) {
	owner := ""
	longest := -1
	for _, pkg := range packages {
		if !localPackage(pkg) {
			continue
		}
		directory, err := filepath.Rel(root, pkg.Dir)
		if err != nil {
			continue
		}
		directory = filepath.ToSlash(directory)
		if directory == "." || strings.HasPrefix(path, directory+"/") {
			if len(directory) > longest {
				owner, longest = pkg.ImportPath, len(directory)
			}
		}
	}
	if owner != "" {
		seeds[owner] = true
	}
}

func reverseEdges(baseline, staged []listedPackage) map[string][]string {
	reverse := make(map[string][]string)
	for _, packages := range [][]listedPackage{baseline, staged} {
		for _, pkg := range packages {
			if !ordinaryPackage(pkg) {
				continue
			}
			for _, imports := range [][]string{pkg.Imports, pkg.TestImports, pkg.XTestImports} {
				for _, dependency := range imports {
					reverse[dependency] = append(reverse[dependency], pkg.ImportPath)
				}
			}
		}
	}
	return reverse
}

func reverseClosure(baseline, staged []listedPackage, seeds map[string]bool) []string {
	reverse := reverseEdges(baseline, staged)
	queue := make([]string, 0, len(seeds))
	for seed := range seeds {
		queue = append(queue, seed)
	}
	for index := 0; index < len(queue); index++ {
		for _, dependent := range reverse[queue[index]] {
			if !seeds[dependent] {
				seeds[dependent] = true
				queue = append(queue, dependent)
			}
		}
	}
	result := []string{}
	for _, pkg := range staged {
		if localPackage(pkg) && seeds[pkg.ImportPath] {
			result = append(result, pkg.ImportPath)
		}
	}
	sort.Strings(result)
	return result
}

func moduleIdentity(module *listedModule) string {
	if module == nil {
		return ""
	}
	identity := module.Path + "@" + module.Version + ":" + module.GoVersion
	if module.Replace != nil {
		identity += "=>" + moduleIdentity(module.Replace) + ":" + module.Replace.Dir
	}
	return identity
}

func seedModuleChanges(baseline, staged []listedPackage, seeds map[string]bool) {
	before := make(map[string]string)
	after := make(map[string]string)
	for _, pkg := range baseline {
		before[pkg.ImportPath] = moduleIdentity(pkg.Module)
	}
	for _, pkg := range staged {
		after[pkg.ImportPath] = moduleIdentity(pkg.Module)
	}
	for name, identity := range before {
		if after[name] != identity {
			seeds[name] = true
		}
	}
	for name, identity := range after {
		if before[name] != identity {
			seeds[name] = true
		}
	}
}

func routePath(path string, plan *checkPlan, checks map[string]bool) {
	if path == "go.mod" || path == "go.sum" {
		plan.VerifyModules = true
	}
	if path == ".golangci.yml" {
		plan.LintAll = true
		checks["scripts/tests/test-verification-policy.sh"] = true
	}
	if path == "README.md" || strings.HasPrefix(path, "docs/") || strings.HasPrefix(path, "skills/") {
		plan.Documentation = true
	}
	if strings.HasPrefix(path, "skills/") {
		checks["scripts/tests/test-skill-drift.sh"] = true
	}
	if strings.HasPrefix(path, "scripts/tests/test-") && strings.HasSuffix(path, ".sh") {
		checks[path] = true
	}
	routeTooling(path, plan, checks)
}

func routeTooling(path string, plan *checkPlan, checks map[string]bool) {
	switch {
	case path == "scripts/utils/manage-write-lease.sh":
		checks["scripts/tests/test-write-coordination.sh"] = true
		fallthrough
	case path == "scripts/utils/run-staged-gate.sh" || path == "scripts/tests/test.sh":
		checks["scripts/tests/test-staged-gate.sh"] = true
		checks["scripts/tests/test-commit-authority.sh"] = true
		checks["scripts/tests/test-release-authority.sh"] = true
	case strings.HasPrefix(path, "scripts/utils/select-gate-packages/") || path == "scripts/utils/run-fast-gate.sh" || path == "scripts/utils/manage-full-proof.sh":
		checks["scripts/tests/test-fast-gate.sh"] = true
	case strings.HasPrefix(path, ".github/workflows/"):
		plan.Documentation = true
		checks["scripts/tests/test-verification-policy.sh"] = true
		checks["scripts/tests/test-release-authority.sh"] = true
	case strings.HasPrefix(path, "scripts/build/"):
		checks["scripts/tests/test-install-local.sh"] = true
		checks["scripts/tests/test-skill-drift.sh"] = true
	case strings.HasPrefix(path, "scripts/release/"):
		checks["scripts/tests/test-release-authority.sh"] = true
	case path == "scripts/utils/check-go-toolchain.sh" || path == "scripts/utils/run-vulnerability-check.sh":
		checks["scripts/tests/test-verification-policy.sh"] = true
	}
}
