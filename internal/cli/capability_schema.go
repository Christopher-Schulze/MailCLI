package cli

import "encoding/json"

type capabilitySchemaReference struct {
	ID      string   `json:"id"`
	Resolve []string `json:"resolve"`
}

func referenceCapabilitySchemas(commands []commandCapability) error {
	for i := range commands {
		command := &commands[i]
		filename := command.ID + ".json"
		if _, err := embeddedSchemaCommandID("schemas/"+filename, filename, command.ID, command.Schema); err != nil {
			return err
		}
		var identity struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(command.Schema, &identity); err != nil {
			return invalidEmbeddedCommandSchema("schemas/"+filename, err)
		}
		command.SchemaRef = &capabilitySchemaReference{
			ID:      identity.ID,
			Resolve: []string{"mailcli", "capabilities", "--for", command.ID, "--schemas", "--json"},
		}
		command.Schema = nil
	}
	return nil
}

// attachOutputSchemas publishes output trees for an explicit --outputs or
// --output-schema request, keeping ordinary discovery compact.
func attachOutputSchemas(commands []commandCapability) error {
	for i := range commands {
		command := &commands[i]
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(command.Schema, &fields); err != nil {
			return invalidEmbeddedCommandSchema("schemas/"+command.ID+".json", err)
		}
		output, err := marshalCLIJSON(outputSchemaForCommand(command.ID))
		if err != nil {
			return err
		}
		fields["output"] = output
		schema, err := marshalCLIJSON(fields)
		if err != nil {
			return err
		}
		command.Schema = schema
	}
	return nil
}

func schemasIncludeOutput(commands []commandCapability) bool {
	for _, command := range commands {
		var fields map[string]json.RawMessage
		if json.Unmarshal(command.Schema, &fields) == nil {
			if _, ok := fields["output"]; ok {
				return true
			}
		}
	}
	return false
}
