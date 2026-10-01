package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mailcli/internal/mail"
)

func TestUpdateTLSCauseKeepsSanitizedPublicMessage(t *testing.T) {
	verification := &tls.CertificateVerificationError{Err: errors.New("untrusted certificate")}
	requestError := &url.Error{Op: "Get", URL: "https://github.com/latest?token=PRIVATE_TOKEN", Err: verification}
	err := contextualUpdateFailure("update_check_failed", "check latest release", sanitizeUpdateRequestError(requestError))
	failure := newErrorData("update", responseData{}, err)
	var retained *tls.CertificateVerificationError
	if !errors.As(err, &retained) || failure.Message != "check latest release: release request failed" {
		t.Fatalf("TLS cause changed the sanitized public message: %+v, cause=%v", failure, retained)
	}
}

func TestUpdateRequestCancellationRetainsCauseAndStops(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deadline"}[deadline], func(t *testing.T) {
			started := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
				close(started)
				<-request.Context().Done()
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			cause := context.DeadlineExceeded
			if !deadline {
				cause = context.Canceled
				done := make(chan struct{})
				go func() {
					select {
					case <-started:
						cancel()
					case <-ctx.Done():
					}
					close(done)
				}()
				defer func() { cancel(); <-done }()
			}
			environment := updateEnvironment{
				client: server.Client(), metadataURL: server.URL + "/latest?token=PRIVATE_TOKEN", currentVersion: "1.0.4",
				urlPolicy: testUpdateURLPolicy(t, server.URL), operatingSystem: "darwin", architecture: "arm64", checkOnly: true,
			}
			_, _, err := fetchLatestRelease(ctx, environment)
			if !errors.Is(err, cause) || errorCode(err) != "update_check_failed" || strings.Contains(err.Error(), "PRIVATE_TOKEN") || strings.Contains(err.Error(), server.URL) {
				t.Fatalf("request cause lost or URL exposed: code=%s, error=%v", errorCode(err), err)
			}
			var stdout, stderr bytes.Buffer
			if code := runUpdateWithEnvironment(ctx, true, environment, &stdout, &stderr); code != 1 {
				t.Fatalf("canceled update code=%d", code)
			}
			var response envelope
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			want := "retry"
			if !deadline {
				want = "stop"
			}
			if response.Next == nil || response.Next.Do != want || response.Error == nil || response.Error.Guidance.EffectCertainty != mail.EffectNone || strings.Contains(stdout.String()+stderr.String(), "PRIVATE_TOKEN") {
				t.Fatalf("cancellation envelope=%s", &stdout)
			}
		})
	}
}

func TestUpdateInvalidMetadataAndVersionsAreTerminal(t *testing.T) {
	for _, test := range []struct{ name, payload, current string }{
		{"malformed JSON", "{", "1.0.4"},
		{"incomplete metadata", "{}", "1.0.4"},
		{"invalid field type", `{"tag_name":5,"html_url":"RELEASE_URL"}`, "1.0.4"},
		{"invalid release version", `{"tag_name":"v1.0.beta","html_url":"RELEASE_URL"}`, "1.0.4"},
		{"invalid installed version", `{"tag_name":"v1.0.5","html_url":"RELEASE_URL"}`, "dev"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if _, err := io.WriteString(writer, strings.ReplaceAll(test.payload, "RELEASE_URL", "http://"+request.Host+"/release")); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			home := t.TempDir()
			environment := updateEnvironment{
				client: server.Client(), metadataURL: server.URL + "/latest", currentVersion: test.current, homeDirectory: home,
				urlPolicy: testUpdateURLPolicy(t, server.URL), operatingSystem: "darwin", architecture: "arm64", checkOnly: true,
			}
			var stdout, stderr bytes.Buffer
			code := runUpdateWithEnvironment(context.Background(), true, environment, &stdout, &stderr)
			var response envelope
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || code != 1 || response.Error == nil || response.Error.Code != "update_package_invalid" || response.Next == nil || response.Next.Do != "stop" || response.Error.Guidance.ReplayAllowed {
				t.Fatalf("permanent metadata failure permits retry: code=%d, decode=%v, output=%s", code, err, &stdout)
			}
			if _, err := os.Stat(filepath.Join(home, "Library")); !os.IsNotExist(err) {
				t.Fatalf("invalid metadata created install state: %v", err)
			}
		})
	}
}

func TestUpdateDownloadLimitsAreTerminal(t *testing.T) {
	for _, contentLength := range []bool{false, true} {
		t.Run(map[bool]string{false: "chunked", true: "declared"}[contentLength], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				if contentLength {
					writer.Header().Set("Content-Length", "33")
				} else {
					writer.WriteHeader(http.StatusOK)
					writer.(http.Flusher).Flush()
				}
				if _, err := io.WriteString(writer, strings.Repeat("x", 33)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			_, err := downloadUpdateResource(context.Background(), server.Client(), server.URL, 16)
			guidance := mail.GuidanceForError("update", err)
			if errorCode(err) != "update_package_invalid" || guidance.Retryability != mail.RetryTerminal || guidance.ReplayAllowed {
				t.Fatalf("oversize request error=%v, guidance=%+v", err, guidance)
			}
		})
	}
}

func TestUpdateBoundaryErrorsRetainCancellation(t *testing.T) {
	for _, stage := range []string{"installed version", "package verification", "installer", "installed verification"} {
		t.Run(stage, func(t *testing.T) {
			environment := newIsolatedUpdateEnvironment(t)
			switch stage {
			case "installed version":
				environment.readInstalledVersion = func(context.Context, string) (string, error) { return "", context.Canceled }
			case "package verification":
				environment.verifyPackage = func(context.Context, string, string) error { return context.Canceled }
			case "installer":
				install := environment.installPackage
				environment.installPackage = func(ctx context.Context, path, binary, home string, lock *os.File) error {
					if err := install(ctx, path, binary, home, lock); err != nil {
						return err
					}
					return context.Canceled
				}
			case "installed verification":
				environment.verifyInstallation = func(context.Context, string, string) error { return context.Canceled }
			}
			result, err := performUpdate(context.Background(), environment, newUpdateReporter(io.Discard, false, false))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("%s lost cause: %v", stage, err)
			}
			if stage == "installer" || stage == "installed verification" {
				failure := newErrorData("update", responseData{UpdateResult: &result}, err)
				next := failureNextAction(failure, nil)
				if result.FailedPhase == "" || next.Do != "check_state" || failure.Guidance.ReplayAllowed || failure.Guidance.Recovery.Command != "version" {
					t.Fatalf("cancellation erased install uncertainty: result=%+v, next=%+v", result, next)
				}
			} else {
				failure := newErrorData("update", responseData{UpdateResult: &result}, err)
				next := failureNextAction(failure, nil)
				if next.Do != "stop" || failure.Guidance.EffectCertainty != mail.EffectNone {
					t.Fatalf("pre-install cancellation suggests an uncertain effect: next=%+v, guidance=%+v", next, failure.Guidance)
				}
			}
		})
	}
}
