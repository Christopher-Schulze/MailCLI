package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type bootstrapZeroReader struct{}

func (bootstrapZeroReader) Read(value []byte) (int, error) {
	clear(value)
	return len(value), nil
}

func writeBootstrapArchive(t *testing.T, path, scenario string) {
	t.Helper()
	scenario = strings.TrimPrefix(scenario, "legacy ")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	level := gzip.BestSpeed
	if scenario == "archive overflow" {
		level = gzip.NoCompression
	}
	compressed, err := gzip.NewWriterLevel(file, level)
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewWriter(compressed)
	root := "mailcli_9.9.9_darwin_arm64"
	for _, entry := range []struct{ name, body string }{
		{"bin/mailcli", "#!/bin/bash\nprintf 'mailcli 9.9.9\\n'\n"},
		{"install.sh", "#!/bin/bash\nprintf executed > \"$MAILCLI_TEST_BOOTSTRAP_MARKER\"\n"},
	} {
		if err := archive.WriteHeader(&tar.Header{Name: root + "/" + entry.name, Mode: 0o755, Size: int64(len(entry.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(archive, entry.body); err != nil {
			t.Fatal(err)
		}
	}
	count := 1
	switch scenario {
	case "entry overflow":
		count = maximumExtractedFileCount
	case "entries at limit":
		count = maximumExtractedFileCount - 2
	}
	for index := 0; index < count; index++ {
		header := &tar.Header{Name: fmt.Sprintf("%s/file-%d", root, index), Mode: 0o600, Typeflag: tar.TypeReg}
		switch scenario {
		case "archive overflow":
			header.Size = maximumReleaseArchive + 1024
		case "expanded overflow":
			header.Size = maximumExtractedPackage + 1024
		case "unsafe whitespace path":
			header.Name = "outside " + root + "/innocent"
		case "unsupported link":
			header.Typeflag, header.Linkname = tar.TypeSymlink, "/tmp"
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := io.CopyN(archive, bootstrapZeroReader{}, header.Size); err != nil {
			t.Fatal(err)
		}
	}
	for _, closer := range []io.Closer{archive, compressed, file} {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBootstrapResourceBoundaries(t *testing.T) {
	for _, scenario := range []string{
		"normal", "checksum at limit", "checksum overflow", "archive overflow", "entry overflow",
		"expanded overflow", "unsafe whitespace path", "unsupported link", "redirect loop", "checksum deadline",
		"legacy checksum overflow", "legacy archive overflow", "entries at limit",
	} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			archiveName := "mailcli_9.9.9_darwin_arm64.tar.gz"
			archivePath := filepath.Join(root, archiveName)
			writeBootstrapArchive(t, archivePath, scenario)
			file, err := os.Open(archivePath)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.New()
			if _, err := io.Copy(digest, file); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			manifest := fmt.Sprintf("%x  %s\n", digest.Sum(nil), archiveName)
			if scenario == "checksum at limit" || strings.HasSuffix(scenario, "checksum overflow") {
				size := maximumChecksumFile
				if strings.HasSuffix(scenario, "checksum overflow") {
					size += 1024
				}
				manifest += "#" + strings.Repeat(" ", size-len(manifest)-1)
			}
			var latestRequests, latestGETs atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch filepath.Base(request.URL.Path) {
				case "latest":
					latestRequests.Add(1)
					if request.Method != http.MethodHead {
						latestGETs.Add(1)
					}
					if scenario == "redirect loop" {
						http.Redirect(writer, request, "/latest", http.StatusFound)
					}
				case "SHA256SUMS":
					if scenario == "checksum deadline" {
						select {
						case <-request.Context().Done():
						case <-time.After(45 * time.Second):
						}
						return
					}
					writer.(http.Flusher).Flush() // No Content-Length, including overflow cases.
					if _, err := io.WriteString(writer, manifest); err != nil {
						return // A bounded client is expected to close the stream early.
					}
				case archiveName:
					writer.(http.Flusher).Flush()
					stream, err := os.Open(archivePath)
					if err != nil {
						t.Error(err)
						return
					}
					if _, err := io.Copy(writer, stream); err != nil && request.Context().Err() == nil {
						t.Logf("download client closed: %v", err)
					}
					if err := stream.Close(); err != nil {
						t.Error(err)
					}
				default:
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()
			certificate := filepath.Join(root, "certificate.pem")
			if err := os.WriteFile(certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
				t.Fatal(err)
			}
			shimRoot := filepath.Join(root, "commands")
			if err := os.Mkdir(shimRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			// Adapt only the GitHub provider address and effective-URL evidence.
			// Real curl, TLS, chunked transfers, deadlines, hashes, tar and Bash run.
			// Legacy cases omit curl's byte guard to prove the stdout pipe bounds it.
			shim := `#!/bin/bash
set -euo pipefail
args=()
skip=false
for argument in "$@"; do
  if [[ "$skip" == true ]]; then skip=false; continue; fi
  if [[ "$MAILCLI_TEST_BOOTSTRAP_SCENARIO" == legacy* && "$argument" == --max-filesize ]]; then
    skip=true; continue
  fi
  case "$argument" in
    https://github.com/Christopher-Schulze/MailCLI/*) args+=("$MAILCLI_TEST_BOOTSTRAP_SERVER/${argument##*/}") ;;
    '%{url_effective}') args+=('https://github.com/Christopher-Schulze/MailCLI/releases/tag/v9.9.9') ;;
    *) args+=("$argument") ;;
  esac
done
exec /usr/bin/curl --disable --noproxy '*' --cacert "$MAILCLI_TEST_BOOTSTRAP_CERT" "${args[@]}"
`
			if err := os.WriteFile(filepath.Join(shimRoot, "curl"), []byte(shim), 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "installer-executed")
			binary := filepath.Join(root, "installed-mailcli")
			if err := os.WriteFile(binary, []byte("#!/bin/bash\nprintf 'mailcli 9.9.9\\n'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "/bin/bash", filepath.Join("..", "..", "scripts", "release", "install-latest.sh"))
			command.Env = append(os.Environ(), "PATH="+shimRoot+":/usr/bin:/bin:/usr/sbin:/sbin", "HOME="+root, "TMPDIR="+root,
				"MAILCLI_TEST_BOOTSTRAP_SERVER="+server.URL, "MAILCLI_TEST_BOOTSTRAP_CERT="+certificate,
				"MAILCLI_TEST_BOOTSTRAP_SCENARIO="+scenario,
				"MAILCLI_TEST_BOOTSTRAP_MARKER="+marker, "MAILCLI_BINARY_DESTINATION="+binary)
			started := time.Now()
			output, runErr := command.CombinedOutput()
			_, markerErr := os.Stat(marker)
			if scenario == "normal" || scenario == "checksum at limit" || scenario == "entries at limit" {
				if runErr != nil || markerErr != nil || latestGETs.Load() != 0 {
					t.Fatalf("ordinary release failed or downloaded latest body: err=%v marker=%v GETs=%d output=%s", runErr, markerErr, latestGETs.Load(), output)
				}
				return
			}
			if runErr == nil || !os.IsNotExist(markerErr) || ctx.Err() != nil {
				t.Fatalf("resource boundary failed before installation: err=%v marker=%v context=%v output=%s", runErr, markerErr, ctx.Err(), output)
			}
			switch scenario {
			case "redirect loop":
				if latestRequests.Load() > maximumUpdateRedirects+1 {
					t.Fatalf("redirect limit exceeded: %d requests", latestRequests.Load())
				}
			case "checksum deadline":
				if time.Since(started) > 35*time.Second || !strings.Contains(string(output), "timed out") {
					t.Fatalf("transfer deadline not enforced: elapsed=%s output=%s", time.Since(started), output)
				}
			case "checksum overflow", "archive overflow", "legacy checksum overflow", "legacy archive overflow":
				if !strings.Contains(string(output), "exceeds") {
					t.Fatalf("download budget not diagnosed: %s", output)
				}
			case "entry overflow", "expanded overflow":
				if !strings.Contains(string(output), "extraction limits") {
					t.Fatalf("archive budget not diagnosed: %s", output)
				}
			}
		})
	}
}
