package main

import (
	"slices"
	"strings"
	"testing"
)

func fixturePackage(name string) listedPackage {
	return listedPackage{Dir: "/product/" + name, ImportPath: "example/" + name, Module: &listedModule{Path: "example", Main: true}}
}

func fixtureGraph() []listedPackage {
	leaf := fixturePackage("leaf")
	consumer := fixturePackage("consumer")
	consumer.Imports = []string{leaf.ImportPath}
	testConsumer := fixturePackage("testconsumer")
	testConsumer.TestImports = []string{consumer.ImportPath}
	externalTest := fixturePackage("externaltest")
	externalTest.XTestImports = []string{leaf.ImportPath}
	return []listedPackage{leaf, consumer, testConsumer, externalTest, fixturePackage("unrelated")}
}

func TestSelectionIncludesTransitiveReverseAndTestImports(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{name: "go source", path: "leaf/value.go"},
		{name: "test source", path: "leaf/value_test.go"},
		{name: "embedded asset", path: "leaf/assets/template.js"},
		{name: "test data", path: "leaf/testdata/input.txt"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			plan, err := selectChecks("/product", fixtureGraph(), fixtureGraph(), []byte(test.path+"\x00"))
			want := []string{"example/consumer", "example/externaltest", "example/leaf", "example/testconsumer"}
			if err != nil || !slices.Equal(plan.Packages, want) {
				t.Fatalf("packages = %v, error = %v; want %v", plan.Packages, err, want)
			}
		})
	}
}

func TestDeletedPackageRetainsSurvivingDependents(t *testing.T) {
	baseline := fixtureGraph()
	staged := append([]listedPackage{}, baseline[1:]...)
	staged[0].Imports = nil
	staged[2].XTestImports = nil
	plan, err := selectChecks("/product", baseline, staged, []byte("leaf/value.go\x00"))
	want := []string{"example/consumer", "example/externaltest", "example/testconsumer"}
	if err != nil || !slices.Equal(plan.Packages, want) {
		t.Fatalf("deleted package closure = %v, %v", plan.Packages, err)
	}
}

func TestRealTestSuffixPackageIsNotConfusedWithGeneratedTestBinary(t *testing.T) {
	for _, test := range []struct {
		name string
		file string
		want bool
	}{
		{name: "real package", file: "main.go", want: true},
		{name: "generated test binary", file: "/cache/generated-test-main", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			pkg := fixturePackage("leaf.test")
			pkg.Name, pkg.GoFiles = "main", []string{test.file}
			if got := ordinaryPackage(pkg); got != test.want {
				t.Fatalf("ordinary package = %t, want %t", got, test.want)
			}
		})
	}
}

func TestModuleChangeSelectsConsumersWithoutUnrelatedPackages(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "version", true: "replacement"}[replacement], func(t *testing.T) {
			baseline, staged := fixtureGraph(), fixtureGraph()
			before := listedPackage{ImportPath: "dependency/api", Module: &listedModule{Path: "dependency", Version: "v1.0.0"}}
			after := listedPackage{ImportPath: before.ImportPath, Module: &listedModule{Path: "dependency", Version: "v1.1.0"}}
			if replacement {
				after.Module.Version = before.Module.Version
				after.Module.Replace = &listedModule{Path: "dependency", Dir: "/replacement"}
			}
			baseline[0].Imports, staged[0].Imports = []string{before.ImportPath}, []string{before.ImportPath}
			plan, err := selectChecks("/product", append(baseline, before), append(staged, after), []byte("go.mod\x00"))
			want := []string{"example/consumer", "example/externaltest", "example/leaf", "example/testconsumer"}
			if err != nil || !plan.VerifyModules || !slices.Equal(plan.Packages, want) {
				t.Fatalf("module closure = %+v, %v", plan, err)
			}
		})
	}
}

func TestDocumentationAndToolingRoutes(t *testing.T) {
	cases := []struct {
		path  string
		docs  bool
		check string
	}{
		{path: "README.md", docs: true, check: "scripts/tests/test-bootstrap.sh"},
		{path: "docs/documentation.md", docs: true},
		{path: "skills/mailcli/SKILL.md", docs: true, check: "scripts/tests/test-skill-drift.sh"},
		{path: ".github/workflows/ci.yml", docs: true, check: "scripts/tests/test-verification-policy.sh"},
		{path: "scripts/utils/manage-write-lease.sh", check: "scripts/tests/test-write-coordination.sh"},
		{path: "scripts/tests/test-bootstrap.sh", check: "scripts/tests/test-bootstrap.sh"},
		{path: ".golangci.yml", check: "scripts/tests/test-verification-policy.sh"},
	}
	for _, test := range cases {
		t.Run(test.path, func(t *testing.T) {
			plan, err := selectChecks("/product", fixtureGraph(), fixtureGraph(), []byte(test.path+"\x00"))
			if err != nil || plan.Documentation != test.docs || (test.check != "" && !slices.Contains(plan.ShellChecks, test.check)) {
				t.Fatalf("route = %+v, %v", plan, err)
			}
			if len(plan.Packages) != 0 {
				t.Fatalf("non-Go path unexpectedly selected packages: %v", plan.Packages)
			}
		})
	}
}

func TestMalformedGraphsAndUnsafePathsFailClosed(t *testing.T) {
	for _, input := range []string{"", "{", `{ "ImportPath":"broken", "Incomplete":true }`, `{ "ImportPath":"broken", "Error":{"Err":"bad import"} }`, `{ "ImportPath":"broken", "DepsErrors":[{"Err":"bad dependency"}] }`} {
		t.Run(input, func(t *testing.T) {
			if _, err := decodePackages(strings.NewReader(input)); err == nil {
				t.Fatal("invalid package graph was accepted")
			}
		})
	}
	for _, path := range []string{"../escape.go", "/escape.go", "leaf/../escape.go"} {
		t.Run(path, func(t *testing.T) {
			if _, err := selectChecks("/product", fixtureGraph(), fixtureGraph(), []byte(path+"\x00")); err == nil {
				t.Fatal("unsafe changed path was accepted")
			}
		})
	}
}
