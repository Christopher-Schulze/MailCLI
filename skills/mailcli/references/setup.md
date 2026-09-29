# Installation, setup, and diagnostics

Check the installation with `mailcli version --json`. Its `data.contract_sha256` identifies the contract; reload `mailcli capabilities --for IDS --schemas --json` when it changes. Install or update with `mailcli update` or the signed release the user provides.

Obey conditional dependencies; an empty list means none. Run `mailcli doctor --json` before store work and again after a store, permission, schema, account or read failure. Never cache `doctor --live`; run it before Apple Events or a read fallback, with Mail running and Automation permission granted. Store reads need Full Disk Access and no Automation; direct send needs neither Mail nor Full Disk Access.

Codex ~/.agents/skills/mailcli; Claude Code ~/.claude/skills/mailcli can link verified default. Create only if absent; inspect existing/dangling entries. Links follow updates; copies use original installer/same MAILCLI_SKILL_DESTINATION, ignored by self-update. Other hosts/cloud: documented paths.

Gmail/iCloud: `mailcli send setup --from ALIAS [--account REF] --json`. Other domains add account + SMTP/IMAP host/port; --credential-account LOGIN selects login. Resolve account/alias via accounts list, obey schema; validate public endpoints before password entry. Never guess hosts; never ask the user to paste account passwords in chat.

Built-in app passwords, no Google/Apple OAuth; both require two-factor auth. Google organization/Advanced Protection policy may disallow app passwords.

Keychain success invalidates caches. partial_effects: keychain_store:complete, binding_publish:none/unknown/complete, optional lock_release:failed. Unverified rename sync/identity=unknown; lock failure cannot undo complete. account_binding_changed: unpublished binding, inspect accounts list/decide; no auto-retry/credential rollback. Explicit flags win, implicit fields locked; validation/Keychain failure=no effect.

Never unlink binding locks.

`mailcli update --check --json` is read-only: `update_available` says whether a newer release exists, nothing is installed. User-requested `mailcli update --json` verifies signed binary/skill. Read `mailcli version --json`, refresh identity/contracts after replacement.

Legacy text: `MAILCLI_OUTPUT=human mailcli update` or `MAILCLI_OUTPUT=human /absolute/path/install.sh`; entrypoints select compatible probes.

Failed install: inspect update_result path/latest_version/updated/failed_phase; complete only with verified installed-version, otherwise unknown. Read version --json, no rerun/assumed rollback.

Persistent lock ~/Library/Application Support/MailCLI/update.lock: Direct wait <=30 s; updater deadline/child ownership. Never unlink/steal/age-expire; retain refused recovery/artifacts, cancel whole process group.

IMAP reads shared; APPEND/mutations exclusive.
