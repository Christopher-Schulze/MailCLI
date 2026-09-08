package mailstore

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"mailcli/internal/mail"
)

const (
	envelopeIndexName            = "Envelope Index"
	persistenceInfoName          = "PersistenceInfo.plist"
	supportedStoreDirectoryName  = "V10"
	supportedStoreDirectoryValue = 10
	maximumPersistenceInfoBytes  = 64 * 1024
)

type generationClassification string

const (
	generationSupported   generationClassification = "supported"
	generationUnsupported generationClassification = "unsupported"
	generationIncomplete  generationClassification = "incomplete"
	generationUnsafe      generationClassification = "unsafe"
)

type storeGenerationCandidate struct {
	name           string
	version        int
	path           string
	classification generationClassification
	detail         string
}

type Config struct {
	MailRoot string
	// MailStorePath optionally pins an exact Mail store generation directory.
	// When empty, MailRoot is inventoried before automatic selection.
	MailStorePath     string
	PreferencesPath   string
	ActiveAccountURLs []string
	AccountBindings   mail.AccountBindingStore
	// SenderIdentityScanLimit optionally increases the bounded Sent-history
	// scan. Zero uses mail.DefaultSenderIdentityScanLimit; values above
	// mail.MaximumSenderIdentityScanLimit are rejected.
	SenderIdentityScanLimit int
}

func normalizeSenderIdentityScanLimit(requested int) (int, error) {
	if requested == 0 {
		return mail.DefaultSenderIdentityScanLimit, nil
	}
	if requested < 1 || requested > mail.MaximumSenderIdentityScanLimit {
		return 0, operationError(
			"invalid_argument",
			fmt.Sprintf(
				"sender identity scan limit must be between 1 and %d",
				mail.MaximumSenderIdentityScanLimit,
			),
		)
	}
	return requested, nil
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
	candidates, err := inventoryStoreGenerations(mailRoot)
	if err != nil {
		return "", err
	}
	if len(candidates) == 0 {
		return "", operationError(
			"mail_store_unavailable",
			fmt.Sprintf("no readable Mail Envelope Index exists under %s; no Mail store generation directory is present", mailRoot),
		)
	}
	return chooseVersionRoot(mailRoot, candidates)
}

func chooseVersionRoot(mailRoot string, candidates []storeGenerationCandidate) (string, error) {
	supported := supportedGenerationCandidates(candidates)
	newer := hasNewerGeneration(candidates)
	peer := hasSameGenerationPeer(candidates)
	if len(supported) == 1 && !newer && !peer {
		return supported[0].path, nil
	}
	if peer && len(supported) == 1 {
		return "", ambiguousGenerationError(
			mailRoot, candidates,
			"multiple V10 generation directory names are present",
		)
	}
	if len(supported) == 1 && newer {
		return chooseSupportedWithNewer(mailRoot, candidates, supported[0])
	}
	if newer {
		return chooseNewerOnly(mailRoot, candidates)
	}
	if unsafe := unsafeSupportedCandidate(candidates); unsafe != nil {
		return "", operationError(
			"unsafe_message_source",
			fmt.Sprintf("Mail store candidate %s is unsafe at %s: %s", unsafe.name, unsafe.path, unsafe.detail),
		)
	}
	return "", unavailableGenerationError(mailRoot, candidates)
}

func chooseSupportedWithNewer(mailRoot string, candidates []storeGenerationCandidate, supported storeGenerationCandidate) (string, error) {
	active, detail := readActiveGeneration(mailRoot)
	if active == supported.name {
		return supported.path, nil
	}
	if active != "" && activeGenerationIsNewer(candidates, active) {
		return "", unsupportedGenerationError(candidates, active)
	}
	return "", ambiguousGenerationError(mailRoot, candidates, detail)
}

func chooseNewerOnly(mailRoot string, candidates []storeGenerationCandidate) (string, error) {
	if unsafe := unsafeNewerCandidate(candidates); unsafe != nil {
		return "", unsafeGenerationError(unsafe)
	}
	if !hasReadableNewerGeneration(candidates) {
		return "", unavailableGenerationError(mailRoot, candidates)
	}
	return "", unsupportedGenerationError(candidates, "")
}

func selectVersionRoot(config Config) (string, error) {
	if config.MailStorePath != "" {
		return validateConfiguredVersionRoot(config.MailStorePath)
	}
	return discoverVersionRoot(config.MailRoot)
}

func inventoryStoreGenerations(mailRoot string) ([]storeGenerationCandidate, error) {
	entries, err := os.ReadDir(mailRoot)
	if err != nil {
		return nil, operationError(
			"mail_store_unavailable",
			fmt.Sprintf("cannot read %s; grant Full Disk Access to the agent host: %v", mailRoot, err),
		)
	}
	var candidates []storeGenerationCandidate
	for _, entry := range entries {
		version, ok := parseVersionDirectoryName(entry.Name())
		if !ok {
			continue
		}
		candidate, err := inspectStoreGeneration(filepath.Join(mailRoot, entry.Name()), version)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(left int, right int) bool {
		if candidates[left].version != candidates[right].version {
			return candidates[left].version > candidates[right].version
		}
		return candidates[left].name < candidates[right].name
	})
	return candidates, nil
}

func parseVersionDirectoryName(name string) (int, bool) {
	if len(name) < 2 || name[0] != 'V' {
		return 0, false
	}
	for _, character := range name[1:] {
		if character < '0' || character > '9' {
			return 0, false
		}
	}
	version, err := strconv.Atoi(name[1:])
	return version, err == nil && version > 0
}

func inspectStoreGeneration(path string, version int) (storeGenerationCandidate, error) {
	candidate := storeGenerationCandidate{
		name: filepath.Base(path), version: version, path: path,
	}
	info, err := os.Lstat(path)
	if err != nil {
		candidate.classification = generationIncomplete
		candidate.detail = fmt.Sprintf("generation directory is unavailable: %v", err)
		return candidate, nil
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		candidate.classification = generationUnsafe
		candidate.detail = "generation path is not a regular directory"
		return candidate, nil
	}
	return inspectStoreGenerationData(candidate)
}

func inspectStoreGenerationData(candidate storeGenerationCandidate) (storeGenerationCandidate, error) {
	mailDataPath := filepath.Join(candidate.path, "MailData")
	mailDataInfo, err := os.Lstat(mailDataPath)
	if os.IsNotExist(err) {
		candidate.classification = generationIncomplete
		candidate.detail = "MailData directory is missing"
		return candidate, nil
	}
	if err != nil {
		candidate.classification = generationIncomplete
		candidate.detail = fmt.Sprintf("MailData directory is unavailable: %v", err)
		return candidate, nil
	}
	if mailDataInfo.Mode()&os.ModeSymlink != 0 || !mailDataInfo.IsDir() {
		candidate.classification = generationUnsafe
		candidate.detail = "MailData path is not a regular directory"
		return candidate, nil
	}
	return classifyEnvelopeIndex(candidate, filepath.Join(mailDataPath, envelopeIndexName))
}

func classifyEnvelopeIndex(candidate storeGenerationCandidate, path string) (storeGenerationCandidate, error) {
	indexInfo, err := os.Lstat(path)
	if os.IsNotExist(err) {
		candidate.classification = generationIncomplete
		candidate.detail = "Envelope Index is missing"
		return candidate, nil
	}
	if err != nil {
		candidate.classification = generationIncomplete
		candidate.detail = fmt.Sprintf("Envelope Index is unavailable: %v", err)
		return candidate, nil
	}
	if indexInfo.Mode()&os.ModeSymlink != 0 || !indexInfo.Mode().IsRegular() {
		candidate.classification = generationUnsafe
		candidate.detail = "Envelope Index is not a regular file"
		return candidate, nil
	}
	candidate.classification = generationUnsupported
	candidate.detail = "regular Envelope Index"
	if candidate.name == supportedStoreDirectoryName && candidate.version == supportedStoreDirectoryValue {
		candidate.classification = generationSupported
	}
	return candidate, nil
}

func validateConfiguredVersionRoot(path string) (string, error) {
	version, ok := parseVersionDirectoryName(filepath.Base(path))
	if !ok || filepath.Base(path) != supportedStoreDirectoryName || version != supportedStoreDirectoryValue {
		return "", operationError(
			"unsupported_mail_store_schema",
			fmt.Sprintf("configured MailStorePath %q is not the supported Mail store generation V10", path),
		)
	}
	candidate, err := inspectStoreGeneration(path, version)
	if err != nil {
		return "", err
	}
	switch candidate.classification {
	case generationSupported:
		return candidate.path, nil
	case generationUnsafe:
		return "", operationError(
			"unsafe_message_source",
			fmt.Sprintf("configured MailStorePath %q is unsafe: %s", path, candidate.detail),
		)
	default:
		return "", operationError(
			"mail_store_unavailable",
			fmt.Sprintf("configured MailStorePath %q is unavailable: %s", path, candidate.detail),
		)
	}
}

func supportedGenerationCandidates(candidates []storeGenerationCandidate) []storeGenerationCandidate {
	var supported []storeGenerationCandidate
	for _, candidate := range candidates {
		if candidate.classification == generationSupported {
			supported = append(supported, candidate)
		}
	}
	return supported
}

func hasNewerGeneration(candidates []storeGenerationCandidate) bool {
	for _, candidate := range candidates {
		if candidate.version > supportedStoreDirectoryValue {
			return true
		}
	}
	return false
}

func hasReadableNewerGeneration(candidates []storeGenerationCandidate) bool {
	for _, candidate := range candidates {
		if candidate.version > supportedStoreDirectoryValue && candidate.classification == generationUnsupported {
			return true
		}
	}
	return false
}

func hasSameGenerationPeer(candidates []storeGenerationCandidate) bool {
	for _, candidate := range candidates {
		if candidate.version == supportedStoreDirectoryValue && candidate.name != supportedStoreDirectoryName {
			return true
		}
	}
	return false
}

func unsafeSupportedCandidate(candidates []storeGenerationCandidate) *storeGenerationCandidate {
	for index := range candidates {
		candidate := &candidates[index]
		if candidate.version == supportedStoreDirectoryValue && candidate.name == supportedStoreDirectoryName && candidate.classification == generationUnsafe {
			return candidate
		}
	}
	return nil
}

func unsafeNewerCandidate(candidates []storeGenerationCandidate) *storeGenerationCandidate {
	for index := range candidates {
		candidate := &candidates[index]
		if candidate.version > supportedStoreDirectoryValue && candidate.classification == generationUnsafe {
			return candidate
		}
	}
	return nil
}

func activeGenerationIsNewer(candidates []storeGenerationCandidate, active string) bool {
	for _, candidate := range candidates {
		if candidate.name == active {
			return candidate.version > supportedStoreDirectoryValue
		}
	}
	return false
}

func generationCandidatesMessage(candidates []storeGenerationCandidate) string {
	parts := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		parts = append(parts, fmt.Sprintf("%s=%s (%s: %s)", candidate.name, candidate.path, candidate.classification, candidate.detail))
	}
	return strings.Join(parts, "; ")
}

func ambiguousGenerationError(mailRoot string, candidates []storeGenerationCandidate, activeDetail string) error {
	return operationError(
		"ambiguous_mail_store_generation",
		fmt.Sprintf(
			"automatic Mail store selection under %s is ambiguous; candidates: %s; %s; configure MailStorePath with an exact supported V10 directory after verifying the active generation",
			mailRoot, generationCandidatesMessage(candidates), activeDetail,
		),
	)
}

func unsupportedGenerationError(candidates []storeGenerationCandidate, active string) error {
	activeDetail := ""
	if active != "" {
		activeDetail = fmt.Sprintf(" PersistenceInfo.plist identifies active generation %s.", active)
	}
	return operationError(
		"unsupported_mail_store_schema",
		fmt.Sprintf(
			"Mail store contains an unsupported newer generation; supported generation is V10.%s Candidates: %s. Upgrade MailCLI for the newer generation or configure an exact supported V10 directory.",
			activeDetail, generationCandidatesMessage(candidates),
		),
	)
}

func unavailableGenerationError(mailRoot string, candidates []storeGenerationCandidate) error {
	return operationError(
		"mail_store_unavailable",
		fmt.Sprintf("no supported V10 Mail store is available under %s; candidates: %s", mailRoot, generationCandidatesMessage(candidates)),
	)
}

func unsafeGenerationError(candidate *storeGenerationCandidate) error {
	return operationError(
		"unsafe_message_source",
		fmt.Sprintf("Mail store candidate %s is unsafe at %s: %s", candidate.name, candidate.path, candidate.detail),
	)
}

func readActiveGeneration(mailRoot string) (string, string) {
	path := filepath.Join(mailRoot, persistenceInfoName)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "", "PersistenceInfo.plist is absent"
	}
	if err != nil {
		return "", fmt.Sprintf("PersistenceInfo.plist is unavailable: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", "PersistenceInfo.plist is not a regular file"
	}
	data, err := readPersistenceInfoFile(path, info)
	if err != nil {
		return "", fmt.Sprintf("PersistenceInfo.plist is invalid: %v", err)
	}
	active, err := parsePersistenceInfoXML(data)
	if err != nil {
		return "", fmt.Sprintf("PersistenceInfo.plist is invalid: %v", err)
	}
	if _, ok := parseVersionDirectoryName(active); !ok {
		return "", fmt.Sprintf("PersistenceInfo.plist is invalid: LastUsedVersionDirectoryName %q is not a numeric generation", active)
	}
	return active, fmt.Sprintf("PersistenceInfo.plist identifies active generation %s", active)
}

func readPersistenceInfoFile(path string, expected os.FileInfo) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if !os.SameFile(expected, opened) {
		return nil, errors.Join(fmt.Errorf("file changed while reading"), file.Close())
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maximumPersistenceInfoBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > maximumPersistenceInfoBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", maximumPersistenceInfoBytes)
	}
	return data, nil
}

func parsePersistenceInfoXML(source []byte) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(source))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return "", fmt.Errorf("plist root is missing")
		}
		if err != nil {
			return "", err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local != "plist" {
			return "", fmt.Errorf("plist root is %q, want plist", start.Name.Local)
		}
		active, err := parsePersistenceInfoPlist(decoder)
		if err != nil {
			return "", err
		}
		if err := requirePersistenceInfoEOF(decoder); err != nil {
			return "", err
		}
		return active, nil
	}
}

func parsePersistenceInfoPlist(decoder *xml.Decoder) (string, error) {
	start, err := nextPersistenceStart(decoder)
	if err != nil {
		return "", err
	}
	if start.Name.Local != "dict" {
		return "", fmt.Errorf("plist root must contain one dict object")
	}
	active, err := parsePersistenceInfoDict(decoder)
	if err != nil {
		return "", err
	}
	if err := consumePersistenceInfoEnd(decoder, "plist"); err != nil {
		return "", err
	}
	return active, nil
}

func parsePersistenceInfoDict(decoder *xml.Decoder) (string, error) {
	var active string
	seenActive := false
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", err
		}
		switch value := token.(type) {
		case xml.CharData:
			if strings.TrimSpace(string(value)) != "" {
				return "", fmt.Errorf("plist dict contains unexpected text")
			}
		case xml.StartElement:
			key, entryValue, isActive, err := decodePersistenceInfoEntry(decoder, value)
			if err != nil {
				return "", err
			}
			if !isActive {
				continue
			}
			if key != "LastUsedVersionDirectoryName" || seenActive {
				return "", fmt.Errorf("plist contains duplicate LastUsedVersionDirectoryName")
			}
			active, seenActive = entryValue, true
		case xml.EndElement:
			if value.Name.Local != "dict" {
				return "", fmt.Errorf("plist dict closed by </%s>", value.Name.Local)
			}
			return finishPersistenceInfoDict(active, seenActive)
		case xml.Comment, xml.ProcInst:
			continue
		default:
			return "", fmt.Errorf("plist dict contains unexpected token")
		}
	}
}

func finishPersistenceInfoDict(active string, seenActive bool) (string, error) {
	if !seenActive || strings.TrimSpace(active) == "" {
		return "", fmt.Errorf("LastUsedVersionDirectoryName is missing")
	}
	return strings.TrimSpace(active), nil
}

func decodePersistenceInfoEntry(decoder *xml.Decoder, keyStart xml.StartElement) (string, string, bool, error) {
	if keyStart.Name.Local != "key" {
		return "", "", false, fmt.Errorf("plist dict contains %q instead of key", keyStart.Name.Local)
	}
	var key string
	if err := decoder.DecodeElement(&key, &keyStart); err != nil {
		return "", "", false, err
	}
	valueStart, err := nextPersistenceStart(decoder)
	if err != nil {
		return "", "", false, err
	}
	if key != "LastUsedVersionDirectoryName" {
		return key, "", false, decoder.Skip()
	}
	if valueStart.Name.Local != "string" {
		return "", "", false, fmt.Errorf("LastUsedVersionDirectoryName is not a string")
	}
	var active string
	if err := decoder.DecodeElement(&active, &valueStart); err != nil {
		return "", "", false, err
	}
	return key, strings.TrimSpace(active), true, nil
}

func nextPersistenceStart(decoder *xml.Decoder) (xml.StartElement, error) {
	for {
		token, err := decoder.Token()
		if err != nil {
			return xml.StartElement{}, err
		}
		switch value := token.(type) {
		case xml.CharData:
			if strings.TrimSpace(string(value)) == "" {
				continue
			}
		case xml.StartElement:
			return value, nil
		case xml.Comment, xml.ProcInst:
			continue
		default:
			return xml.StartElement{}, fmt.Errorf("plist value is missing")
		}
	}
}

func consumePersistenceInfoEnd(decoder *xml.Decoder, name string) error {
	for {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch value := token.(type) {
		case xml.CharData:
			if strings.TrimSpace(string(value)) == "" {
				continue
			}
		case xml.Comment, xml.ProcInst, xml.Directive:
			continue
		case xml.EndElement:
			if value.Name.Local != name {
				return fmt.Errorf("plist root closed by </%s>", value.Name.Local)
			}
			return nil
		default:
			return fmt.Errorf("plist root closed by unexpected token")
		}
	}
}

func requirePersistenceInfoEOF(decoder *xml.Decoder) error {
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch value := token.(type) {
		case xml.CharData:
			if strings.TrimSpace(string(value)) == "" {
				continue
			}
		case xml.Comment, xml.ProcInst:
			continue
		default:
			return fmt.Errorf("plist contains trailing content")
		}
	}
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
