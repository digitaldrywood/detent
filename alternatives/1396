package hubclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// The workspace capability report on the native machine endpoints (decisions
// section 18.1). The hub's claim gate reads what a runner reported from the row
// it stamps the heartbeat on, so both register and heartbeat must carry it, and
// a runner that reports nothing must send neither field: the hub leaves a
// stored report alone when a request omits it.

// recordNativeMachineCalls answers the native machine endpoints and keeps the
// decoded body of every call.
func recordNativeMachineCalls(t *testing.T) (*NativeClient, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test" }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	return native, &bodies
}

func TestNativeMachineCallsCarryTheWorkspaceReport(t *testing.T) {
	t.Parallel()
	machine := Machine{
		ID: tracker.MachineID("machine_1"), Hostname: "host", DisplayName: "host", Capacity: 2, Version: "test",
	}
	reporting := machine
	reporting.WorkspaceCapabilities = workspacesession.Capabilities{Files: true}
	reporting.WorkspaceIsolation = workspacesession.IsolationUser

	tests := []struct {
		name              string
		machine           Machine
		call              func(*NativeClient, Machine) error
		wantCapabilities  map[string]any
		wantIsolation     any
		wantFieldsPresent bool
	}{
		{
			name: "register reports files", machine: reporting,
			call:             func(c *NativeClient, m Machine) error { return c.RegisterMachine(t.Context(), m) },
			wantCapabilities: map[string]any{"terminal": false, "files": true, "diff": false, "preview": false},
			wantIsolation:    workspacesession.IsolationUser, wantFieldsPresent: true,
		},
		{
			name: "heartbeat reports files", machine: reporting,
			call:             func(c *NativeClient, m Machine) error { return c.HeartbeatMachine(t.Context(), m) },
			wantCapabilities: map[string]any{"terminal": false, "files": true, "diff": false, "preview": false},
			wantIsolation:    workspacesession.IsolationUser, wantFieldsPresent: true,
		},
		{
			name: "register without a report omits both fields", machine: machine,
			call: func(c *NativeClient, m Machine) error { return c.RegisterMachine(t.Context(), m) },
		},
		{
			name: "heartbeat without a report omits both fields", machine: machine,
			call: func(c *NativeClient, m Machine) error { return c.HeartbeatMachine(t.Context(), m) },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client, bodies := recordNativeMachineCalls(t)
			if err := test.call(client, test.machine); err != nil {
				t.Fatalf("call returned %v", err)
			}
			if len(*bodies) != 1 {
				t.Fatalf("recorded %d request bodies, want 1", len(*bodies))
			}
			body := (*bodies)[0]
			capabilities, hasCapabilities := body["workspace_capabilities"]
			isolation, hasIsolation := body["workspace_isolation"]
			if hasCapabilities != test.wantFieldsPresent || hasIsolation != test.wantFieldsPresent {
				t.Fatalf("workspace_capabilities present = %t, workspace_isolation present = %t, want both %t",
					hasCapabilities, hasIsolation, test.wantFieldsPresent)
			}
			if !test.wantFieldsPresent {
				return
			}
			decoded, ok := capabilities.(map[string]any)
			if !ok {
				t.Fatalf("workspace_capabilities = %#v, want an object", capabilities)
			}
			for name, want := range test.wantCapabilities {
				if decoded[name] != want {
					t.Errorf("workspace_capabilities[%q] = %#v, want %#v", name, decoded[name], want)
				}
			}
			if isolation != test.wantIsolation {
				t.Errorf("workspace_isolation = %#v, want %#v", isolation, test.wantIsolation)
			}
		})
	}
}

// A machine struct is serialised whole by the v1 register endpoint, which
// rejects unknown fields, so the workspace report must never leak into it.
func TestMachineJSONOmitsTheWorkspaceReport(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(Machine{
		ID: tracker.MachineID("machine_1"), Hostname: "host", Capacity: 1, Version: "test",
		WorkspaceCapabilities: workspacesession.Capabilities{Files: true},
		WorkspaceIsolation:    workspacesession.IsolationUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	if _, found := body["workspace_capabilities"]; found {
		t.Errorf("Machine JSON = %s, want no workspace_capabilities", encoded)
	}
	if _, found := body["workspace_isolation"]; found {
		t.Errorf("Machine JSON = %s, want no workspace_isolation", encoded)
	}
}
