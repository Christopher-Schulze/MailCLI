package mailstore

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const envelopeIndexName = "Envelope Index"

type Config struct {
	MailRoot          string
	PreferencesPath   string
	ActiveAccountURLs []string
}

func DefaultConfig() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve home directory: %w", err)
	}
	return Config{
		MailRoot: filepath.Join(home, "Library", "Mail"),
		PreferencesPath: filepath.Join(
			home, "Library", "Containers", "com.apple.mail", "Data", "Library",
			"Preferences", "com.apple.mail.plist",
		),
	}, nil
}

func discoverVersionRoot(mailRoot string) (string, error) {
	entries, err := os.ReadDir(mailRoot)
	if err != nil {
		return "", operationError(
			"mail_store_unavailable",
			fmt.Sprintf("cannot read %s; grant Full Disk Access to the agent host: %v", mailRoot, err),
		)
	}
	type candidate struct {
		version int
		path    string
	}
	var candidates []candidate
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || len(name) < 2 || name[0] != 'V' {
			continue
		}
		version, err := strconv.Atoi(name[1:])
		if err != nil || version < 1 {
			continue
		}
		path := filepath.Join(mailRoot, name)
		info, err := os.Lstat(filepath.Join(path, "MailData", envelopeIndexName))
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		candidates = append(candidates, candidate{version: version, path: path})
	}
	if len(candidates) == 0 {
		return "", operationError(
			"mail_store_unavailable",
			fmt.Sprintf("no readable Mail Envelope Index exists under %s", mailRoot),
		)
	}
	for _, candidate := range candidates {
		if candidate.version == 10 {
			return candidate.path, nil
		}
	}
	sort.Slice(candidates, func(left int, right int) bool {
		return candidates[left].version > candidates[right].version
	})
	if candidates[0].version > 10 {
		return "", operationError(
			"unsupported_mail_store_schema",
			fmt.Sprintf("Mail store layout V%d is unsupported; upgrade MailCLI", candidates[0].version),
		)
	}
	return "", operationError(
		"mail_store_unavailable",
		fmt.Sprintf("no readable V10 Mail Envelope Index exists under %s", mailRoot),
	)
}

func loadActiveAccountURLs(ctx context.Context, config Config) ([]string, error) {
	if len(config.ActiveAccountURLs) > 0 {
		return append([]string(nil), config.ActiveAccountURLs...), nil
	}
	command := exec.CommandContext(
		ctx, "/usr/bin/plutil", "-extract", "AccountOrdering", "xml1", "-o", "-",
		config.PreferencesPath,
	)
	output, err := command.Output()
	if err != nil {
		return nil, operationError(
			"mail_store_preferences_unavailable",
			fmt.Sprintf("cannot read Mail account ordering from %s: %v", config.PreferencesPath, err),
		)
	}
	urls, err := parseAccountOrderingXML(output)
	if err != nil {
		return nil, operationError("mail_store_preferences_invalid", err.Error())
	}
	return urls, nil
}

func parseAccountOrderingXML(source []byte) ([]string, error) {
	var document struct {
		Values []string `xml:"array>string"`
	}
	if err := xml.Unmarshal(source, &document); err != nil {
		return nil, fmt.Errorf("parse Mail AccountOrdering property: %w", err)
	}
	if len(document.Values) == 0 {
		return nil, fmt.Errorf("mail AccountOrdering property is empty")
	}
	values := make([]string, 0, len(document.Values))
	seen := make(map[string]struct{}, len(document.Values))
	for _, value := range document.Values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("mail AccountOrdering contains an empty URL")
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	return values, nil
}
