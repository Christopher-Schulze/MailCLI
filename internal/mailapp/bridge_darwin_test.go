package mailapp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOSAScriptRunnerPreservesDeadlineAndReapsProcess(t *testing.T) {
	processID := 0
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := (osaScriptRunner{started: func(pid int) { processID = pid }}).Run(
		ctx,
		`function run(_) { delay(10); return "unreachable"; }`,
		`{}`,
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("osaScriptRunner.Run() error = %v, want deadline exceeded", err)
	}
	if processID <= 0 {
		t.Fatal("osaScriptRunner.Run() did not report the started process")
	}
	if err := syscall.Kill(processID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("timed-out osascript PID %d still exists: %v", processID, err)
	}
}

func TestOSAScriptRunnerReapsSuccessfulProcess(t *testing.T) {
	processID := 0
	output, err := (osaScriptRunner{started: func(pid int) { processID = pid }}).Run(
		context.Background(), `function run(_) { return "complete"; }`, `{}`,
	)
	if err != nil || strings.TrimSpace(string(output)) != "complete" {
		t.Fatalf("osaScriptRunner.Run() output = %q, error = %v", output, err)
	}
	if processID <= 0 {
		t.Fatal("osaScriptRunner.Run() did not report the started process")
	}
	if err := syscall.Kill(processID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("successful osascript PID %d still exists: %v", processID, err)
	}
}

type recursionResult struct {
	Path []string `json:"path"`
}

func TestBridgeRecursivelyCollectsNestedMailboxes(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    const child = {
        name: () => "Child",
        unreadCount: () => 2,
        mailboxes: () => []
    };
    const parent = {
        name: () => "Parent",
        unreadCount: () => 1,
        mailboxes: () => [child]
    };
    const output = [];
    collectMailboxes({id: "account-id"}, [parent], [], output);
    return JSON.stringify(output.map(item => ({path: item.path})));
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}

	var result []recursionResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("result count = %d, want 2", len(result))
	}
	if len(result[1].Path) != 2 || result[1].Path[0] != "Parent" || result[1].Path[1] != "Child" {
		t.Fatalf("nested path = %#v", result[1].Path)
	}
}

func TestBridgeUsesDirectMessageIDLookup(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    const expected = {exists: () => true, id: () => 42};
    const mailbox = {
        messages: {
            byId: identifier => identifier === 42 ? expected : {exists: () => false}
        }
    };
    const message = findMessage(mailbox, "42");
    return JSON.stringify({id: message.id()});
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"id":42}` {
		t.Fatalf("output = %q", output)
	}
}

func TestBridgeDraftLookupDoesNotScanMailboxAfterStaleLibraryID(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    let scans = 0;
    const messages = () => { scans += 1; return []; };
    messages.byId = () => ({exists: () => false});
    try {
        findDraftMessage({messages: messages}, "42", "stable-id", "Subject");
        return JSON.stringify({code: "none", scans: scans});
    } catch (error) {
        return JSON.stringify({code: error.bridgeCode, scans: scans});
    }
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"code":"not_found","scans":0}` {
		t.Fatalf("output = %q", output)
	}
}

func TestBridgeDraftLookupRejectsMismatchedDirectIdentity(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    const wrong = {exists: () => true, messageId: () => "stable-id", subject: () => "Wrong"};
    const messages = () => { throw new Error("mailbox scan"); };
    messages.byId = () => wrong;
    try {
        findDraftMessage({messages: messages}, "42", "stable-id", "Right");
        return JSON.stringify({code: "none"});
    } catch (error) {
        return JSON.stringify({code: error.bridgeCode});
    }
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"code":"stale_reference"}` {
		t.Fatalf("output = %q", output)
	}
}

func TestBridgeMarksOnlyActualSendInvocationAsAttempted(t *testing.T) {
	tests := []struct {
		name          string
		applyFunction string
		sendFunction  string
		want          string
	}{
		{
			name:          "field preparation failed",
			applyFunction: `function applyOutgoingFields() { throw new Error("prepare"); }`,
			sendFunction:  `() => true`, want: `{"attempted":false,"closed":true}`,
		},
		{
			name:          "send invocation failed",
			applyFunction: `function applyOutgoingFields() {}`,
			sendFunction:  `() => { throw new Error("send"); }`, want: `{"attempted":true,"closed":false}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testScript := bridgeScript + `
let closed = false;
function outgoingForDraft() { return {send: ` + test.sendFunction + `, close: () => { closed = true; }}; }
` + test.applyFunction + `
function run(_) {
    try {
        sendDraft({}, {draft: {kind: "new", from: "sender@example.com"}});
        return JSON.stringify({attempted: true, closed: closed});
    } catch (error) {
        return JSON.stringify({attempted: Boolean(error.sendAttempted), closed: closed});
    }
}`
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			output, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
			if err != nil {
				t.Fatalf("osaScriptRunner.Run() error = %v", err)
			}
			if strings.TrimSpace(string(output)) != test.want {
				t.Fatalf("output = %q, want %s", output, test.want)
			}
		})
	}
}

func TestBridgeRejectsNewSendWithoutSenderBeforeComposeCreation(t *testing.T) {
	testScript := bridgeScript + `
let composeCreated = false;
function outgoingForDraft() {
    composeCreated = true;
    return {send: () => true};
}
function run(_) {
    try {
        sendDraft({}, {draft: {kind: "new"}});
        return JSON.stringify({code: "none", compose_created: composeCreated});
    } catch (error) {
        return JSON.stringify({code: error.bridgeCode, compose_created: composeCreated});
    }
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"code":"invalid_request","compose_created":false}` {
		t.Fatalf("output = %q", output)
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
	output, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
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
	output, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
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
	output, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
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

func TestProductionBridgeNeverMakesComposeWindowsVisible(t *testing.T) {
	if strings.Contains(bridgeScript, ".visible = true") {
		t.Fatal("bridge makes a compose window visible")
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

func TestInstalledMailDefinitionContainsRequiredWriteSurface(t *testing.T) {
	payload, err := os.ReadFile(mailDefinitionPath)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", mailDefinitionPath, err)
	}
	definition := string(payload)
	required := []string{
		`<command name="forward"`, `<command name="reply"`, `<command name="send"`,
		`<responds-to command="save">`,
		`<command name="synchronize"`, `<class name="outgoing message"`,
		`<property name="read status"`, `<property name="flagged status"`,
		`<property name="junk mail status"`, `<class name="mail attachment"`,
	}
	for _, fragment := range required {
		if !strings.Contains(definition, fragment) {
			t.Fatalf("Mail.sdef missing %q", fragment)
		}
	}
}
