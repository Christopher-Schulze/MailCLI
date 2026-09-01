# MailCLI

Local Apple Mail access for the shell and coding agents.

[![Go 1.27](https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white)](go.mod)
[![Platform](https://img.shields.io/badge/macOS-Apple%20silicon-000000?logo=apple)](#compatibility)
[![License](https://img.shields.io/badge/license-MIT-2ea44f)](LICENSE)

MailCLI gives command-line tools and agents a typed interface to the accounts already configured in Apple Mail. It reads mail from Mail's local store, performs writes through the running Mail app, and never asks for Gmail, iCloud, IMAP, SMTP, OAuth, or app-specific credentials.

```bash
mailcli messages search --sender example.com --after 2026-01-01 --json
mailcli messages get --ref MESSAGE_REF --json
mailcli attachments save --message MESSAGE_REF --attachment ATTACHMENT_ID --output /absolute/path/file.pdf --json
```

## Why MailCLI

Apple Mail's scripting interface can perform targeted mailbox mutations but performs poorly for large reads and is unsafe for composition on the verified Mail 16 host. MailCLI separates those workloads and fails closed where Mail cannot preserve reviewed content.

- Lists, filters, metadata searches, message reads, raw source, and downloaded attachments use Mail's local store without Apple Events. A targeted incomplete-content fallback accepts only Mail's raw RFC 5322 source as body or MIME-part evidence.
- Body search scans the selected `.emlx` sources on demand within explicit message and byte limits. MailCLI creates no second mail index.
- Mark, move, copy, delete, sync, and targeted incomplete-content fallback use serialized Apple Events against the exact running Mail process.
- Local new, reply, reply-all, and forward drafts remain fully reviewable. Native compose, draft export, outbound attachments, and send report `compose_automation_unsupported` before contacting Mail because live Mail 16 tests discarded body/recipient data and retained phantom drafts.
- Machine output uses one versioned JSON envelope with typed errors, opaque references, explicit pagination, and search coverage.

Reads of locally available content on the supported store profile add no work to the Mail process. Requesting content that Mail has not downloaded may use one targeted Apple Event. Explicit writes still require Mail.app to do the requested work. MailCLI bounds and serializes those calls so multiple agents cannot build an Apple Events queue behind the application.

## Capabilities

Run `mailcli capabilities --json` before automation. Its versioned response is the authoritative command contract for agents: command IDs, read/write class, confirmation, Mail-store and Mail.app dependencies, result states, and hard limits. Help text is for humans and must not be parsed as capability data.

| Area | Commands | Behavior |
|---|---|---|
| Accounts | `accounts list` | Lists configured accounts and sender identities |
| Mailboxes | `mailboxes list`, `mailboxes resolve` | Handles Inbox, Sent, Drafts, Archive, Junk, Trash, custom folders, and nested labels |
| Messages | `messages list`, `filter`, `search`, `get`, `raw` | Pages metadata, applies typed filters, scans bodies, and returns normalized or RFC 5322 content |
| Attachments | `attachments list`, `attachments save` | Inspects and exports received files without overwriting a destination |
| Composition | `drafts create`, `list`, `inspect`, `update`, `open`, `discard` | Manages reviewable local drafts and inspects existing Mail drafts; `save` and `send` are blocked on Mail 16 |
| Responses | `messages reply`, `messages forward` | Creates local reply, reply-all, and forward review drafts without opening a compose object |
| Organization | `messages mark`, `move`, `copy`, `delete` | Revalidates message identity before each mutation and reads back the result |
| Synchronization | `sync` | Checks all mail or synchronizes one selected account |
| Maintenance | `update` | Checks GitHub, verifies the release checksum, and atomically updates the binary and companion skill |

Run `mailcli help` for the compact command overview. Focused command help accepts
`help`, `-h`, or `--help` and renders aligned long options with semantic value
names and readable defaults.

Capability discovery, local draft management, local reply/forward creation, and
the explicitly unsupported native save/send preflight bypass the Mail store and
Mail.app entirely. Missing or unknown command/subcommand routes do the same.
They therefore avoid SQLite, `plutil`, and Apple Events startup work.

## Architecture

```mermaid
flowchart LR
    Agent["Agent or shell"] --> CLI["MailCLI"]
    CLI -->|"list, filter, search, get, raw"| Store["Read-only Mail store adapter"]
    Store --> Index["Envelope Index"]
    Store --> EMLX[".emlx message sources"]
    CLI -->|"mutate, sync"| Gate["Cross-process access gate"]
    CLI -->|"one missing body or attachment"| Gate
    Gate --> Bridge["Targeted Apple Events bridge"]
    Bridge --> Mail["Already-running Mail.app"]
```

Mail.app remains the source of truth. MailCLI has no daemon, background process, watcher, copied corpus, owned search index, or persistent cache of its own.

The store adapter opens Mail's Envelope Index with SQLite `mode=ro`, `query_only=1`, WAL participation, and a private connection cache. Before reading, it validates the store version, framework version, UUID, required schema, account catalog, mailbox mapping, and filesystem containment. Message, mailbox-cache, and external-attachment files are opened relative to one held Mail-store directory descriptor with macOS `O_NOFOLLOW_ANY`; selection, hashing, and copying remain bound to the same regular-file identity. An unknown profile fails with `unsupported_mail_store_schema` before any message query runs.

The write bridge acquires a context-aware BSD advisory lock, requires Mail to be running, verifies the exact `com.apple.mail` process identity, and binds each request to that PID. Before any potentially mutating Apple Event, it durably pre-arms an exact-PID recovery marker; if that write cannot be synchronized, the operation does not start. SIGINT, SIGTERM, and context cancellation also write a private bridge marker so the script can close an owned unsent compose object; only an unresponsive bridge is force-stopped after the cleanup grace period. Each invocation owns one `osascript` process group, reaps it, and verifies that the group is gone before releasing the gate. A definite completion clears the pre-armed state; an incomplete operation or caller crash leaves it latched as `mail_recovery_required` until the affected Mail process has been replaced. Read probes, search, raw reads, sync triggers, and Automation denial do not create false recovery state. MailCLI never launches, activates, quits, kills, or restarts Mail.

See [docs/documentation.md](docs/documentation.md) for the full command, reference, cursor, draft, send, and coverage contracts.

## Compatibility

MailCLI has a deliberately narrow support target:

| Requirement | Supported value |
|---|---|
| Operating system | macOS 15.6.1 |
| Architecture | Apple silicon, `darwin/arm64` |
| Mail | Mail 16.0, build `3826.700.81` |
| Envelope Index | Store version `4`, minor version `74003` |
| Go toolchain | Go 1.27 or newer for source builds |

MailCLI fails closed when the Mail store profile changes. This protects the local database from speculative compatibility code, but it also means a new macOS or Mail release may require an explicit adapter update.

## Install

The `v1.0.4` release archive installs both the native CLI and its companion agent skill:

```bash
VERSION=1.0.4
curl -fLO "https://github.com/Christopher-Schulze/MailCLI/releases/download/v${VERSION}/mailcli_${VERSION}_darwin_arm64.tar.gz"
curl -fLO "https://github.com/Christopher-Schulze/MailCLI/releases/download/v${VERSION}/SHA256SUMS"
shasum -a 256 -c SHA256SUMS
tar -xzf "mailcli_${VERSION}_darwin_arm64.tar.gz"
"./mailcli_${VERSION}_darwin_arm64/install.sh"
command -v mailcli
mailcli version --json
```

The release installer copies the verified binary to `~/.local/bin/mailcli` and the skill to `~/.agents/skills/mailcli`. It stages and verifies both before replacing an existing installation, restores backups on failure, rejects symbolic-link destinations and unresolved backup paths, and never removes macOS security attributes. Start a new agent session after installation so the skill is discovered.

After the first installation, update both components with:

```bash
mailcli update
mailcli update --json
```

Interactive terminals show bounded progress while MailCLI checks GitHub, downloads the exact `darwin/arm64` asset, verifies `SHA256SUMS`, validates the package binary and signature, and runs the rollback-safe installer. JSON mode emits exactly one envelope and no animation. Concurrent updaters are serialized, release URLs must use HTTPS, and shell startup injection variables are removed from the installer environment.

To build from source instead, install Go 1.27 or newer and the Xcode Command Line Tools for CGO:

```bash
git clone https://github.com/Christopher-Schulze/MailCLI.git
cd MailCLI
./scripts/build/install-local.sh
```

The source installer builds a native `darwin/arm64` executable and copies only the binary to `~/.local/bin/mailcli` by default. Pass an explicit destination as the first argument to install elsewhere, then install or link [`skills/mailcli`](skills/mailcli) separately when agent support is needed.

The current release binary is ad-hoc signed but not Apple-notarized because no Developer ID identity is available. A browser download can therefore be quarantined by Gatekeeper. Verify `SHA256SUMS` first; if macOS still blocks the verified binary, explicitly remove only that binary's quarantine attribute with `xattr -d com.apple.quarantine ~/.local/bin/mailcli`. The installer never performs this bypass automatically.

### Grant permissions

MailCLI uses the permissions of the process that launches it. Grant permissions to Terminal, Codex, or the relevant agent host in **System Settings > Privacy & Security**.

| Permission | Required for |
|---|---|
| Full Disk Access | Accounts, mailboxes, messages, searches, raw source, and downloaded attachments |
| Automation access to Mail | Live diagnostics, missing-content fallback, message mutations, and sync |

Accessibility and Screen Recording are not required.

Verify the read path without contacting Mail.app:

```bash
mailcli doctor --json
```

Use the live probe only before an operation that needs Apple Events:

```bash
mailcli doctor --live --json
```

Mail must already be running. The live probe never launches Mail or creates, saves, or sends a message. It verifies the exact process identity and performs one read-only Apple Events version query. Mail 16 retains some hidden outgoing backends even after `close saving no`, so a healthcheck must never manufacture a compose object.

## Usage

### Discover accounts and mailboxes

Never guess account, mailbox, message, or attachment identifiers. Resolve them through the CLI.

```bash
mailcli accounts list --json
mailcli mailboxes list --account ACCOUNT_REF --json
mailcli mailboxes resolve --account ACCOUNT_REF --path Gesendet --json
mailcli mailboxes resolve --account ACCOUNT_REF --path Projects --path 2026 --json
```

### List, filter, and search

```bash
mailcli messages list --mailbox MAILBOX_REF --limit 25 --json
mailcli messages filter --mailbox MAILBOX_REF --read false --attachment true --json
mailcli messages search --sender example.com --after 2026-01-01 --json
mailcli messages search --query "invoice tracking number" --max-messages 50000 --json
```

List, filter, and search commands accept page sizes from 1 through 25. Continue with `data.page.next_cursor` until it is absent. Body search starts with the smallest result window needed for the requested page and grows its two-worker scan window only while candidates do not match. It reports candidate counts, scanned messages and bytes, partial sources, missing sources, bounds, and `data.page.coverage.complete`.

### Read messages and attachments

```bash
mailcli messages get --ref MESSAGE_REF --json
mailcli messages raw --ref MESSAGE_REF
mailcli attachments list --message MESSAGE_REF --json
mailcli attachments save \
  --message MESSAGE_REF \
  --attachment ATTACHMENT_ID \
  --output /absolute/non-existing/path/document.pdf \
  --json
```

`messages get` returns normalized content and completion metadata. A fallback result is complete only after MailCLI parses a full raw RFC 5322 source; failed Mail scripting properties remain explicitly incomplete. `messages raw` returns the unmodified RFC 5322 message and rejects sources above 64 MiB before materializing them through Mail.app. Attachment IDs are deterministic MIME-part paths, including during targeted fallback, while exported attachment files contain the decoded MIME-part bytes and report their media type, byte count, and SHA-256. Attachment export requires an absolute path and refuses to overwrite an existing file.

### Create and review a message

MailCLI uses local structured drafts as the review boundary. Creating or editing a local draft never sends mail.

```bash
printf '%s' '{
  "from": "me@example.com",
  "to": [{"name": "Recipient", "address": "recipient@example.com"}],
  "cc": [],
  "bcc": [],
  "subject": "Project update",
  "body": "Hello,\n\nHere is the update.\n\nBest regards\n",
  "attachments": ["/absolute/path/report.pdf"]
}' | mailcli drafts create --input - --json

mailcli drafts inspect --ref DRAFT_REF --json
```

Every draft JSON object must contain an explicit `body` field; an intentionally empty string is valid. Duplicate addresses across To, CC, and BCC are rejected. Hard limits are 64 KiB of subject text, 4 MiB of reviewed body text, 200 total recipients, 100 attachments, and 512 MiB of attachment bytes. MailCLI records attachment size and SHA-256 when it creates or updates a local draft.

On the verified Mail 16 build, `drafts save` and `drafts send` return `compose_automation_unsupported` before acquiring the Apple Events gate. Live tests proved that Mail can accept scripted setters while persisting only the automatic signature, lose recipients, reject scripted attachments, and retain an invisible outgoing backend after `close saving no`. `mailcli capabilities --json` therefore reports `compose_write:false`, `compose_attachment_write:false`, and `send_transport:"none"`. Retained reconciliation fails closed unless Mail supplied an exact final native body and exact headers, recipient roles, and attachment count. Use Mail's UI for final composition and sending.

### Reply, forward, and organize

```bash
printf '%s' '{"body":"Thanks, I will review this.\n"}' \
  | mailcli messages reply --message MESSAGE_REF --input - --all --json

printf '%s' '{"to":[{"address":"recipient@example.com"}],"body":"For your review.\n"}' \
  | mailcli messages forward --message MESSAGE_REF --input - --json

mailcli messages mark --ref MESSAGE_REF --read true --flagged false --json
mailcli messages move --ref MESSAGE_REF --mailbox DESTINATION_MAILBOX_REF --json
mailcli messages copy --ref MESSAGE_REF --mailbox DESTINATION_MAILBOX_REF --json
mailcli messages delete --ref MESSAGE_REF --confirm --json
mailcli sync --account ACCOUNT_REF --json
```

Reply and forward commands require a store-bound source reference and create local review drafts. They do not open, save, or send a Mail compose object. Forward inputs still require an explicit destination.

Mark, move, and delete reject messages identified as drafts. To intentionally mutate a draft, first close every editor for it and repeat with `--allow-draft`; deletion still also requires `--confirm`. This prevents Mail from recreating a deleted or moved draft when an open editor later saves.

## JSON contract

Every data-bearing command supports `--json` and returns the same top-level envelope:

```json
{
  "schema_version": 1,
  "ok": true,
  "command": "accounts.list",
  "data": {
    "accounts": []
  },
  "error": null
}
```

Exit code `0` means success, `1` means a runtime or Mail failure, and `2` means invalid CLI usage. Errors use stable machine-readable codes and actionable messages. References and cursors are opaque and bound to the current Mail store. Resolve a fresh reference after moving, copying, deleting, or synchronizing a message.

## Agent skill

The repository includes a companion skill at [`skills/mailcli`](skills/mailcli). It teaches Codex and compatible agents when to invoke MailCLI, how to paginate every mailbox, how to interpret incomplete search coverage, and how to honor the Mail 16 compose boundary.

The release installer places it at `~/.agents/skills/mailcli`, the personal skill location discovered by Codex. For a source checkout, link the repository copy manually:

```bash
mkdir -p "${HOME}/.agents/skills"
ln -s "$(pwd)/skills/mailcli" "${HOME}/.agents/skills/mailcli"
```

The link command refuses if a skill already exists at that destination. It does not overwrite an installed skill. Start a new agent session after either installation method.

## Safety model

| Guarantee | Mechanism |
|---|---|
| No provider credentials | Reuses accounts and Keychain-backed authentication already configured in Mail.app |
| No Mail database writes | Opens the Envelope Index read-only and rejects journal or schema mutations |
| No owned mail index | Searches current local sources on demand and persists no corpus |
| No broad Apple Events reads | Uses the store for enumeration and search, with fallback for one resolved incomplete message only |
| No corrupted or phantom compose data | Blocks native compose, draft export, outbound attachment insertion, and send on Mail 16 before any Apple Event |
| No Mail.app lifecycle control | Requires the exact running Mail PID and never launches, activates, quits, kills, or restarts Mail.app |
| No residual bridge process | Waits for the owned `osascript` leader, terminates residual group members, and verifies process-group absence before command completion |
| No Apple Events backlog after uncertainty | Durably pre-arms the exact affected Mail PID before mutation and rejects every later live operation after an incomplete caller until that process has been replaced |
| No accidental overwrite or path substitution | Attachment export accepts only an absolute destination that does not exist; Mail-store sources reject symlinks and file-identity replacement |
| No silent incomplete search | Reports source completeness and scan bounds on every search page |

MailCLI stores only local review drafts, historical send/save claims, and access/update lock state under `~/Library/Application Support/MailCLI`. State directories use mode `0700`; state files use mode `0600`.

## Limitations

- Mail 16 native composition is disabled. Scripted plain text, HTML, recipient, attachment, save, and send behavior is not reliable enough to preserve reviewed data.
- Local reply and forward drafts capture intent but cannot guarantee Mail-native threading, quoted content, or original forwarded attachments until completed in Mail's UI.
- `drafts open` inspects a persisted native draft headlessly; Mail 16 has no reliable headless in-place editor for it.
- Messages that are not fully downloaded may need one targeted Apple Events fallback. The result reports remaining missing parts instead of claiming completeness.
- Body search is bounded work over current `.emlx` sources, not an instant persistent index. Narrow account, mailbox, sender, date, or subject scope for large stores.

## Development

```bash
./scripts/tests/test.sh
./scripts/build/build.sh
./scripts/release/build-release.sh 1.0.5
MAILCLI_LIVE_TESTS=1 go test -count=1 -run '^TestLive' -v ./internal/mailstore
./scripts/tests/test-live-responsiveness.sh
```

The main gate checks every shell script's syntax and executable bit, then runs `gofmt`, module verification, Staticcheck, `go vet`, `golangci-lint`, `govulncheck`, uncached race tests, coverage, architecture regression checks, and isolated release installation tests. The release builder refuses to overwrite assets and writes the combined archive plus `SHA256SUMS` to `dist/` unless `MAILCLI_RELEASE_DIRECTORY` selects an empty absolute directory. Release binaries use `-trimpath -ldflags='-s -w'`; the release test rejects DWARF sections and binaries above 10 MiB. On the verified Go 1.27 host this reduced the executable from 15,496,930 to 9,431,826 bytes without changing runtime code or dependencies. Live tests are opt-in. Build the binary before running the responsiveness gate; it executes three bounded read-only live probes, requires the exact same Mail process identity and compose-object count, rejects residual `mailcli` or `osascript` processes and Mail-held repository handles, and verifies that the post-operation probe remains within the measured and absolute latency bounds. On the supported release host, bypassing store startup reduced process-inclusive `drafts list --json` peak RSS from 10.13-10.45 MB to 6.59-6.78 MB; this is a host-specific reference measurement, not a platform guarantee.

## License

MailCLI is available under the [MIT License](LICENSE). Copyright 2026 Christopher Schulze.
