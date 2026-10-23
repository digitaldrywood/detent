package operatortool

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestModelSelectionArgumentBounds(t *testing.T) {
	for _, test := range []struct {
		name, input string
		invalid     bool
	}{
		{"inherit", `{"expected_revision":"0","selection":null}`, false},
		{"partial", `{"expected_revision":"1","selection":{"normal_model":"chosen","levels":{"normal":{"effort":"high"}},"stages":{"plan":{"effort":"low"}},"rules":[{"name":"complex","level":"complex","selector":{"and":[{"labels":{"include":["complexity:complex"]}}]}}]}}`, false},
		{"missing selection", `{"expected_revision":"0"}`, true},
		{"missing revision", `{"selection":null}`, true},
		{"negative revision", `{"expected_revision":"-1","selection":null}`, true},
		{"signed revision", `{"expected_revision":"+1","selection":null}`, true},
		{"unknown selection", `{"expected_revision":"0","selection":{"token":"secret"}}`, true},
		{"unknown level", `{"expected_revision":"0","selection":{"levels":{"normal":{"token":"secret"}}}}`, true},
		{"oversized model", `{"expected_revision":"0","selection":{"normal_model":"` + strings.Repeat("m", 257) + `"}}`, true},
		{"oversized map", `{"expected_revision":"0","selection":{"levels":{` + func() string {
			fields := []string{}
			for i := range 201 {
				key, _ := json.Marshal(string(rune('a' + i)))
				fields = append(fields, string(key)+`:{}`)
			}
			return strings.Join(fields, ",")
		}() + `}}}`, true},
		{"oversized rules", `{"expected_revision":"0","selection":{"rules":[` + strings.TrimSuffix(strings.Repeat(`{"name":"rule","level":"normal"},`, 201), ",") + `]}}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var request ProjectRequest[ModelSelectionInput]
			raw := json.RawMessage(`{"project_id":"p","request_id":"retry","input":` + test.input + `}`)
			if err := DecodeModelSelectionArguments(Call{Name: UpdateProjectModelSelection, Arguments: raw}, &request); (err != nil) != test.invalid {
				t.Fatalf("decode=%v want invalid=%v", err, test.invalid)
			}
		})
	}
	for _, test := range []struct {
		name, tool, raw string
		read, invalid   bool
	}{
		{"organization read", GetOrganizationModelSelection, `{}`, true, false},
		{"project read", GetProjectModelSelection, `{"project_id":"p"}`, true, false},
		{"missing project", GetProjectModelSelection, `{}`, true, true},
		{"organization project selector", GetOrganizationModelSelection, `{"project_id":""}`, true, true},
		{"forged organization", GetOrganizationModelSelection, `{"organization_id":"foreign"}`, true, true},
		{"null root", GetOrganizationModelSelection, `null`, true, true},
		{"organization write", UpdateOrganizationModelSelection, `{"request_id":"retry","input":{"expected_revision":"1","selection":{"enabled":false}}}`, false, false},
		{"organization write project selector", UpdateOrganizationModelSelection, `{"project_id":"","request_id":"retry","input":{"expected_revision":"1","selection":{"enabled":false}}}`, false, true},
		{"oversized retry", UpdateProjectModelSelection, `{"project_id":"p","request_id":"` + strings.Repeat("r", 129) + `","input":{"expected_revision":"1","selection":null}}`, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var target any = &ProjectRequest[ModelSelectionInput]{}
			if test.read {
				target = &ModelSelectionReadRequest{}
			}
			if err := DecodeModelSelectionArguments(Call{Name: test.tool, Arguments: json.RawMessage(test.raw)}, target); (err != nil) != test.invalid {
				t.Fatalf("decode=%v want invalid=%v", err, test.invalid)
			}
		})
	}
}
