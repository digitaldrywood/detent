package operatortool

import (
	"encoding/json"
	"slices"
)

func WithOrganizationSelector(definition Definition) (Definition, error) {
	if definition.Name == OrganizationList || definition.Name == ConnectionInfo {
		return definition, nil
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
		return Definition{}, err
	}
	if schema == nil {
		return Definition{}, ErrInvalidArguments
	}
	var properties map[string]json.RawMessage
	if err := json.Unmarshal(schema["properties"], &properties); err != nil {
		return Definition{}, err
	}
	if properties == nil {
		properties = map[string]json.RawMessage{}
	}
	properties["organization_id"] = json.RawMessage(`{"type":"string","minLength":1,"description":"Target organization from organization_list"}`)
	var err error
	schema["properties"], err = json.Marshal(properties)
	if err != nil {
		return Definition{}, err
	}
	var required []string
	if len(schema["required"]) > 0 {
		if err := json.Unmarshal(schema["required"], &required); err != nil {
			return Definition{}, err
		}
	}
	if !slices.Contains(required, "organization_id") {
		required = append(required, "organization_id")
	}
	schema["required"], err = json.Marshal(required)
	if err != nil {
		return Definition{}, err
	}
	definition.InputSchema, err = json.Marshal(schema)
	return definition, err
}
