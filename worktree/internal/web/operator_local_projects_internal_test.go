package web

import (
	"encoding/json"
	"testing"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

func TestLocalConfigurationOwnerSelector(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		ok   bool
	}{
		{"local", `{"project_id":"selected"}`, true},
		{"cloud runner", `{"project_id":"selected","runner_id":"another-runner"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeLocalOwnerArguments(operatortool.LocalProjectConfiguration, json.RawMessage(test.raw), true)
			if (err == nil) != test.ok {
				t.Fatalf("selector acceptance=%t, want %t: %v", err == nil, test.ok, err)
			}
		})
	}
}
