package mailapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"mailcli/internal/mail"
)

const (
	mailApplicationPath = "/System/Applications/Mail.app"
	mailDefinitionPath  = "/System/Applications/Mail.app/Contents/Resources/Mail.sdef"
	osaScriptPath       = "/usr/bin/osascript"
)

func Probe(ctx context.Context, live bool, gate accessGate) mail.DiagnosticReport {
	checks := []mail.Check{
		equalCheck("platform", runtime.GOOS, "darwin", "unsupported_platform"),
		equalCheck("architecture", runtime.GOARCH, "arm64", "unsupported_architecture"),
		pathCheck("osascript", osaScriptPath, "osascript_unavailable"),
		pathCheck("mail-app", mailApplicationPath, "mail_app_unavailable"),
		pathCheck("mail-scripting-definition", mailDefinitionPath, "mail_scripting_unavailable"),
	}

	if live {
		checks = append(checks, probeAutomation(ctx, gate))
	} else {
		checks = append(checks, mail.Check{
			Name:   "mail-automation",
			Status: "not-run",
			Detail: "use doctor --live to verify read-only Apple Events access and Mail process identity",
		})
	}

	return mail.DiagnosticReport{Checks: checks}
}

func equalCheck(name string, got string, want string, code string) mail.Check {
	if got == want {
		return mail.Check{Name: name, Status: "pass", Detail: got}
	}

	return mail.Check{
		Name:   name,
		Status: "fail",
		Code:   code,
		Detail: fmt.Sprintf("got %s, require %s", got, want),
	}
}

func pathCheck(name string, path string, code string) mail.Check {
	if _, err := os.Stat(path); err != nil {
		return mail.Check{Name: name, Status: "fail", Code: code, Detail: err.Error()}
	}

	return mail.Check{Name: name, Status: "pass", Detail: path}
}

func probeAutomation(ctx context.Context, gate accessGate) mail.Check {
	return probeAutomationWithRunner(ctx, gate, osaScriptRunner{})
}

func probeAutomationWithRunner(ctx context.Context, gate accessGate, runner scriptRunner) mail.Check {
	var lease accessLease = noOpAccessLease{}
	if gate != nil {
		var err error
		lease, err = gate.Acquire(ctx)
		if err != nil {
			mapped := mapAccessGateError(err)
			check := mail.Check{
				Name: "mail-automation", Status: "fail", Detail: mapped.Error(),
			}
			var operationError *OperationError
			if errors.As(mapped, &operationError) {
				check.Code = operationError.ErrorCode()
			}
			return check
		}
	}
	output, _, err := runner.Run(
		ctx,
		fmt.Sprintf(
			`function run(_) {
    ObjC.import("AppKit");
    const processID = %d;
    const running = $.NSRunningApplication.runningApplicationWithProcessIdentifier(processID);
    let bundleIdentifier = "";
    try { bundleIdentifier = ObjC.unwrap(running.bundleIdentifier); } catch (_) {}
    if (bundleIdentifier !== "com.apple.mail") {
        throw new Error("Mail.app process identity changed before the Apple Events probe");
    }
    return String(Application(processID).version());
}`,
			lease.TargetPID(),
		),
		`{}`,
	)
	releaseErr := lease.Release(false)
	if err != nil {
		detail := fmt.Sprintf("Apple Events probe failed: %v: %s", err, strings.TrimSpace(string(output)))
		if releaseErr != nil {
			detail += fmt.Sprintf("; release Mail.app access gate: %v", releaseErr)
		}
		code, remediation := classifyAutomationFailure(err, output)
		return mail.Check{Name: "mail-automation", Status: "fail", Code: code, Detail: detail + remediation}
	}
	if releaseErr != nil {
		return mail.Check{
			Name: "mail-automation", Status: "fail",
			Code:   "mail_access_gate_failed",
			Detail: fmt.Sprintf("release Mail.app access gate: %v", releaseErr),
		}
	}

	return mail.Check{
		Name:   "mail-automation",
		Status: "pass",
		Detail: "Mail.app " + strings.TrimSpace(string(output)),
	}
}

func classifyAutomationFailure(err error, output []byte) (string, string) {
	detail := strings.ToLower(err.Error() + " " + string(output))
	if strings.Contains(detail, "-1743") || strings.Contains(detail, "not authorized to send apple events") ||
		strings.Contains(detail, "not authorised to send apple events") {
		return "mail_automation_denied",
			"; allow the calling host to control Mail in System Settings > Privacy & Security > Automation"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "mail_automation_timeout", "; retry after Mail.app becomes responsive"
	}
	return "mail_automation_failed", ""
}
