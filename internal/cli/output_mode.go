package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ResolveOutputMode consumes global output flags before initialization.
// Command option values and operands following -- remain untouched.
func ResolveOutputMode(args []string, stdout io.Writer, environment string) ([]string, bool, error) {
	normalized, explicit, insertion, err := separateOutputFlags(args)
	jsonOutput := !writerIsTerminal(stdout)
	switch environment {
	case "human":
		jsonOutput = false
	case "json":
		jsonOutput = true
	}
	if explicit != "" {
		jsonOutput = explicit == "json"
	}
	if err != nil {
		return normalized, jsonOutput, err
	}
	if environment != "" && environment != "json" && environment != "human" {
		return normalized, jsonOutput, fmt.Errorf("MAILCLI_OUTPUT must be json or human")
	}
	if jsonOutput {
		normalized = append(normalized[:insertion], append([]string{"--json"}, normalized[insertion:]...)...)
	}
	return normalized, jsonOutput, nil
}

func separateOutputFlags(args []string) ([]string, string, int, error) {
	arity := outputCommandFlagArity(args)
	normalized := make([]string, 0, len(args)+1)
	mode := ""
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			insertion := len(normalized)
			return append(normalized, args[index:]...), mode, insertion, nil
		}
		selected, err := outputFlagMode(argument)
		if err != nil {
			return normalized, mode, len(normalized), err
		}
		if selected != "" {
			if mode != "" && mode != selected {
				return normalized, "json", len(normalized), fmt.Errorf("output flags request conflicting modes")
			}
			mode = selected
			continue
		}
		normalized = append(normalized, argument)
		if strings.HasPrefix(argument, "-") && argument != "-" {
			name, _, hasValue := flagArgument(argument)
			if arity[name] && !hasValue && index+1 < len(args) {
				index++
				normalized = append(normalized, args[index])
			}
		}
	}
	return normalized, mode, len(normalized), nil
}

func outputCommandFlagArity(args []string) map[string]bool {
	lookup := make([]string, 0, len(args))
	for _, argument := range args {
		if selected, err := outputFlagMode(argument); selected == "" || err != nil {
			lookup = append(lookup, argument)
		}
	}
	if contract, _ := commandContractForArgs(lookup); contract != nil {
		arity, _ := referenceGlobalJSONFlagArity(contract)
		return arity
	}
	return nil
}

func outputFlagMode(argument string) (string, error) {
	flag, value, hasValue := strings.Cut(argument, "=")
	if flag != "--json" && flag != "--human" {
		return "", nil
	}
	enabled := true
	if hasValue {
		var err error
		enabled, err = strconv.ParseBool(value)
		if err != nil {
			return "", fmt.Errorf("%s requires a boolean value", flag)
		}
	}
	if (flag == "--json") == enabled {
		return "json", nil
	}
	return "human", nil
}
