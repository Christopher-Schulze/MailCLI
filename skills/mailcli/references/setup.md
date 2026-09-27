# Installation, setup, and diagnostics

Before extraction/install, trusted OpenSSL 3 must verify README-pinned Ed25519 over exact SHA256SUMS and archive digest; both required.

1. Checkout: `scripts/build/install-local.sh [BINARY_DESTINATION]`, default ~/.local/bin/mailcli, skill override MAILCLI_SKILL_DESTINATION; transactional matching binary/skill, restart agent. Optional `scripts/utils/mailcli-preflight.sh capabilities --binary PATH [--for IDS]` caches ok JSON/0600 by binary SHA-256/schema/selection, no expansion; --refresh refreshes, invalidate clears. Otherwise session-cache current scoped contracts.

2. Read-only checkout drift: `scripts/tests/report-skill-drift.sh --repository PATH --installed PATH`; match=current, missing/mismatch/unstable need reconciliation, never install.

Obey conditional dependencies; empty means none. Store: doctor --json; refresh after store/permission/schema/account/read failure. Healthy checkout cache <=300 s. Never cache `doctor --live`; run before Apple Events with Mail running/Automation permission (also read fallback). Store reads: Full Disk Access/no Automation; direct send: no Mail/Full Disk Access.

Codex ~/.agents/skills/mailcli; Claude Code ~/.claude/skills/mailcli can symlink verified default. Create only if absent; inspect existing/dangling entries. Links follow updates; separate copies use original installer/same MAILCLI_SKILL_DESTINATION, ignored by self-update. Other hosts/cloud: documented paths.

Gmail/iCloud: `mailcli send setup --from ALIAS [--account REF] --json`. Other domains add account + SMTP/IMAP host/port flags; --credential-account LOGIN selects credential identity. Resolve account/alias via accounts list, obey schema; public endpoints validate before password entry. Never guess hosts/passwords in chat.

Built-in app passwords, no Google/Apple OAuth; both require two-factor auth. Google organization/Advanced Protection policy may disallow app passwords.

Keychain success invalidates caches. partial_effects retains keychain_store:complete, binding_publish:none/unknown/complete, optional lock_release:failed. Unverified rename sync/identity=unknown; lock failure cannot undo complete. account_binding_changed: binding unpublished, inspect accounts list/decide; no auto-retry/credential rollback. Explicit flags win, implicit fields locked; validation/Keychain failure=no effect.

Never unlink binding locks (legacy then new). Retained default account-bindings.lock; custom account-bindings-<full lowercase basename SHA256>.lock in pinned parent, independent per basename.

Update only on user request: `mailcli update --json` verifies signed binary/skill. Read `mailcli version --json`, refresh identity/contracts after replacement.

Legacy text probes: `MAILCLI_OUTPUT=human mailcli update` or `MAILCLI_OUTPUT=human /absolute/path/install.sh`; current entrypoints select compatible probes.

Failed install: inspect update_result path/latest_version/updated/failed_phase; complete only if installed-version verified, otherwise unknown. Read version --json, no auto-rerun/assumed rollback.

Install lock ~/Library/Application Support/MailCLI/update.lock persists through cleanup. Direct wait <=30 s; updater deadline/child ownership. Self-update ignores MAILCLI_INSTALL_PACKAGE_ROOT, source honors it. Never unlink/steal/age-expire; retain refused recovery/artifacts, cancel whole process group.

IMAP reads shared; APPEND/mutations exclusive.
