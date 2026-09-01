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

function collectMailboxes(account, mailboxes, parentPath, output) {
    for (const mailbox of mailboxes) {
        const name = safe(() => mailbox.name(), "");
        const path = parentPath.concat([name]);
        output.push({
            object: mailbox,
            accountID: account.id,
            name: name,
            path: path,
            unreadCount: safe(() => mailbox.unreadCount(), 0)
        });
        collectMailboxes(account, mailbox.mailboxes(), path, output);
    }
}

function mailboxRecords(mail, request, resolution) {
    const accounts = request.account_id
        ? [resolveAccount(mail, request.account_id, resolution)]
        : resolvedAccounts(mail, resolution);
    const records = [];
    for (const account of accounts) {
        collectMailboxes(account, account.object.mailboxes(), [], records);
    }
    records.sort((left, right) => {
        const leftKey = left.accountID + "\u0000" + left.path.join("\u0000");
        const rightKey = right.accountID + "\u0000" + right.path.join("\u0000");
        return leftKey.localeCompare(rightKey);
    });
    return records;
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

function findMessage(mailbox, libraryID) {
    const numericID = Number(libraryID);
    if (!Number.isSafeInteger(numericID)) {
        bridgeError("invalid_request", "message library identifier is invalid");
    }
    const message = mailbox.messages.byId(numericID);
    if (!message.exists()) {
        bridgeError("not_found", "message not found");
    }
    return message;
}

function findDraftMessage(mailbox, libraryID, expectedMessageID, expectedSubject) {
    const message = findMessage(mailbox, libraryID);
    validateMessageIdentity(message, expectedMessageID, expectedSubject);
    return message;
}

function validateMessageIdentity(message, expectedMessageID, expectedSubject) {
    if (expectedMessageID && safe(() => message.messageId(), "") !== expectedMessageID) {
        bridgeError("stale_reference", "message identity changed");
    }
    if (expectedSubject && safe(() => message.subject(), "") !== expectedSubject) {
        bridgeError("stale_reference", "message identity changed");
    }
}

function recipients(message, property) {
    return safe(() => message[property](), []).map(recipient => ({
        name: safe(() => recipient.name(), ""),
        address: safe(() => recipient.address(), "")
    }));
}

function attachments(message) {
    return safe(() => message.mailAttachments(), []).map(attachment => ({
        id: safe(() => String(attachment.id()), ""),
        name: safe(() => attachment.name(), ""),
        mime_type: safe(() => attachment.mimeType(), null),
        size: safe(() => attachment.fileSize(), 0),
        downloaded: safe(() => attachment.downloaded(), false)
    }));
}

function saveAttachment(mail, request, resolution) {
    const account = resolveAccount(mail, request.account_id, resolution);
    const mailbox = resolveMailbox(account, request.mailbox_path, resolution);
    const message = findMessage(mailbox, request.message_id);
    validateMessageIdentity(message, request.expected_message_id, request.expected_subject);
    const matches = message.mailAttachments().filter(attachment =>
        safe(() => String(attachment.id()), "") === request.attachment_id
    );
    if (matches.length === 0) {
        bridgeError("not_found", "attachment not found");
    }
    if (matches.length > 1) {
        bridgeError("ambiguous_reference", "attachment reference is ambiguous");
    }
    matches[0].save({in: Path(request.output_path)});
    return {};
}

function addRecipients(mail, outgoing, property, constructor, values) {
    const existing = Object.create(null);
    for (const recipient of outgoing[property]()) {
        const address = String(recipient.address()).trim().toLowerCase();
        if (address) {
            existing[address] = true;
        }
    }
    for (const value of values || []) {
        const address = String(value.address || "").trim().toLowerCase();
        if (!address || existing[address]) {
            continue;
        }
        outgoing[property].push(mail[constructor]({
            address: value.address,
            name: value.name || ""
        }));
        existing[address] = true;
    }
}

function resolveDraftSource(mail, source, resolution) {
    if (!source) {
        bridgeError("invalid_request", "source message is required");
    }
    const account = resolveAccount(mail, source.account_id, resolution);
    const mailbox = resolveMailbox(account, source.mailbox_path, resolution);
    const message = findMessage(mailbox, source.library_id);
    validateMessageIdentity(message, source.expected_message_id, source.expected_subject);
    return message;
}

function outgoingForDraft(mail, draft, resolution) {
    if (draft.kind === "new") {
        const properties = {visible: false};
        if (draft.from) {
            properties.sender = draft.from;
        }
        if (draft.subject) {
            properties.subject = draft.subject;
        }
        const outgoing = mail.OutgoingMessage(properties);
        mail.outgoingMessages.push(outgoing);
        return outgoing;
    }
    const source = resolveDraftSource(mail, draft.source, resolution);
    if (draft.kind === "reply") {
        return source.reply({openingWindow: false, replyToAll: Boolean(draft.reply_all)});
    }
    if (draft.kind === "forward") {
        return source.forward({openingWindow: false});
    }
    bridgeError("invalid_request", "unsupported draft kind");
}

function composeRecipients(outgoing, property) {
    return outgoing[property]().map(recipient => ({
        name: safe(() => recipient.name(), ""),
        address: String(recipient.address())
    }));
}

function composeAttachmentPaths(outgoing) {
    const attachments = outgoing.content.attachments();
    if (attachments === null) {
        return [];
    }
    return attachments.map(attachment => String(attachment.fileName()));
}

function composeSnapshot(outgoing) {
    return {
        from: String(outgoing.sender()),
        to: composeRecipients(outgoing, "toRecipients"),
        cc: composeRecipients(outgoing, "ccRecipients"),
        bcc: composeRecipients(outgoing, "bccRecipients"),
        subject: String(outgoing.subject()),
        body: String(outgoing.content()),
        attachment_paths: composeAttachmentPaths(outgoing)
    };
}

function stableComposeFingerprint(snapshot) {
    return JSON.stringify(snapshot);
}

function sleepForCompose(seconds) {
    $.NSThread.sleepForTimeInterval(seconds);
}

function waitForComposeSnapshot(outgoing, accepts, initialDelay, retryDelay, maximumSnapshots, code, message) {
    abortIfCancelled();
    sleepForCompose(initialDelay);
    let previousAccepted = "";
    let successfulSnapshotReads = 0;
    let lastSnapshotError = null;
    for (let attempt = 0; attempt < maximumSnapshots; attempt += 1) {
        abortIfCancelled();
        let snapshot;
        try {
            snapshot = composeSnapshot(outgoing);
            successfulSnapshotReads += 1;
        } catch (error) {
            lastSnapshotError = error;
            snapshot = null;
        }
        if (snapshot !== null && accepts(snapshot)) {
            const fingerprint = stableComposeFingerprint(snapshot);
            if (fingerprint === previousAccepted) {
                return snapshot;
            }
            previousAccepted = fingerprint;
        } else {
            previousAccepted = "";
        }
        if (attempt + 1 < maximumSnapshots) {
            sleepForCompose(retryDelay);
        }
    }
    if (successfulSnapshotReads === 0 && lastSnapshotError !== null) {
        const detail = String(lastSnapshotError.message || lastSnapshotError);
        bridgeError(code, message + ": " + detail);
    }
    bridgeError(code, message);
}

function waitForStableCompose(outgoing, requiredAttachmentCount) {
    return waitForComposeSnapshot(
        outgoing,
        snapshot => snapshot.attachment_paths.length >= (requiredAttachmentCount || 0),
        4, 2, 5,
        "compose_not_ready",
        "Mail.app did not stabilize the native compose object before the send deadline"
    );
}

function canonicalComposeText(value) {
    return String(value).replace(/\r\n/g, "\n").replace(/\r/g, "\n").normalize("NFC");
}

function recipientAddresses(values) {
    return values.map(value => String(value.address).trim().toLowerCase());
}

function hasDuplicateAddresses(values) {
    const seen = Object.create(null);
    for (const address of recipientAddresses(values)) {
        if (!address || seen[address]) {
            return true;
        }
        seen[address] = true;
    }
    return false;
}

function hasDuplicateRecipients(snapshot) {
    return hasDuplicateAddresses(snapshot.to.concat(snapshot.cc, snapshot.bcc));
}

function expectedRecipientAddresses(nativeValues, reviewedValues) {
    const addresses = [];
    const seen = Object.create(null);
    for (const value of (nativeValues || []).concat(reviewedValues || [])) {
        const address = String(value.address || "").trim().toLowerCase();
        if (address && !seen[address]) {
            addresses.push(address);
            seen[address] = true;
        }
    }
    return addresses.sort();
}

function recipientsMatchMaterialization(actual, nativeValues, reviewedValues) {
    const actualAddresses = recipientAddresses(actual).sort();
    const expectedAddresses = expectedRecipientAddresses(nativeValues, reviewedValues);
    return JSON.stringify(actualAddresses) === JSON.stringify(expectedAddresses);
}

function normalizedMailbox(value) {
    const text = String(value || "").trim();
    const bracketed = text.match(/<([^<>]+)>\s*$/);
    return String(bracketed ? bracketed[1] : text).trim().toLowerCase();
}

function basename(path) {
    const components = String(path).split("/");
    return components[components.length - 1];
}

function containsReviewedAttachments(paths, expected) {
    const counts = Object.create(null);
    for (const path of paths) {
        const name = basename(path).normalize("NFC");
        counts[name] = (counts[name] || 0) + 1;
    }
    for (const attachment of expected || []) {
        const name = basename(attachment.path).normalize("NFC");
        if (!counts[name]) {
            return false;
        }
        counts[name] -= 1;
    }
    return true;
}

function composeMatchesDraft(snapshot, draft, native) {
    const body = canonicalComposeText(snapshot.body);
    const expectedBody = canonicalComposeText(draft.body);
    const nativeBody = canonicalComposeText(native.body || "");
    let materializedBody = expectedBody || nativeBody;
    if (expectedBody && nativeBody) {
        materializedBody = expectedBody + "\n\n" + nativeBody;
    }
    if (body !== materializedBody) {
        return false;
    }
    if (normalizedMailbox(snapshot.from) !== normalizedMailbox(draft.from || native.from)) {
        return false;
    }
    if (String(snapshot.subject).normalize("NFC") !== String(draft.subject || native.subject || "").normalize("NFC")) {
        return false;
    }
    if (hasDuplicateRecipients(snapshot)) {
        return false;
    }
    if (!recipientsMatchMaterialization(snapshot.to, native.to, draft.to) ||
        !recipientsMatchMaterialization(snapshot.cc, native.cc, draft.cc) ||
        !recipientsMatchMaterialization(snapshot.bcc, native.bcc, draft.bcc)) {
        return false;
    }
    if (snapshot.to.length + snapshot.cc.length + snapshot.bcc.length === 0) {
        return false;
    }
    if (snapshot.attachment_paths.length !== (native.attachment_paths || []).length + (draft.attachments || []).length) {
        return false;
    }
    return containsReviewedAttachments(snapshot.attachment_paths, (native.attachment_paths || []).map(path => ({path: path}))) &&
        containsReviewedAttachments(snapshot.attachment_paths, draft.attachments);
}

function waitForPreparedCompose(outgoing, draft, native) {
    return waitForComposeSnapshot(
        outgoing,
        snapshot => composeMatchesDraft(snapshot, draft, native),
        1.5, 1.5, 5,
        "compose_integrity_failed",
        "Mail.app did not retain the reviewed body, recipients, and attachments; send was blocked"
    );
}

function applyOutgoingFields(mail, outgoing, draft) {
    if (draft.from) {
        outgoing.sender = draft.from;
    }
    if (draft.subject) {
        outgoing.subject = draft.subject;
    }
    const originalContent = String(safe(() => outgoing.content(), ""));
    outgoing.content = draft.body && originalContent
        ? draft.body + "\n\n" + originalContent
        : draft.body || originalContent;
    addRecipients(mail, outgoing, "toRecipients", "ToRecipient", draft.to);
    addRecipients(mail, outgoing, "ccRecipients", "CcRecipient", draft.cc);
    addRecipients(mail, outgoing, "bccRecipients", "BccRecipient", draft.bcc);
    for (const attachment of draft.attachments || []) {
        outgoing.content.attachments.push(mail.Attachment({fileName: Path(attachment.path)}));
    }
}

function sendMaterialization(snapshot) {
    return {
        from: snapshot.from,
        to: snapshot.to,
        cc: snapshot.cc,
        bcc: snapshot.bcc,
        subject: snapshot.subject,
        attachment_count: snapshot.attachment_paths.length
    };
}

function ensureComposeAvailable(mail) {
    const visibleComposeExists = mail.outgoingMessages().some(outgoing =>
        safe(() => Boolean(outgoing.visible()), true)
    );
    if (visibleComposeExists) {
        bridgeError("compose_busy", "Mail.app already has a visible compose window; close or save it first");
    }
}

function closeOwnedCompose(outgoing) {
    try {
        outgoing.close({saving: "no"});
    } catch (_) {
        return false;
    }
    for (let attempt = 0; attempt < 5; attempt += 1) {
        let exists;
        try {
            exists = Boolean(outgoing.exists());
        } catch (_) {
            exists = null;
        }
        if (exists === false) {
            return true;
        }
        if (attempt + 1 < 5) {
            sleepForCompose(0.5);
        }
    }
    return false;
}

function sendDraft(mail, request, resolution) {
    if (!request.draft) {
        bridgeError("invalid_request", "draft is required");
    }
    if (request.draft.kind === "new" && !request.draft.from) {
        bridgeError("invalid_request", "sending a new draft requires an explicit sender");
    }
    abortIfCancelled();
    ensureComposeAvailable(mail);
    const outgoing = outgoingForDraft(mail, request.draft, resolution);
    let sendAttempted = false;
    try {
        const native = waitForStableCompose(outgoing, request.draft.expected_native_attachment_count);
        applyOutgoingFields(mail, outgoing, request.draft);
        const finalSnapshot = waitForPreparedCompose(outgoing, request.draft, native);
        abortIfCancelled();
        sendAttempted = true;
        const accepted = Boolean(outgoing.send());
        if (!accepted) {
            if (!closeOwnedCompose(outgoing)) {
                bridgeError("compose_cleanup_failed", "Mail.app rejected send and the unsent compose object could not be closed");
            }
        }
        return {
            accepted: accepted,
            send_attempted: true,
            materialized: sendMaterialization(finalSnapshot)
        };
    } catch (error) {
        error.sendAttempted = sendAttempted;
        if (!sendAttempted) {
            if (!closeOwnedCompose(outgoing)) {
                const preparationMessage = String(error.message || error);
                error.bridgeCode = "compose_cleanup_failed";
                error.message = "Mail.app rejected send preparation and the unsent compose object could not be closed: " + preparationMessage;
            }
        }
        throw error;
    }
}

function saveDraft(mail, request, resolution) {
    if (!request.draft || !request.draft.from) {
        bridgeError("invalid_request", "draft with an explicit sender is required");
    }
    abortIfCancelled();
    ensureComposeAvailable(mail);
    const outgoing = outgoingForDraft(mail, request.draft, resolution);
    let saveAttempted = false;
    try {
        const native = waitForStableCompose(outgoing, request.draft.expected_native_attachment_count);
        applyOutgoingFields(mail, outgoing, request.draft);
        const finalSnapshot = waitForPreparedCompose(outgoing, request.draft, native);
        abortIfCancelled();
        saveAttempted = true;
        outgoing.close({saving: "yes"});
        return {accepted: true, materialized: sendMaterialization(finalSnapshot)};
    } catch (error) {
        if (!saveAttempted) {
            if (!closeOwnedCompose(outgoing)) {
                const preparationMessage = String(error.message || error);
                error.bridgeCode = "compose_cleanup_failed";
                error.message = "Mail.app rejected draft preparation and the unsaved compose object could not be closed: " + preparationMessage;
            }
        }
        throw error;
    }
}

function markMessage(mail, request, resolution) {
    const account = resolveAccount(mail, request.account_id, resolution);
    const mailbox = resolveMailbox(account, request.mailbox_path, resolution);
    const message = findMessage(mailbox, request.message_id);
    validateMessageIdentity(message, request.expected_message_id, request.expected_subject);
    if (request.read !== undefined) {
        message.readStatus = Boolean(request.read);
    }
    if (request.flagged !== undefined) {
        message.flaggedStatus = Boolean(request.flagged);
    }
    if (request.junk !== undefined) {
        message.junkMailStatus = Boolean(request.junk);
    }
    return {accepted: true};
}

function transferMessage(mail, request, resolution) {
    const account = resolveAccount(mail, request.account_id, resolution);
    const mailbox = resolveMailbox(account, request.mailbox_path, resolution);
    const message = findMessage(mailbox, request.message_id);
    validateMessageIdentity(message, request.expected_message_id, request.expected_subject);
    const destinationAccount = resolveAccount(mail, request.destination_account_id, resolution);
    const destination = resolveMailbox(destinationAccount, request.destination_mailbox_path, resolution);
    request.copy
        ? mail.duplicate(message, {to: destination})
        : mail.move(message, {to: destination});
    return {accepted: true};
}

function deleteMessage(mail, request, resolution) {
    const account = resolveAccount(mail, request.account_id, resolution);
    const mailbox = resolveMailbox(account, request.mailbox_path, resolution);
    const message = findMessage(mailbox, request.message_id);
    validateMessageIdentity(message, request.expected_message_id, request.expected_subject);
    mail.delete(message);
    return {accepted: true};
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

function messageDetail(message, accountID, mailboxPath) {
    const summary = messageSummary(message, accountID, mailboxPath);
    summary.reply_to = safe(() => message.replyTo(), "");
    summary.to = recipients(message, "toRecipients");
    summary.cc = recipients(message, "ccRecipients");
    summary.bcc = recipients(message, "bccRecipients");
    summary.headers = safe(() => message.allHeaders(), "");
    summary.content = safe(() => message.content(), "");
    summary.attachments = attachments(message);
    return summary;
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
    case "mailboxes.list":
        return {
            mailboxes: mailboxRecords(mail, request, resolution).map(record => ({
                account_id: record.accountID,
                name: record.name,
                path: record.path,
                unread_count: record.unreadCount
            }))
        };
    case "messages.list":
        return listMessages(mail, request, resolution);
    case "messages.get": {
        const account = resolveAccount(mail, request.account_id, resolution);
        const mailbox = resolveMailbox(account, request.mailbox_path, resolution);
        const message = findMessage(mailbox, request.message_id);
        validateMessageIdentity(message, request.expected_message_id, request.expected_subject);
        return {message: messageDetail(message, account.id, request.mailbox_path)};
    }
    case "drafts.open": {
        const account = resolveAccount(mail, request.account_id, resolution);
        const mailbox = resolveMailbox(account, request.mailbox_path, resolution);
        const message = findDraftMessage(
            mailbox, request.message_id, request.expected_message_id, request.expected_subject
        );
        return {message: messageDetail(message, account.id, request.mailbox_path)};
    }
    case "messages.raw": {
        const account = resolveAccount(mail, request.account_id, resolution);
        const mailbox = resolveMailbox(account, request.mailbox_path, resolution);
        const message = findMessage(mailbox, request.message_id);
        validateMessageIdentity(message, request.expected_message_id, request.expected_subject);
        return {raw_source: message.source()};
    }
    case "attachments.save":
        return saveAttachment(mail, request, resolution);
    case "drafts.send":
        return sendDraft(mail, request, resolution);
    case "drafts.save":
        return saveDraft(mail, request, resolution);
    case "messages.mark":
        return markMessage(mail, request, resolution);
    case "messages.transfer":
        return transferMessage(mail, request, resolution);
    case "messages.delete":
        return deleteMessage(mail, request, resolution);
    case "mail.sync":
        return syncMail(mail, request, resolution);
    default:
        bridgeError("invalid_request", "unknown MailCLI bridge operation");
    }
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
        const response = dispatch(Application(request.mail_pid), request);
        response.ok = true;
        response.error = null;
        return JSON.stringify(response);
    } catch (error) {
        return JSON.stringify({
            ok: false,
            send_attempted: Boolean(error.sendAttempted),
            error: {
                code: error.bridgeCode || "mail_error",
                message: error.bridgeCode ? error.message : "Mail.app Apple Events operation failed"
            }
        });
    }
}
