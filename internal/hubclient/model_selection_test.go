package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCloudModelSelectionDelivery(t *testing.T) {
	for _, test := range []struct {
		name, model, source  string
		override             config.ModelSelection
		status               int
		transportError       error
		body                 string
		canceled             bool
		expired              bool
		wantError, wantRetry bool
	}{
		{name: "organization default", model: "gpt-6.1-sol", source: "organization"},
		{name: "project override", model: "gpt-6-astra", source: "project", override: config.ModelSelection{NormalModel: new("gpt-6-astra")}},
		{name: "transport deadline", transportError: context.DeadlineExceeded, wantError: true, wantRetry: true},
		{name: "interrupted response", transportError: io.ErrUnexpectedEOF, wantError: true, wantRetry: true},
		{name: "unavailable server", status: http.StatusServiceUnavailable, wantError: true, wantRetry: true},
		{name: "unauthorized", status: http.StatusUnauthorized, wantError: true},
		{name: "forbidden", status: http.StatusForbidden, wantError: true},
		{name: "missing project", status: http.StatusNotFound, wantError: true},
		{name: "malformed response", body: "{", wantError: true},
		{name: "invalid model selection", body: `{"model_selection":"invalid"}`, wantError: true},
		{name: "oversized response", body: strings.Repeat(" ", maxResponseBytes+1), wantError: true},
		{name: "transport cancellation", transportError: context.Canceled, wantError: true},
		{name: "canceled parent", canceled: true, transportError: context.Canceled, wantError: true},
		{name: "expired parent", expired: true, transportError: context.DeadlineExceeded, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			selection := config.ResolveCloudModelSelection(config.ModelSelection{Preset: new("sol_first"), NormalModel: new("gpt-6.1-sol")}, test.override)
			raw, err := json.Marshal(selection)
			if err != nil {
				t.Fatal(err)
			}
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.status != 0 {
					w.WriteHeader(test.status)
					return
				}
				if test.body != "" {
					_, _ = io.WriteString(w, test.body)
					return
				}
				if r.URL.Path == "/api/v2/organizations/org_test/projects/prj_test/policy" {
					w.WriteHeader(http.StatusConflict)
					_ = json.NewEncoder(w).Encode(map[string]string{"code": "policy_mismatch", "message": "No approved repository policy"})
					return
				}
				if r.URL.Path != "/api/v2/organizations/org_test/projects/prj_test" {
					t.Errorf("unexpected request %s", r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(tracker.NativeProject{ModelSelection: raw})
			})
			client, err := New(Config{URL: "https://example.invalid", TokenSource: func() string { return "worker" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				if test.transportError != nil {
					return nil, test.transportError
				}
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, r)
				return recorder.Result(), nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_test", "prj_test")
			if err != nil {
				t.Fatal(err)
			}
			workflow := config.Workflow{Config: config.Default(), Definition: config.ProjectDefinition{Layout: config.ProjectDefinitionSplit}, Prompt: "Keep instructions."}
			workflow.Config.Agents.ModelSelection = config.ModelSelection{Preset: new("sol_first"), NormalModel: new("legacy-project")}
			ctx := t.Context()
			if test.canceled {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			if test.expired {
				expired, cancel := context.WithDeadline(ctx, time.Unix(1, 0))
				defer cancel()
				ctx = expired
			}
			resolved, err := native.ResolveProjectWorkflow(ctx, workflow, nil)
			if (err != nil) != test.wantError || connector.IsRetryable(err) != test.wantRetry {
				t.Fatalf("ResolveProjectWorkflow error = %v, want error=%v retry=%v", err, test.wantError, test.wantRetry)
			}
			if test.wantRetry && !errors.Is(err, ErrUnavailable) {
				t.Fatalf("Hub unavailability cause lost: %v", err)
			}
			if test.transportError != nil && !errors.Is(err, test.transportError) {
				t.Fatalf("transport cause lost: %v", err)
			}
			if test.wantError {
				return
			}
			resolved.Config = resolved.Config.WithAgentDefaults(config.Agents{ModelSelection: config.ModelSelection{Preset: new("sol_first"), NormalModel: new("legacy-global")}}, config.AgentBudgetDefaults{})
			if resolved.Config.EffectiveModelSelection().Model("normal") != test.model || resolved.Config.EffectiveModelSelection().Sources["normal_model"] != test.source || resolved.Prompt != workflow.Prompt {
				t.Fatalf("resolved workflow = %+v", resolved)
			}
		})
	}
}
