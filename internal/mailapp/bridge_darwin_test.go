package mailapp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOSAScriptRunnerPreservesDeadlineAndReapsProcess(t *testing.T) {
	processID := 0
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, started, err := (osaScriptRunner{
		started: func(pid int) { processID = pid }, cancellationGrace: 20 * time.Millisecond,
	}).Run(
		ctx,
		`function run(_) { delay(10); return "unreachable"; }`,
		`{}`,
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("osaScriptRunner.Run() error = %v, want deadline exceeded", err)
	}
	if !started {
		t.Fatal("osaScriptRunner.Run() started = false, want true")
	}
	if processID <= 0 {
		t.Fatal("osaScriptRunner.Run() did not report the started process")
	}
	if err := syscall.Kill(processID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("timed-out osascript PID %d still exists: %v", processID, err)
	}
	if err := syscall.Kill(-processID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("timed-out osascript process group %d still exists: %v", processID, err)
	}
}

func TestOSAScriptRunnerReapsSuccessfulProcess(t *testing.T) {
	processID := 0
	output, started, err := (osaScriptRunner{started: func(pid int) { processID = pid }}).Run(
		context.Background(), `function run(_) { return "complete"; }`, `{}`,
	)
	if err != nil || strings.TrimSpace(string(output)) != "complete" {
		t.Fatalf("osaScriptRunner.Run() output = %q, error = %v", output, err)
	}
	if !started {
		t.Fatal("osaScriptRunner.Run() started = false, want true")
	}
	if processID <= 0 {
		t.Fatal("osaScriptRunner.Run() did not report the started process")
	}
	if err := syscall.Kill(processID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("successful osascript PID %d still exists: %v", processID, err)
	}
	if err := syscall.Kill(-processID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("successful osascript process group %d still exists: %v", processID, err)
	}
}

func TestProductionBridgeReadsPrivateRequestFile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, started, err := (osaScriptRunner{}).Run(ctx, bridgeScript, `{"mail_pid":0}`)
	if err != nil || !started {
		t.Fatalf("osaScriptRunner.Run() output = %q, started = %t, error = %v", output, started, err)
	}
	var response bridgeResponse
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v, output = %q", err, output)
	}
	if response.OK || response.Error == nil || response.Error.Code != "mail_not_running" {
		t.Fatalf("bridge response = %+v", response)
	}
}

func TestOSAScriptRunnerTransportsRequestBeyondARGMAX(t *testing.T) {
	request := strings.Repeat("x", 2*1024*1024)
	testScript := `
function run(argv) {
    ObjC.import("Foundation");
    const data = $.NSData.dataWithContentsOfFile(argv[0]);
    return String(data.length);
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, started, err := (osaScriptRunner{}).Run(ctx, testScript, request)
	if err != nil || !started || strings.TrimSpace(string(output)) != "2097152" {
		t.Fatalf("osaScriptRunner.Run() output = %q, started = %t, error = %v", output, started, err)
	}
}

func TestOSAScriptRunnerAllowsCooperativeCancellationCleanup(t *testing.T) {
	testScript := `
function run(argv) {
    ObjC.import("Foundation");
    while (!$.NSFileManager.defaultManager.fileExistsAtPath(argv[1])) {
        $.NSThread.sleepForTimeInterval(0.01);
    }
    return "cancelled-cleanly";
}`
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	output, started, err := (osaScriptRunner{cancellationGrace: time.Second}).Run(ctx, testScript, `{}`)
	if err != nil || !started || strings.TrimSpace(string(output)) != "cancelled-cleanly" {
		t.Fatalf("osaScriptRunner.Run() output = %q, started = %t, error = %v", output, started, err)
	}
}

func TestBridgeMapsAutomationDenial(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    const error = new Error("Not authorized to send Apple events. (-1743)");
    error.number = -1743;
    return JSON.stringify(bridgeFailure(error));
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if !strings.Contains(string(output), `"code":"mail_automation_denied"`) ||
		!strings.Contains(string(output), `System Settings > Privacy & Security > Automation`) {
		t.Fatalf("automation failure = %q", output)
	}
}

func TestBridgeRejectsNonMailProcessIdentity(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    ObjC.import("Foundation");
    const processID = Number($.NSProcessInfo.processInfo.processIdentifier);
    try {
        verifyMailProcess(processID);
        return JSON.stringify({code: "none"});
    } catch (error) {
        return JSON.stringify({code: error.bridgeCode});
    }
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"code":"mail_process_changed"}` {
		t.Fatalf("process identity result = %q", output)
	}
}

func TestBridgeCursorValidationDoesConstantWork(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    let reads = 0;
    const messages = [
        {id: () => { reads += 1; return 10; }},
        {id: () => { reads += 1; return 20; }},
        {id: () => { reads += 1; return 30; }}
    ];
    const start = pageStart(messages, 2, "20");
    return JSON.stringify({start: start, reads: reads});
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"start":2,"reads":1}` {
		t.Fatalf("output = %q", output)
	}
}

func TestBridgeCachesAccountAndMailboxResolutionPerRequest(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    let accountReads = 0;
    let rootMailboxReads = 0;
    let childMailboxReads = 0;
    const inbox = {
        name: () => "Inbox",
        mailboxes: () => { childMailboxReads += 1; return []; }
    };
    const account = {
        enabled: () => true,
        id: () => "account-id",
        name: () => "Primary",
        mailboxes: () => { rootMailboxReads += 1; return [inbox]; }
    };
    const mail = {accounts: () => { accountReads += 1; return [account]; }};
    const resolution = createResolutionContext();
    const firstAccount = resolveAccount(mail, "account-id", resolution);
    const secondAccount = resolveAccount(mail, "account-id", resolution);
    resolveMailbox(firstAccount, ["Inbox"], resolution);
    resolveMailbox(secondAccount, ["Inbox"], resolution);
    return JSON.stringify({
        account_reads: accountReads,
        root_mailbox_reads: rootMailboxReads,
        child_mailbox_reads: childMailboxReads
    });
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"account_reads":1,"root_mailbox_reads":1,"child_mailbox_reads":0}` {
		t.Fatalf("resolution reads = %q", output)
	}
}

func TestBridgeRejectsOversizedMessagePageBeforeMailAccess(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    let accountReads = 0;
    const mail = {accounts: () => { accountReads += 1; return []; }};
    try {
        listMessages(mail, {limit: 26}, createResolutionContext());
        return JSON.stringify({code: "none", account_reads: accountReads});
    } catch (error) {
        return JSON.stringify({code: error.bridgeCode, account_reads: accountReads});
    }
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"code":"invalid_request","account_reads":0}` {
		t.Fatalf("oversized page result = %q", output)
	}
}

func TestProductionBridgeContainsNoWhoseFilter(t *testing.T) {
	if strings.Contains(bridgeScript, ".whose(") {
		t.Fatal("bridge contains an unbounded whose filter")
	}
}

func TestProductionBridgeContainsNoMailboxWideMessageRead(t *testing.T) {
	if strings.Contains(bridgeScript, "mailbox.messages()") {
		t.Fatal("bridge contains a mailbox-wide message read")
	}
}

func TestProductionBridgeContainsNoUIOrPollingControl(t *testing.T) {
	forbidden := []string{"delay(", ".activate(", ".launch(", ".quit(", "System Events"}
	for _, fragment := range forbidden {
		if strings.Contains(bridgeScript, fragment) {
			t.Fatalf("bridge contains forbidden UI or polling control %q", fragment)
		}
	}
}

func TestProductionBridgeTargetsOnlyTheGatedMailProcess(t *testing.T) {
	if strings.Contains(bridgeScript, `Application("Mail")`) ||
		!strings.Contains(bridgeScript, "Application(request.mail_pid)") ||
		!strings.Contains(bridgeScript, "Number.isInteger(request.mail_pid)") {
		t.Fatal("bridge is not bound exclusively to the access-gate Mail PID")
	}
}
