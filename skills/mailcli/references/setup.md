# Installation, setup, and diagnostics

--for IDS --schemas inlines schema_ref contracts; --outputs adds schema.output/$defs.

Before extraction/install, trusted OpenSSL 3 verifies README-pinned Ed25519 over exact SHA256SUMS and archive digest; both required.

Checkout: `scripts/build/install-local.sh [BINARY_DESTINATION]`, default ~/.local/bin/mailcli, MAILCLI_SKILL_DESTINATION override; binary/skill transaction, restart agent. Cache scoped contracts.

Obey conditional dependencies; empty=none. Store: doctor --json; refresh after store/permission/schema/account/read failure. Healthy checkout cache <=300 s. Never cache `doctor --live`; run before Apple Events/read fallback with Mail running/Automation permission. Store reads: Full Disk Access/no Automation; direct send: no Mail/Full Disk Access.

Codex ~/.agents/skills/mailcli; Claude Code ~/.claude/skills/mailcli can link verified default. Create only if absent; inspect existing/dangling entries. Links follow updates; copies use original installer/same MAILCLI_SKILL_DESTINATION, ignored by self-update. Other hosts/cloud: documented paths.

Gmail/iCloud: `mailcli send setup --from ALIAS [--account REF] --json`. Other domains add account + SMTP/IMAP host/port; --credential-account LOGIN selects login. Resolve account/alias via accounts list, obey schema; validate public endpoints before password entry. Never guess hosts/passwords in chat.

Built-in app passwords, no Google/Apple OAuth; both require two-factor auth. Google organization/Advanced Protection policy may disallow app passwords.

Keychain success invalidates caches. partial_effects: keychain_store:complete, binding_publish:none/unknown/complete, optional lock_release:failed. Unverified rename sync/identity=unknown; lock failure cannot undo complete. account_binding_changed: unpublished binding, inspect accounts list/decide; no auto-retry/credential rollback. Explicit flags win, implicit fields locked; validation/Keychain failure=no effect.

Never unlink binding locks (legacy then new). Default account-bindings.lock; custom account-bindings-<full lowercase basename SHA256>.lock in pinned parent, independent per basename.

User-requested `mailcli update --json` verifies signed binary/skill. Read `mailcli version --json`, refresh identity/contracts after replacement.

Legacy text: `MAILCLI_OUTPUT=human mailcli update` or `MAILCLI_OUTPUT=human /absolute/path/install.sh`; entrypoints select compatible probes.

Failed install: inspect update_result path/latest_version/updated/failed_phase; complete only with verified installed-version, otherwise unknown. Read version --json, no rerun/assumed rollback.

Persistent lock ~/Library/Application Support/MailCLI/update.lock: Direct wait <=30 s; updater deadline/child ownership. Never unlink/steal/age-expire; retain refused recovery/artifacts, cancel whole process group.

IMAP reads shared; APPEND/mutations exclusive.
