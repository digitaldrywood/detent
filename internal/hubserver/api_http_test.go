package hubserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestDecodeAPIJSONCompatibility(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		scope   apiScope
		body    string
		wantErr bool
		project string
	}{
		{name: "worker future heartbeat field", scope: apiScopeWorker, body: `{"version":"develop-040e7195c","future_observation":true}`},
		{name: "worker newer project configuration", scope: apiScopeWorker, body: `{"version":"develop-040e7195c","project_configuration":{"project_id":"project","policy_mismatch":false}}`, project: "project"},
		{name: "operator unknown field", scope: apiScopeOperator, body: `{"runner_version":"test"}`, wantErr: true},
		{name: "admin nested unknown field", scope: apiScopeAdmin, body: `{"project_configuration":{"project_name":"project"}}`, wantErr: true},
		{name: "unauthenticated unknown field", body: `{"future_observation":true}`, wantErr: true},
		{name: "worker invalid known field", scope: apiScopeWorker, body: `{"version":42}`, wantErr: true},
		{name: "worker multiple values", scope: apiScopeWorker, body: `{"version":"test"} {}`, wantErr: true},
		{name: "worker malformed JSON", scope: apiScopeWorker, body: `{"version":`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.body)), httptest.NewRecorder())
			if test.scope != "" {
				c.Set("hub_api_credential", apiCredential{Scope: test.scope})
			}
			var heartbeat struct {
				Version              string `json:"version"`
				ProjectConfiguration struct {
					ProjectID string `json:"project_id"`
				} `json:"project_configuration"`
			}
			err := decodeAPIJSON(c, &heartbeat)
			if (err != nil) != test.wantErr {
				t.Fatalf("decode error = %v, want error %t", err, test.wantErr)
			}
			if !test.wantErr && heartbeat.Version != "develop-040e7195c" {
				t.Fatalf("version = %q", heartbeat.Version)
			}
			if !test.wantErr && heartbeat.ProjectConfiguration.ProjectID != test.project {
				t.Fatalf("project ID = %q", heartbeat.ProjectConfiguration.ProjectID)
			}
		})
	}
}
