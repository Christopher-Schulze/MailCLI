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
