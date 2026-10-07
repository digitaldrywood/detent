package hubserver

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestOperatorRevisionSchemaContract(t *testing.T) {
	targets := map[string]any{
		operatortool.MoveItem:                         &operatortool.MoveItemArguments{},
		operatortool.SetPriority:                      &operatortool.SetPriorityArguments{},
		operatortool.EditItem:                         &operatortool.WorkArguments{},
		operatortool.EditComment:                      &operatortool.WorkArguments{},
		operatortool.SetDependency:                    &operatortool.WorkArguments{},
		operatortool.ArchiveItem:                      &operatortool.WorkArguments{},
		operatortool.RestoreItem:                      &operatortool.WorkArguments{},
		operatortool.ReviewChange:                     &operatortool.ChangeArguments{},
		operatortool.OrganizationProjectRankUpdate:    &operatoradmin.Input{},
		operatortool.UpdateApply:                      &runnerUpdateChange{},
		operatortool.MarkUrgentRunnerUpdate:           &urgentRunnerUpdateChange{},
		operatortool.UpdateRunnerRouting:              &runnerRoutingRequest{},
		operatortool.UpdateFleetRunner:                &runnerauth.RoutingChange{},
		operatortool.UpdateRunnerHost:                 &runnerauth.HostChange{},
		operatortool.UpdateFleetHost:                  &runnerauth.HostChange{},
		operatortool.UpdateRunnerCapacity:             &runnerCapacityChange{},
		operatortool.UpdateOrganizationModelSelection: &operatortool.ModelSelectionInput{},
		operatortool.UpdateProjectModelSelection:      &operatortool.ModelSelectionInput{},
		"update_project_integration":                  &operatortool.IntegrationInput{},
		"bind_native_repository":                      &operatortool.RepositoryInput{},
		"start_git_hub_import":                        &operatortool.ImportStartInput{},
		"advance_git_hub_import":                      &operatortool.ImportAdvanceInput{},
		"patch_project_action":                        &projectActionPatch{},
		"create_project_action_run":                   &workspaceToolRequest{},
	}
	type schema struct {
		Type       json.RawMessage   `json:"type"`
		Properties map[string]schema `json:"properties"`
	}
	seen := make(map[string]bool)
	for _, definition := range operatortool.Registry() {
		var input schema
		if err := json.Unmarshal(definition.InputSchema, &input); err != nil {
			t.Fatal(err)
		}
		var visit func(schema, string)
		visit = func(input schema, path string) {
			for name, property := range input.Properties {
				if name != "expected_revision" {
					visit(property, path+name+".")
					continue
				}
				seen[definition.Name] = true
				t.Run(definition.Name+"/"+path+name, func(t *testing.T) {
					target, ok := targets[definition.Name]
					if !ok {
						t.Fatal("revision-taking tool has no handler contract case")
					}
					var revision any
					var revisionType string
					if err := json.Unmarshal(property.Type, &revisionType); err != nil {
						t.Fatal(err)
					}
					switch revisionType {
					case "string":
						revision = "16"
					case "integer":
						revision = 16
					default:
						t.Fatalf("unsupported revision type %q", revisionType)
					}
					raw, err := json.Marshal(map[string]any{"expected_revision": revision})
					if err != nil {
						t.Fatal(err)
					}
					if err := operatortool.DecodeArguments(raw, target); err != nil {
						t.Fatalf("handler rejects schema-declared %s revision: %v", revisionType, err)
					}
					if got := reflect.ValueOf(target).Elem().FieldByName("ExpectedRevision").Int(); got != 16 {
						t.Fatalf("decoded revision=%d, want 16", got)
					}
					if definition.Name == operatortool.MoveItem || operatortool.IsWorkTool(definition.Name) {
						fields := map[string]any{"project_id": "project", "identifier": "item", "expected_revision": revision}
						switch definition.Name {
						case operatortool.MoveItem:
							fields["request_id"], fields["target_state"] = "move", "Todo"
						case operatortool.EditItem:
							fields["title"] = "Edited"
						case operatortool.EditComment:
							fields["comment_id"], fields["body"] = "comment", "Edited comment"
						case operatortool.SetDependency:
							fields["related"], fields["operation"] = "blocker", "add"
						}
						raw, err := json.Marshal(fields)
						if err != nil {
							t.Fatal(err)
						}
						if definition.Name == operatortool.MoveItem {
							_, err = operatortool.DecodeNativeMoveItem(raw)
						} else {
							_, err = operatortool.DecodeWorkArguments(definition.Name, raw)
						}
						if err != nil {
							t.Fatalf("handler rejects schema-declared revision: %v", err)
						}
						if definition.Name == operatortool.EditItem || definition.Name == operatortool.SetDependency {
							for _, value := range []json.RawMessage{json.RawMessage(`16`), json.RawMessage(`16.0`)} {
								fields["expected_revision"] = value
								compatibilityRaw, err := json.Marshal(fields)
								if err != nil {
									t.Fatal(err)
								}
								request, err := operatortool.DecodeWorkArguments(definition.Name, compatibilityRaw)
								if err != nil || request.ExpectedRevision != 16 {
									t.Fatalf("numeric compatibility revision=%d error=%v, want 16", request.ExpectedRevision, err)
								}
							}
						}
					}
				})
			}
		}
		visit(input, "")
	}
	for name := range targets {
		if !seen[name] {
			t.Errorf("handler contract case %s has no advertised expected_revision", name)
		}
	}
}
