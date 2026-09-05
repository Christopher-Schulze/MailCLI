function bridgeError(code, message) {
    const error = new Error(message);
    error.bridgeCode = code;
    throw error;
}

let bridgeCancellationPath = "";

function cancellationRequested() {
    return bridgeCancellationPath && Boolean(
        $.NSFileManager.defaultManager.fileExistsAtPath(bridgeCancellationPath)
    );
}

function abortIfCancelled() {
    if (cancellationRequested()) {
        bridgeError("operation_cancelled", "Mail.app operation was cancelled before mutation");
    }
}

function verifyMailProcess(processID) {
    ObjC.import("AppKit");
    const running = $.NSRunningApplication.runningApplicationWithProcessIdentifier(processID);
    const bundleIdentifier = safe(() => ObjC.unwrap(running.bundleIdentifier), "");
    if (bundleIdentifier !== "com.apple.mail") {
        bridgeError("mail_process_changed", "Mail.app process identity changed before the Apple Events operation");
    }
}

function readBridgeRequest(path) {
    ObjC.import("Foundation");
    const data = $.NSData.dataWithContentsOfFile(path);
    if (!data) {
        bridgeError("invalid_request", "MailCLI bridge request could not be read");
    }
    const value = $.NSString.alloc.initWithDataEncoding(data, $.NSUTF8StringEncoding);
    if (!value) {
        bridgeError("invalid_request", "MailCLI bridge request is not valid UTF-8");
    }
    return JSON.parse(ObjC.unwrap(value));
}

function safe(read, fallback) {
    try {
        const value = read();
        return value === undefined ? fallback : value;
    } catch (_) {
        return fallback;
    }
}

function safeDate(read) {
    const value = safe(read, null);
    if (value === null || Number.isNaN(value.getTime())) {
        return null;
    }
    return value.toISOString();
}

function enabledAccounts(mail) {
    return mail.accounts()
        .filter(account => safe(() => account.enabled(), false))
        .map(account => ({
            object: account,
            id: String(account.id()),
            name: safe(() => account.name(), "")
        }))
        .sort((left, right) => left.id.localeCompare(right.id));
}

function createResolutionContext() {
    return {accounts: null, mailboxes: Object.create(null)};
}

function resolvedAccounts(mail, resolution) {
    if (!resolution) {
        return enabledAccounts(mail);
    }
    if (resolution.accounts === null) {
        resolution.accounts = enabledAccounts(mail);
    }
    return resolution.accounts;
}

function resolveAccount(mail, accountID, resolution) {
    const matches = resolvedAccounts(mail, resolution).filter(account => account.id === accountID);
    if (matches.length === 0) {
        bridgeError("not_found", "enabled account not found");
    }
    if (matches.length > 1) {
        bridgeError("ambiguous_reference", "account reference is ambiguous");
    }
    return matches[0];
}

function resolveMailbox(account, path, resolution) {
    if (!Array.isArray(path) || path.length === 0) {
        bridgeError("invalid_request", "mailbox path is required");
    }

    const cacheKey = account.id + "\u0000" + JSON.stringify(path);
    if (resolution && Object.prototype.hasOwnProperty.call(resolution.mailboxes, cacheKey)) {
        return resolution.mailboxes[cacheKey];
    }

    let mailboxes = account.object.mailboxes();
    let mailbox = null;
    for (let index = 0; index < path.length; index += 1) {
        const name = path[index];
        const matches = mailboxes.filter(candidate => safe(() => candidate.name(), "") === name);
        if (matches.length === 0) {
            bridgeError("not_found", "mailbox not found");
        }
        if (matches.length > 1) {
            bridgeError("ambiguous_reference", "mailbox path is ambiguous");
        }
        mailbox = matches[0];
        if (index + 1 < path.length) {
            mailboxes = mailbox.mailboxes();
        }
    }
    if (resolution) {
        resolution.mailboxes[cacheKey] = mailbox;
    }
    return mailbox;
}

function messageSummary(message, accountID, mailboxPath) {
    return {
        account_id: accountID,
        mailbox_path: mailboxPath,
        library_id: String(message.id()),
        message_id: safe(() => message.messageId(), ""),
        subject: safe(() => message.subject(), ""),
        sender: safe(() => message.sender(), ""),
        date_received: safeDate(() => message.dateReceived()),
        date_sent: safeDate(() => message.dateSent()),
        read: safe(() => message.readStatus(), false),
        flagged: safe(() => message.flaggedStatus(), false),
        junk: safe(() => message.junkMailStatus(), false),
        deleted: safe(() => message.deletedStatus(), false),
        size: safe(() => message.messageSize(), 0),
        attachment_count: safe(() => message.mailAttachments().length, 0)
    };
}

function syncMail(mail, request, resolution) {
    if (request.account_id) {
        const account = resolveAccount(mail, request.account_id, resolution);
        mail.synchronize({with: account.object});
        return {};
    }
    mail.checkForNewMail();
    return {};
}

function pageStart(messages, offset, expectedPreviousID) {
    if (!offset) {
        return 0;
    }
    if (offset < 1 || offset > messages.length) {
        bridgeError("stale_cursor", "cursor offset is no longer present");
    }
    const actualPreviousID = String(messages[offset - 1].id());
    if (actualPreviousID !== expectedPreviousID) {
        bridgeError("stale_cursor", "mailbox changed before the cursor");
    }
    return offset;
}

function listMessages(mail, request, resolution) {
    const limit = Number(request.limit);
    if (!Number.isInteger(limit) || limit < 1 || limit > 25) {
        bridgeError("invalid_request", "message page limit must be between 1 and 25");
    }
    const account = resolveAccount(mail, request.account_id, resolution);
    const mailbox = resolveMailbox(account, request.mailbox_path, resolution);
    const messages = mailbox.messages;
    const start = pageStart(messages, request.offset || 0, request.expected_previous_id || "");
    const end = Math.min(start + limit, messages.length);
    const output = [];
    for (let index = start; index < end; index += 1) {
        output.push(messageSummary(messages[index], account.id, request.mailbox_path));
    }
    return {
        messages: output,
        next_offset: end < messages.length && output.length > 0 ? end : null,
        next_previous_id: end < messages.length && output.length > 0
            ? output[output.length - 1].library_id : null
    };
}

function dispatch(mail, request) {
    const resolution = createResolutionContext();
    switch (request.operation) {
    case "accounts.list":
        return {
            accounts: resolvedAccounts(mail, resolution).map(account => ({
                id: account.id,
                name: account.name,
                email_addresses: safe(() => account.object.emailAddresses(), [])
            }))
        };
    case "messages.list":
        return listMessages(mail, request, resolution);
    case "mail.sync":
        return syncMail(mail, request, resolution);
    default:
        bridgeError("invalid_request", "unknown MailCLI bridge operation");
    }
}

function bridgeFailure(error) {
    const nativeDetail = String(error.message || error);
    const automationDenied = Number(error.number) === -1743 || nativeDetail.includes("-1743") ||
        nativeDetail.toLowerCase().includes("not authorized to send apple events") ||
        nativeDetail.toLowerCase().includes("not authorised to send apple events");
    let errorCode = "mail_error";
    let errorMessage = "Mail.app Apple Events operation failed";
    if (error.bridgeCode) {
        errorCode = error.bridgeCode;
        errorMessage = error.message;
    } else if (automationDenied) {
        errorCode = "mail_automation_denied";
        errorMessage = "Automation access to Mail is denied; allow the calling host in System Settings > Privacy & Security > Automation";
    }
    return {
        ok: false,
        recovery_required: Boolean(error.recoveryRequired),
        error: {code: errorCode, message: errorMessage}
    };
}
function run(argv) {
    try {
        if (argv.length !== 2) {
            bridgeError("invalid_request", "request and cancellation paths are required");
        }
        bridgeCancellationPath = argv[1];
        const request = readBridgeRequest(argv[0]);
        abortIfCancelled();
        if (!Number.isInteger(request.mail_pid) || request.mail_pid <= 0) {
            bridgeError("mail_not_running", "Mail.app target process is unavailable");
        }
        verifyMailProcess(request.mail_pid);
        const mail = Application(request.mail_pid);
        verifyMailProcess(request.mail_pid);
        const response = dispatch(mail, request);
        response.ok = true;
        response.error = null;
        return JSON.stringify(response);
    } catch (error) {
        return JSON.stringify(bridgeFailure(error));
    }
}
