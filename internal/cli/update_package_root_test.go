package cli

import (
	"context"
	"crypto/sha256"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestUpdatePackageRootBinding(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	binaryPath := filepath.Join(t.TempDir(), "mailcli")
	command := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-mod=readonly", "-o", binaryPath, "../../cmd/mailcli")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build real update payload: %v: %s", err, output)
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	installer, err := os.ReadFile(filepath.Join("..", "..", "scripts", "release", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	root := "mailcli_" + version + "_darwin_arm64"
	files := map[string]updateArchiveFile{
		root + "/bin/mailcli":                       {content: string(binary), mode: 0o755},
		root + "/install.sh":                        {content: string(installer), mode: 0o755},
		root + "/skills/mailcli/SKILL.md":           {content: "authenticated skill\n", mode: 0o600},
		root + "/skills/mailcli/agents/openai.yaml": {content: "authenticated agent\n", mode: 0o600},
	}
	archive := buildUpdateArchive(t, files)
	checksums := checksumFile(root+".tar.gz", archive)
	server := newUpdateTestServer(t, version, archive, checksums)
	defer server.Close()
	for _, test := range []struct {
		name          string
		alternate     string
		retainedError string
	}{
		{name: "same version", alternate: version},
		{name: "stale version", alternate: "0.0.1"},
		{name: "invalid binary", alternate: "invalid"},
		{name: "nonexistent root"},
		{name: "relative root"},
		{name: "symlink root"},
		{name: "root directory"},
		{name: "retained backup", alternate: version, retainedError: "backup"},
		{name: "retained recovery record", alternate: version, retainedError: "manifest"},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment := updateTestEnvironment(t, server, "0.0.0")
			createInstalledUpdateFixture(t, environment, "0.0.0", "old skill")
			verified := 0
			environment.verifyPackage = func(ctx context.Context, path, expected string) error {
				verified++
				return verifyReleaseBinary(ctx, path, expected)
			}
			environment.verifyInstallation = verifyInstalledBinary
			alternateRoot := t.TempDir()
			for _, directory := range []string{"bin", "skills/mailcli/agents"} {
				if err := os.MkdirAll(filepath.Join(alternateRoot, directory), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			alternateBinary := "#!/bin/sh\nprintf invoked >> \"$MAILCLI_TEST_UNTRUSTED_EXECUTION\"\n" + testUpdateBinary(test.alternate)
			for path, contents := range map[string]string{
				"bin/mailcli": alternateBinary, "skills/mailcli/SKILL.md": "alternate skill\n", "skills/mailcli/agents/openai.yaml": "alternate agent\n",
			} {
				if err := os.WriteFile(filepath.Join(alternateRoot, path), []byte(contents), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			override := alternateRoot
			switch test.name {
			case "nonexistent root":
				override = filepath.Join(alternateRoot, "absent")
			case "relative root":
				override = "relative-package"
			case "symlink root":
				override = filepath.Join(t.TempDir(), "package-link")
				if err := os.Symlink(alternateRoot, override); err != nil {
					t.Fatal(err)
				}
			case "root directory":
				override = "/"
			}
			sentinel := filepath.Join(t.TempDir(), "untrusted-executed")
			t.Setenv("MAILCLI_INSTALL_PACKAGE_ROOT", override)
			t.Setenv("MAILCLI_TEST_UNTRUSTED_EXECUTION", sentinel)
			retainedPath := ""
			if test.retainedError != "" {
				retainedPath = environment.executablePath + ".mailcli-backup"
				if test.retainedError == "manifest" {
					retainedPath = filepath.Join(environment.homeDirectory, "Library", "Application Support", "MailCLI", "install-transactions", "txn.retained", "manifest")
					if err := os.MkdirAll(filepath.Dir(retainedPath), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(retainedPath, []byte("retained recovery evidence\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			operationCtx, operationCancel := context.WithTimeout(ctx, 30*time.Second)
			defer operationCancel()
			result, updateErr := performUpdate(operationCtx, environment, newUpdateReporter(io.Discard, false, false))
			if verified != 1 {
				t.Errorf("package verification calls = %d", verified)
			}
			if test.retainedError == "" {
				if updateErr != nil || !result.Updated || result.LatestVersion != version {
					t.Errorf("authenticated update = %+v, %v", result, updateErr)
				}
			} else {
				if updateErrorCodeForTest(updateErr) != "update_install_failed" || result.Updated {
					t.Errorf("unsafe recovery record did not stop update: %+v, %v", result, updateErr)
				}
				retained, err := os.ReadFile(retainedPath)
				if err != nil || string(retained) != "retained recovery evidence\n" {
					t.Errorf("recovery evidence changed: %q, %v", retained, err)
				}
			}
			if _, err := os.Lstat(sentinel); !os.IsNotExist(err) {
				t.Errorf("ambient package binary was executed: %v", err)
			}
			wantBinary, wantSkill, wantAgent := binary, "authenticated skill\n", "authenticated agent\n"
			if test.retainedError != "" {
				wantBinary, wantSkill, wantAgent = []byte(testUpdateBinary("0.0.0")), "old skill\n", "old agent\n"
			}
			for path, wanted := range map[string][]byte{
				environment.executablePath: wantBinary,
				filepath.Join(environment.homeDirectory, ".agents", "skills", "mailcli", "SKILL.md"):              []byte(wantSkill),
				filepath.Join(environment.homeDirectory, ".agents", "skills", "mailcli", "agents", "openai.yaml"): []byte(wantAgent),
				filepath.Join(alternateRoot, "bin", "mailcli"):                                                    []byte(alternateBinary),
			} {
				actual, err := os.ReadFile(path)
				if err != nil || sha256.Sum256(actual) != sha256.Sum256(wanted) {
					t.Errorf("payload identity changed at %s: %v", path, err)
				}
			}
		})
	}
}
