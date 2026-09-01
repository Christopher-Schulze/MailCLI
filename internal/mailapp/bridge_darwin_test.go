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
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
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
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
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
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
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
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
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
let composeExists = true;
function outgoingForDraft() { return {
    send: ` + test.sendFunction + `,
    close: () => { closed = true; composeExists = false; },
    exists: () => composeExists
}; }
` + test.applyFunction + `
function waitForStableCompose() { return {attachment_paths: []}; }
function waitForPreparedCompose() {
    return {from: "sender@example.com", to: [{address: "to@example.com"}], cc: [], bcc: [], subject: "", body: "", attachment_paths: []};
}
function composeSnapshot() {
    return {from: "sender@example.com", to: [{address: "to@example.com"}], cc: [], bcc: [], subject: "", body: "", attachment_paths: []};
}
function run(_) {
    try {
		sendDraft({outgoingMessages: () => []}, {draft: {
			kind: "new", from: "sender@example.com", to: [{address: "to@example.com"}],
			cc: [], bcc: [], subject: "", body: "", attachments: []
		}});
        return JSON.stringify({attempted: true, closed: closed});
    } catch (error) {
        return JSON.stringify({attempted: Boolean(error.sendAttempted), closed: closed});
    }
}`
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
			if err != nil {
				t.Fatalf("osaScriptRunner.Run() error = %v", err)
			}
			if strings.TrimSpace(string(output)) != test.want {
				t.Fatalf("output = %q, want %s", output, test.want)
			}
		})
	}
}

func TestBridgeDeduplicatesNativeReplyRecipients(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    const recipients = [{address: () => "person@example.com", name: () => "Existing"}];
    const outgoing = {toRecipients: () => recipients};
    outgoing.toRecipients.push = value => recipients.push(value);
    const mail = {ToRecipient: value => ({address: () => value.address, name: () => value.name})};
    addRecipients(mail, outgoing, "toRecipients", "ToRecipient", [
        {address: "PERSON@example.com"}, {address: "second@example.com"}
    ]);
    return JSON.stringify(recipients.map(recipient => recipient.address()));
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `["person@example.com","second@example.com"]` {
		t.Fatalf("recipient result = %q", output)
	}
}

func TestBridgeComposeWaitHasStrictSnapshotBudget(t *testing.T) {
	testScript := bridgeScript + `
function sleepForCompose(_) {}
function run(_) {
    let stableReads = 0;
    composeSnapshot = () => {
        stableReads += 1;
        return {attachment_paths: []};
    };
    waitForStableCompose({}, 0);

    let changingReads = 0;
    composeSnapshot = () => {
        changingReads += 1;
        return {attachment_paths: [], generation: changingReads};
    };
    let code = "none";
    try {
        waitForStableCompose({}, 0);
    } catch (error) {
        code = error.bridgeCode;
    }
    return JSON.stringify({stable_reads: stableReads, changing_reads: changingReads, code: code});
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"stable_reads":2,"changing_reads":5,"code":"compose_not_ready"}` {
		t.Fatalf("compose wait result = %q", output)
	}
}

func TestBridgeTreatsMissingAttachmentCollectionAsEmpty(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    const outgoing = {content: {attachments: () => null}};
    return JSON.stringify({paths: composeAttachmentPaths(outgoing)});
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"paths":[]}` {
		t.Fatalf("attachment paths = %q", output)
	}
}

func TestBridgeComposeWaitReportsLastSnapshotFailure(t *testing.T) {
	testScript := bridgeScript + `
function sleepForCompose(_) {}
function run(_) {
    composeSnapshot = () => { throw new Error("attachment collection unavailable"); };
    try {
        waitForStableCompose({}, 0);
        return JSON.stringify({code: "none", message: ""});
    } catch (error) {
        return JSON.stringify({code: error.bridgeCode, message: error.message});
    }
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	const want = `{"code":"compose_not_ready","message":"Mail.app did not stabilize the native compose object before the send deadline: attachment collection unavailable"}`
	if strings.TrimSpace(string(output)) != want {
		t.Fatalf("compose snapshot failure = %q, want %s", output, want)
	}
}

func TestBridgeRejectsUnverifiedComposeCleanup(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    let closeCalls = 0;
    const outgoing = {
        close: () => { closeCalls += 1; },
        exists: () => true
    };
    outgoingForDraft = () => outgoing;
    waitForStableCompose = () => { throw new Error("prepare failed"); };
    try {
        sendDraft({outgoingMessages: () => []}, {draft: {
            kind: "new", from: "sender@example.com"
        }});
        return JSON.stringify({code: "none", close_calls: closeCalls});
    } catch (error) {
        return JSON.stringify({code: error.bridgeCode, close_calls: closeCalls});
    }
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"code":"compose_cleanup_failed","close_calls":1}` {
		t.Fatalf("compose cleanup result = %q", output)
	}
}

func TestBridgeDerivedComposeDoesNotFetchFullRFCSource(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    let sourceReads = 0;
    const source = {
        source: () => { sourceReads += 1; return "raw"; },
        reply: () => ({kind: "reply"})
    };
    resolveDraftSource = () => source;
    const outgoing = outgoingForDraft({}, {kind: "reply", source: {}}, {});
    return JSON.stringify({kind: outgoing.kind, source_reads: sourceReads});
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"kind":"reply","source_reads":0}` {
		t.Fatalf("derived compose result = %q", output)
	}
}

func TestBridgeExplicitlyWritesNewDraftBodyBeforeValidation(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    const recipients = [];
    const outgoing = {
        content: "constructor body was lost",
        sender: "", subject: "",
        toRecipients: () => recipients,
        ccRecipients: () => [],
        bccRecipients: () => []
    };
    outgoing.toRecipients.push = value => recipients.push(value);
    outgoing.ccRecipients.push = () => {};
    outgoing.bccRecipients.push = () => {};
    const mail = {
        ToRecipient: value => value,
        CcRecipient: value => value,
        BccRecipient: value => value
    };
    applyOutgoingFields(mail, outgoing, {
        kind: "new", from: "sender@example.com", subject: "Subject", body: "Reviewed body",
        to: [{address: "to@example.com"}], cc: [], bcc: [], attachments: []
    });
    const finalSnapshot = {
        from: outgoing.sender, to: [{address: "to@example.com"}], cc: [], bcc: [],
        subject: outgoing.subject, body: outgoing.content, attachment_paths: []
    };
    return JSON.stringify({
        body: outgoing.content,
        recipients: recipients.length,
        matches: composeMatchesDraft(finalSnapshot, {
            kind: "new", from: "sender@example.com", subject: "Subject", body: "Reviewed body",
            to: [{address: "to@example.com"}], cc: [], bcc: [], attachments: []
        }, {
            from: "sender@example.com", to: [], cc: [], bcc: [],
            subject: "Subject", body: "", attachment_paths: []
        })
    });
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"body":"Reviewed body","recipients":1,"matches":true}` {
		t.Fatalf("new draft preparation result = %q", output)
	}
}

func TestBridgeNewComposeDefersReviewedBodyUntilNativeStateIsStable(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    let constructorProperties = null;
    const mail = {
        OutgoingMessage: properties => {
            constructorProperties = properties;
            return {};
        },
        outgoingMessages: []
    };
    outgoingForDraft(mail, {
        kind: "new", from: "sender@example.com", subject: "Subject", body: "Reviewed body"
    }, {});
    return JSON.stringify({has_content: Object.prototype.hasOwnProperty.call(constructorProperties, "content")});
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"has_content":false}` {
		t.Fatalf("new compose constructor result = %q", output)
	}
}

func TestBridgeNewComposePreservesStableMailSignature(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    const recipients = [];
    let content = "Automatic signature";
    const outgoing = {
        sender: "sender@example.com", subject: "Subject",
        toRecipients: () => recipients,
        ccRecipients: () => [],
        bccRecipients: () => []
    };
    Object.defineProperty(outgoing, "content", {
        get: () => () => content,
        set: value => { content = value; }
    });
    outgoing.toRecipients.push = value => recipients.push(value);
    outgoing.ccRecipients.push = () => {};
    outgoing.bccRecipients.push = () => {};
    const mail = {
        ToRecipient: value => value,
        CcRecipient: value => value,
        BccRecipient: value => value
    };
    const draft = {
        kind: "new", from: "sender@example.com", subject: "Subject", body: "Reviewed body",
        to: [{address: "to@example.com"}], cc: [], bcc: [], attachments: []
    };
    const native = {
        from: "sender@example.com", to: [], cc: [], bcc: [],
        subject: "Subject", body: "Automatic signature", attachment_paths: []
    };
    applyOutgoingFields(mail, outgoing, draft);
    const finalSnapshot = {
        from: outgoing.sender, to: [{address: "to@example.com"}], cc: [], bcc: [],
        subject: outgoing.subject, body: content, attachment_paths: []
    };
    return JSON.stringify({body: content, matches: composeMatchesDraft(finalSnapshot, draft, native)});
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"body":"Reviewed body\n\nAutomatic signature","matches":true}` {
		t.Fatalf("new compose signature result = %q", output)
	}
}

func TestBridgeComposeIntegrityRejectsUnobservedBodySuffix(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    const draft = {
        kind: "new", from: "sender@example.com", subject: "Subject", body: "Reviewed body",
        to: [{address: "to@example.com"}], cc: [], bcc: [], attachments: []
    };
    const native = {
        from: "sender@example.com", to: [], cc: [], bcc: [],
        subject: "Subject", body: "", attachment_paths: []
    };
    const snapshot = {
        from: "sender@example.com", to: [{address: "to@example.com"}], cc: [], bcc: [],
        subject: "Subject", body: "Reviewed body\n\nUnobserved suffix", attachment_paths: []
    };
    return JSON.stringify({matches: composeMatchesDraft(snapshot, draft, native)});
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"matches":false}` {
		t.Fatalf("unobserved body suffix result = %q", output)
	}
}

func TestBridgeComposeIntegrityAcceptsReviewedReplyWithNativeQuote(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    const draft = {
        kind: "reply", from: "sender@example.com", subject: "Re: Subject",
        body: "Reviewed response", to: [], cc: [], bcc: [], attachments: []
    };
    const native = {
        from: "sender@example.com", to: [{address: "person@example.com"}], cc: [], bcc: [],
        subject: "Re: Subject", body: "On Monday, Person wrote:\n> Original message", attachment_paths: []
    };
    const snapshot = {
        from: "Sender <sender@example.com>", to: [{address: "PERSON@example.com"}], cc: [], bcc: [],
        subject: "Re: Subject",
        body: "Reviewed response\n\nOn Monday, Person wrote:\n> Original message",
        attachment_paths: []
    };
    return JSON.stringify({matches: composeMatchesDraft(snapshot, draft, native)});
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"matches":true}` {
		t.Fatalf("reply compose integrity result = %q", output)
	}
}

func TestBridgeSaveClosesComposeWithStandardSaveOption(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    let closeOption = "";
    let saveCalls = 0;
    const outgoing = {
        close: options => { closeOption = options.saving; },
        save: () => { saveCalls += 1; }
    };
    const snapshot = {
        from: "sender@example.com", to: [{address: "to@example.com"}], cc: [], bcc: [],
        subject: "Subject", body: "Body", attachment_paths: []
    };
    outgoingForDraft = () => outgoing;
    waitForStableCompose = () => snapshot;
    applyOutgoingFields = () => {};
    waitForPreparedCompose = () => snapshot;
    const result = saveDraft({outgoingMessages: () => []}, {draft: {
        kind: "new", from: "sender@example.com", to: [{address: "to@example.com"}],
        cc: [], bcc: [], subject: "Subject", body: "Body", attachments: []
    }});
    return JSON.stringify({accepted: result.accepted, close_option: closeOption, save_calls: saveCalls});
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"accepted":true,"close_option":"yes","save_calls":0}` {
		t.Fatalf("save lifecycle result = %q", output)
	}
}

func TestBridgeRejectsSendWhileAnotherComposeExists(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    let created = false;
    outgoingForDraft = () => { created = true; return {}; };
    try {
        sendDraft({outgoingMessages: () => [{visible: () => true}]}, {draft: {
            kind: "new", from: "sender@example.com"
        }});
        return JSON.stringify({code: "none", created: created});
    } catch (error) {
        return JSON.stringify({code: error.bridgeCode, created: created});
    }
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"code":"compose_busy","created":false}` {
		t.Fatalf("compose-busy result = %q", output)
	}
}

func TestBridgeIgnoresHiddenOutgoingBackend(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    try {
        ensureComposeAvailable({outgoingMessages: () => [{visible: () => false}]});
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
	if strings.TrimSpace(string(output)) != `{"code":"none"}` {
		t.Fatalf("hidden compose availability result = %q", output)
	}
}

func TestBridgeComposeIntegrityRejectsMissingBodyAndDuplicateRecipient(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    const draft = {body: "Reviewed body", to: [{address: "person@example.com"}], cc: [], bcc: [], attachments: []};
    const signatureOnly = {
        from: "sender@example.com", to: [{address: "person@example.com"}], cc: [], bcc: [],
        subject: "Re: Subject", body: "Automatic signature", attachment_paths: []
    };
    const duplicated = {
        from: "sender@example.com", to: [{address: "person@example.com"}, {address: "PERSON@example.com"}],
        cc: [], bcc: [], subject: "Re: Subject", body: "Reviewed body\n\nQuote", attachment_paths: []
    };
    const quoteLost = {
        from: "sender@example.com", to: [{address: "person@example.com"}], cc: [], bcc: [],
        subject: "Re: Subject", body: "Reviewed body", attachment_paths: []
    };
    return JSON.stringify({
        missing_body: composeMatchesDraft(signatureOnly, draft, {body: "", attachment_paths: []}),
        duplicate_recipient: composeMatchesDraft(duplicated, draft, {body: "", attachment_paths: []}),
        quote_lost: composeMatchesDraft(quoteLost, draft, {body: "Original quote", attachment_paths: []})
    });
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"missing_body":false,"duplicate_recipient":false,"quote_lost":false}` {
		t.Fatalf("integrity result = %q", output)
	}
}

func TestBridgeComposeIntegrityRejectsUnexpectedRecipientAndAttachment(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    const draft = {
        kind: "reply", body: "Reviewed body", to: [{address: "person@example.com"}],
        cc: [], bcc: [], attachments: []
    };
    const native = {
        from: "sender@example.com", to: [], cc: [], bcc: [], subject: "Re: Subject",
        body: "Original quote", attachment_paths: []
    };
    const unexpectedRecipient = {
        from: "sender@example.com",
        to: [{address: "person@example.com"}, {address: "other@example.com"}], cc: [], bcc: [],
        subject: "Re: Subject", body: "Reviewed body\n\nOriginal quote", attachment_paths: []
    };
    const unexpectedAttachment = {
        from: "sender@example.com", to: [{address: "person@example.com"}], cc: [], bcc: [],
        subject: "Re: Subject", body: "Reviewed body\n\nOriginal quote", attachment_paths: ["/tmp/extra.bin"]
    };
    return JSON.stringify({
        recipient: composeMatchesDraft(unexpectedRecipient, draft, native),
        attachment: composeMatchesDraft(unexpectedAttachment, draft, native)
    });
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"recipient":false,"attachment":false}` {
		t.Fatalf("integrity result = %q", output)
	}
}

func TestBridgeComposeIntegrityAcceptsEquivalentSenderFormatting(t *testing.T) {
	testScript := bridgeScript + `
function run(_) {
    const draft = {
        kind: "new", from: "sender@example.com", to: [{address: "person@example.com"}],
        cc: [], bcc: [], subject: "Cafe\u0301", body: "Body", attachments: []
    };
    const native = {
        from: "Sender Name <sender@example.com>", to: [], cc: [], bcc: [],
        subject: "Caf\u00e9", body: "", attachment_paths: []
    };
    const snapshot = {
        from: "Sender Name <SENDER@example.com>", to: [{address: "person@example.com"}],
        cc: [], bcc: [], subject: "Caf\u00e9", body: "Body", attachment_paths: []
    };
    return JSON.stringify({matches: composeMatchesDraft(snapshot, draft, native)});
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"matches":true}` {
		t.Fatalf("integrity result = %q", output)
	}
}

func TestBridgeClosesComposeWhenMailRejectsSend(t *testing.T) {
	testScript := bridgeScript + `
let closed = false;
const snapshot = {
    from: "sender@example.com", to: [{address: "to@example.com"}], cc: [], bcc: [],
    subject: "Subject", body: "Body", attachment_paths: []
};
const native = {
    from: "sender@example.com", to: [], cc: [], bcc: [],
    subject: "Subject", body: "Body", attachment_paths: []
};
let composeExists = true;
function outgoingForDraft() { return {
    send: () => false,
    close: () => { closed = true; composeExists = false; },
    exists: () => composeExists
}; }
function applyOutgoingFields() {}
function waitForStableCompose() { return native; }
function waitForPreparedCompose() { return snapshot; }
function composeSnapshot() { return snapshot; }
function run(_) {
    const result = sendDraft({outgoingMessages: () => []}, {draft: {
        kind: "new", from: "sender@example.com", to: [{address: "to@example.com"}],
        subject: "Subject", body: "Body", attachments: []
    }});
    return JSON.stringify({accepted: result.accepted, closed: closed});
}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
	if err != nil {
		t.Fatalf("osaScriptRunner.Run() error = %v", err)
	}
	if strings.TrimSpace(string(output)) != `{"accepted":false,"closed":true}` {
		t.Fatalf("send rejection result = %q", output)
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
	output, _, err := (osaScriptRunner{}).Run(ctx, testScript, `{}`)
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

func TestProductionBridgeContainsNoTightComposePolling(t *testing.T) {
	for _, fragment := range []string{"sleepForCompose(0.2)", "Date.now() - started"} {
		if strings.Contains(bridgeScript, fragment) {
			t.Fatalf("bridge contains tight compose polling %q", fragment)
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
		`<responds-to command="save">`, `<responds-to command="close">`,
		`<command name="synchronize"`, `<class name="outgoing message"`,
		`<property name="visible"`, `<property name="read status"`, `<property name="flagged status"`,
		`<property name="junk mail status"`, `<class name="mail attachment"`,
	}
	for _, fragment := range required {
		if !strings.Contains(definition, fragment) {
			t.Fatalf("Mail.sdef missing %q", fragment)
		}
	}
}
