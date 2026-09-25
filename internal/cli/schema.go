package cli

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
)

var (
	schemaPayloadOnce  sync.Once
	schemaPayloadBytes []byte
)

func schemaPayload() []byte {
	schemaPayloadOnce.Do(func() {
		reader, err := gzip.NewReader(strings.NewReader(schemaPayloadGZIP))
		if err != nil {
			return
		}
		decoded, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil {
			return
		}
		schemaPayloadBytes = decoded
	})
	return schemaPayloadBytes
}

func schemaForCommand(id string) json.RawMessage {
	prefix := []byte(`{"id":"` + id + `@v1"`)
	for _, line := range bytes.Split(schemaPayload(), []byte{'\n'}) {
		if bytes.HasPrefix(line, prefix) {
			return json.RawMessage(append([]byte(nil), line...))
		}
	}
	return emptyCommandSchema(id)
}

func emptyCommandSchema(id string) json.RawMessage {
	encoded, err := json.Marshal(id + "@v1")
	if err != nil {
		return json.RawMessage(`{"id":"unknown@v1","version":1,"flags":[],"positional_arguments":[],"constraints":[]}`)
	}
	output := append([]byte(`{"id":`), encoded...)
	output = append(output, `,"version":1,"flags":[],"positional_arguments":[],"constraints":[]}`...)
	return json.RawMessage(output)
}

func resolveCapabilityScope(command, family, scope string) (string, string, error) {
	command = strings.TrimSpace(command)
	family = strings.TrimSpace(family)
	scope = strings.TrimSpace(scope)
	selectors := 0
	if command != "" {
		selectors++
	}
	if family != "" {
		selectors++
	}
	if scope != "" {
		selectors++
	}
	if selectors > 1 {
		return "", "", &commandError{code: "invalid_argument", message: "--command, --family, and --scope are mutually exclusive"}
	}
	if command != "" {
		if !publishedCommandID(command) {
			return "", "", unknownCapabilityScope(command)
		}
		return command, "", nil
	}
	if family != "" {
		if !publishedCommandFamily(family) {
			return "", "", unknownCapabilityScope(family)
		}
		return "", family, nil
	}
	if scope == "" {
		return "", "", nil
	}
	if publishedCommandID(scope) {
		return scope, "", nil
	}
	if publishedCommandFamily(scope) {
		return "", scope, nil
	}
	return "", "", unknownCapabilityScope(scope)
}

func resolveCapabilityCommands(selector string) ([]string, error) {
	values := strings.Split(selector, ",")
	if strings.TrimSpace(selector) == "" {
		return nil, &commandError{code: "invalid_argument", message: "--commands requires at least one command ID"}
	}
	selected := make(map[string]struct{}, len(values))
	for _, value := range values {
		id := strings.TrimSpace(value)
		if id == "" {
			return nil, &commandError{code: "invalid_argument", message: "--commands cannot contain an empty command ID"}
		}
		if _, exists := selected[id]; exists {
			return nil, &commandError{code: "invalid_argument", message: fmt.Sprintf("--commands repeats command ID %q", id)}
		}
		if !publishedCommandID(id) {
			return nil, unknownCapabilityScope(id)
		}
		selected[id] = struct{}{}
	}
	canonical := make([]string, 0, len(selected))
	for _, contract := range commandContracts {
		if !commandIsPublished(contract) {
			continue
		}
		if _, ok := selected[contract.ID]; ok {
			canonical = append(canonical, contract.ID)
		}
	}
	return canonical, nil
}

func publishedCommandID(id string) bool {
	for _, contract := range commandContracts {
		if commandIsPublished(contract) && contract.ID == id {
			return true
		}
	}
	return false
}

func publishedCommandFamily(family string) bool {
	for _, contract := range commandContracts {
		if commandIsPublished(contract) && (contract.ID == family || strings.HasPrefix(contract.ID, family+".")) {
			return true
		}
	}
	return false
}

func unknownCapabilityScope(value string) error {
	return &commandError{code: "invalid_argument", message: fmt.Sprintf("unknown capability scope %q", value)}
}
