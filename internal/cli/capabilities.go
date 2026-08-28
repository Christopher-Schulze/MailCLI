package cli

import (
	"fmt"
	"io"

	"mailcli/internal/mail"
)

const capabilitySchemaVersion = 1

type capabilityManifest struct {
	SchemaVersion int                 `json:"schema_version"`
	Name          string              `json:"name"`
	Version       string              `json:"version"`
	Commands      []commandCapability `json:"commands"`
	Limits        capabilityLimits    `json:"limits"`
}

type commandCapability struct {
	ID                string   `json:"id"`
	EffectClass       string   `json:"effect_class"`
	Confirmation      string   `json:"confirmation"`
	StoreDependency   string   `json:"store_dependency"`
	MailAppDependency string   `json:"mail_app_dependency"`
	ResultStates      []string `json:"result_states"`
}

type capabilityLimits struct {
	Platform               string `json:"platform"`
	Architecture           string `json:"architecture"`
	OwnsMailIndex          bool   `json:"owns_mail_index"`
	BackgroundProcess      bool   `json:"background_process"`
	RawMIMERead            bool   `json:"raw_mime_read"`
	RawMIMESend            bool   `json:"raw_mime_send"`
	SendTransport          string `json:"send_transport"`
	MaximumPageSize        int    `json:"maximum_page_size"`
	MaximumDraftInputBytes int    `json:"maximum_draft_input_bytes"`
}

func capabilities() capabilityManifest {
	read := func(id, store, mailApp string, states ...string) commandCapability {
		return commandCapability{
			ID: id, EffectClass: "read", Confirmation: "none",
			StoreDependency: store, MailAppDependency: mailApp, ResultStates: states,
		}
	}
	write := func(id, class, confirmation, store, mailApp string, states ...string) commandCapability {
		return commandCapability{
			ID: id, EffectClass: class, Confirmation: confirmation,
			StoreDependency: store, MailAppDependency: mailApp, ResultStates: states,
		}
	}
	return capabilityManifest{
		SchemaVersion: capabilitySchemaVersion,
		Name:          name,
		Version:       version,
		Commands: []commandCapability{
			read("capabilities", "none", "none", "available"),
			read("version", "none", "none", "available"),
			read("doctor", "mail-store", "optional-automation", "healthy", "unhealthy"),
			read("accounts.list", "mail-store", "none", "complete"),
			read("mailboxes.list", "mail-store", "none", "complete"),
			read("mailboxes.resolve", "mail-store", "none", "resolved"),
			read("messages.list", "mail-store", "none", "complete"),
			read("messages.filter", "mail-store", "none", "complete"),
			read("messages.search", "mail-store", "none", "complete"),
			read("messages.get", "mail-store", "fallback-automation", "complete", "partial"),
			read("messages.raw", "mail-store", "none", "complete"),
			read("attachments.list", "mail-store", "fallback-automation", "complete", "partial"),
			write("attachments.save", "filesystem-write", "none", "mail-store", "fallback-automation", "saved"),
			write("drafts.create", "local-write", "none", "draft-store", "none", "created"),
			read("drafts.list", "draft-store", "none", "complete"),
			read("drafts.inspect", "draft-store", "none", "complete"),
			write("drafts.update", "local-write", "none", "draft-store", "none", "updated"),
			write("drafts.save", "mail-write", "none", "mail-and-draft-store", "automation", "saved"),
			read("drafts.open", "mail-store", "automation", "complete"),
			write("drafts.send", "external-effect", "required-flag", "mail-and-draft-store", "automation", "sent_store_observed", "accepted_by_mail", "outcome_unknown"),
			read("drafts.reconcile", "mail-and-draft-store", "none", "sent_store_observed", "accepted_by_mail", "outcome_unknown"),
			write("drafts.discard", "local-write", "required-flag", "draft-store", "none", "discarded"),
			write("messages.reply", "local-write", "none", "mail-and-draft-store", "none", "created"),
			write("messages.forward", "local-write", "none", "mail-and-draft-store", "none", "created"),
			write("messages.mark", "mail-write", "none", "mail-store", "automation", "updated"),
			write("messages.move", "mail-write", "none", "mail-store", "automation", "moved"),
			write("messages.copy", "mail-write", "none", "mail-store", "automation", "copied"),
			write("messages.delete", "mail-write", "required-flag", "mail-store", "automation", "deleted"),
			write("sync", "mail-write", "none", "mail-store", "automation", "triggered"),
		},
		Limits: capabilityLimits{
			Platform: "darwin", Architecture: "arm64",
			OwnsMailIndex: false, BackgroundProcess: false,
			RawMIMERead: true, RawMIMESend: false,
			SendTransport:          "mail-app-compose",
			MaximumPageSize:        mail.MaximumPageLimit,
			MaximumDraftInputBytes: maximumDraftInputBytes,
		},
	}
}

func runCapabilities(args []string, stdout io.Writer, stderr io.Writer) int {
	if helpOnly(args) {
		writeLine(stdout, "usage: mailcli capabilities [--json]")
		return 0
	}
	flags, err := parseBooleanFlags(args, "--json")
	if err != nil {
		writeLine(stderr, err)
		return 2
	}
	manifest := capabilities()
	if flags["--json"] {
		return writeJSON(stdout, envelope{
			SchemaVersion: schemaVersion,
			OK:            true,
			Command:       "capabilities",
			Data:          responseData{Capabilities: &manifest},
		})
	}
	for _, command := range manifest.Commands {
		writeFormat(
			stdout, "%s\t%s\tconfirmation=%s\tstore=%s\tmail_app=%s\tstates=%s\n",
			command.ID, command.EffectClass, command.Confirmation, command.StoreDependency,
			command.MailAppDependency, fmt.Sprint(command.ResultStates),
		)
	}
	return 0
}
