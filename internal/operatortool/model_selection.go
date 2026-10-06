package operatortool

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	GetOrganizationModelSelection    = "get_organization_model_selection"
	UpdateOrganizationModelSelection = "update_organization_model_selection"
	GetProjectModelSelection         = "get_project_model_selection"
	UpdateProjectModelSelection      = "update_project_model_selection"
)

type ModelSelectionReadRequest struct {
	ProjectID string `json:"project_id,omitempty"`
}

type ModelSelectionInput struct {
	ExpectedRevision tracker.Revision       `json:"expected_revision,string"`
	Selection        *config.ModelSelection `json:"selection"`
}

func IsModelSelection(name string) bool {
	return name == GetOrganizationModelSelection || name == UpdateOrganizationModelSelection || name == GetProjectModelSelection || name == UpdateProjectModelSelection
}

func IsOrganizationModelSelection(name string) bool {
	return name == GetOrganizationModelSelection || name == UpdateOrganizationModelSelection
}

func ModelSelectionCatalog() []Definition {
	definitions := []Definition{}
	for _, name := range []string{GetOrganizationModelSelection, UpdateOrganizationModelSelection, GetProjectModelSelection, UpdateProjectModelSelection} {
		read := name == GetOrganizationModelSelection || name == GetProjectModelSelection
		t := reflect.TypeFor[ModelSelectionReadRequest]()
		if !read {
			t = reflect.TypeFor[ProjectRequest[ModelSelectionInput]]()
		}
		schema := modelSelectionSchema(t)
		properties := schema["properties"].(map[string]any)
		if IsOrganizationModelSelection(name) {
			delete(properties, "project_id")
			schema["required"] = []string{}
			if !read {
				schema["required"] = []string{"request_id", "input"}
			}
		} else if read {
			schema["required"] = []string{"project_id"}
		}
		if !read {
			selection := properties["input"].(map[string]any)["properties"].(map[string]any)["selection"].(map[string]any)
			if IsOrganizationModelSelection(name) {
				selection["type"] = "object"
			} else {
				selection["type"] = []string{"object", "null"}
			}
			schema["$defs"] = map[string]any{"model_selection_selector": modelSelectionStructSchema(reflect.TypeFor[selector.Selector]())}
		}
		raw, err := json.Marshal(schema)
		if err != nil {
			panic(err)
		}
		definitions = append(definitions, Definition{Name: name, Description: "Read or update Cloud model selection through the shared application owner. Organization operations require a hosted session; project operations require current native project grants. Updates require expected_revision and request_id; null project selection restores organization inheritance.", InputSchema: raw, Annotations: Annotations{ReadOnly: read, Idempotent: true}, Meta: ToolMetadata{Toolset: "projects"}})
	}
	return definitions
}

func modelSelectionSchema(t reflect.Type) map[string]any {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeFor[selector.Selector]() {
		return map[string]any{"$ref": "#/$defs/model_selection_selector"}
	}
	switch t.Kind() {
	case reflect.Struct:
		return modelSelectionStructSchema(t)
	case reflect.Map:
		return map[string]any{"type": "object", "maxProperties": 200, "propertyNames": map[string]any{"maxLength": 256}, "additionalProperties": modelSelectionSchema(t.Elem())}
	case reflect.Slice:
		return map[string]any{"type": "array", "maxItems": 200, "items": modelSelectionSchema(t.Elem())}
	default:
		return projectSchema(t)
	}
}

func modelSelectionStructSchema(t reflect.Type) map[string]any {
	properties := map[string]any{}
	required := []string{}
	for i := range t.NumField() {
		field := t.Field(i)
		tag := field.Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		child := modelSelectionSchema(field.Type)
		if strings.Contains(tag, ",string") {
			child = map[string]any{"type": "string", "maxLength": 20, "pattern": "^[0-9]+$"}
		}
		if name == "request_id" {
			child["maxLength"] = 128
		}
		properties[name] = child
		if !strings.Contains(tag, ",omitempty") {
			required = append(required, name)
		}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

func DecodeModelSelectionArguments(call Call, target any) error {
	raw := call.Arguments
	if DecodeArguments(raw, target) != nil {
		return ErrInvalidArguments
	}
	var value any
	if json.Unmarshal(raw, &value) != nil || !boundedModelSelection(value, 0) {
		return ErrInvalidArguments
	}
	fields, ok := value.(map[string]any)
	if !ok {
		return ErrInvalidArguments
	}
	if IsOrganizationModelSelection(call.Name) {
		if _, present := fields["project_id"]; present {
			return ErrInvalidArguments
		}
	} else if project, ok := fields["project_id"].(string); !ok || strings.TrimSpace(project) == "" {
		return ErrInvalidArguments
	}
	if request, ok := target.(*ProjectRequest[ModelSelectionInput]); ok {
		input, ok := fields["input"].(map[string]any)
		if !ok || input["expected_revision"] == nil || strings.TrimSpace(request.RequestID) == "" || len(request.RequestID) > 128 || request.Input.ExpectedRevision < 0 {
			return ErrInvalidArguments
		}
		if _, present := input["selection"]; !present {
			return ErrInvalidArguments
		}
		revision, ok := input["expected_revision"].(string)
		if !ok || revision == "" || len(revision) > 20 || strings.IndexFunc(revision, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return ErrInvalidArguments
		}
	}
	return nil
}

func boundedModelSelection(value any, depth int) bool {
	if depth > 32 {
		return false
	}
	switch v := value.(type) {
	case string:
		return len(v) <= 256
	case float64:
		return v >= 0 && v <= 2147483647
	case []any:
		if len(v) > 200 {
			return false
		}
		for _, child := range v {
			if !boundedModelSelection(child, depth+1) {
				return false
			}
		}
	case map[string]any:
		if len(v) > 200 {
			return false
		}
		for key, child := range v {
			if len(key) > 256 || !boundedModelSelection(child, depth+1) {
				return false
			}
		}
	}
	return true
}
