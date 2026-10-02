package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerCapacityOwner(t *testing.T) {
	for _, scenario := range []string{"apply and reload", "stale configuration", "different identity", "different selected path", "different machine", "immutable configuration"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "global.yaml")
			identityPath := filepath.Join(root, "identity.json")
			file, err := runnerauth.Initialize(identityPath, "http://127.0.0.1:1")
			if err != nil {
				t.Fatal(err)
			}
			file.Identity.OrganizationID = "org_test"
			file.Identity.ProjectIDs = []tracker.ProjectID{"prj_test"}
			file.Identity.Operations = []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat}
			file.Identity.ExpiresAt = time.Now().Add(time.Hour)
			if err := runnerauth.Save(identityPath, file); err != nil {
				t.Fatal(err)
			}
			cfg, err := globalconfig.DefaultAt(path, globalconfig.WithProjectPathLiterals())
			if err != nil {
				t.Fatal(err)
			}
			providerPath := filepath.Join(root, "provider.json")
			provider := []byte(`{"external_producer":"private-provider-config","max_concurrent":2}`)
			if err := os.WriteFile(providerPath, provider, 0600); err != nil {
				t.Fatal(err)
			}
			cfg.Global.MaxConcurrentAgents = 2
			cfg.Client = globalconfig.HubClient{URL: file.HubURL, IdentityFile: identityPath, OrganizationID: "org_test", NativeProjects: map[string]string{"native": "prj_test"}, Capacity: 2, ProviderCapacityFile: providerPath}
			cfg.APIToken = "private-api-credential"
			if err := globalconfig.Write(path, cfg); err != nil {
				t.Fatal(err)
			}
			cfg, err = globalconfig.Read(path, globalconfig.WithProjectPathLiterals())
			if err != nil {
				t.Fatal(err)
			}
			state := newGlobalConfigState(cfg)
			owner := runnerCapacityOwner(cfg, state.get)
			before := owner(t.Context(), nil)
			if before == nil || !before.Manageable || before.RuntimeLimit != 2 {
				t.Fatalf("before=%+v", before)
			}
			request := &runnerauth.CapacityRequest{ExpectedConfigRevision: before.Revision, Capacity: 6, Backend: "codex"}
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "stale configuration":
				if err := os.WriteFile(path, append(original, []byte("\n# operator changed configuration\n")...), 0600); err != nil {
					t.Fatal(err)
				}
				original, err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			case "different identity":
				file.Identity.OrganizationID = "org_other"
				if err := runnerauth.Save(identityPath, file); err != nil {
					t.Fatal(err)
				}
			case "different machine":
				cfg.Client.MachineID = string(runnerauth.NewBinding().MachineID)
				if err := globalconfig.Write(path, cfg); err != nil {
					t.Fatal(err)
				}
				original, err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			case "immutable configuration":
				if err := os.Chmod(path, 0400); err != nil {
					t.Fatal(err)
				}
			case "different selected path":
				other := cfg
				other.Path = filepath.Join(root, "other.yaml")
				state.set(other)
			}
			result := owner(t.Context(), request)
			updated, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "apply and reload" {
				if string(original) != string(updated) {
					t.Fatal("changed configuration without current selected authority")
				}
				if scenario == "immutable configuration" && (result == nil || result.Manageable || result.Constraint == "") {
					t.Fatalf("immutable result=%+v", result)
				}
				if scenario == "stale configuration" && (result == nil || result.Constraint == "") {
					t.Fatalf("stale result=%+v", result)
				}
				if scenario == "stale configuration" || scenario == "immutable configuration" {
					observed := owner(t.Context(), nil)
					if observed == nil || observed.Constraint != result.Constraint || observed.Manageable != result.Manageable {
						t.Fatalf("application failure lost before heartbeat: result=%+v observed=%+v", result, observed)
					}
					if err := os.Chmod(path, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, append(original, []byte("\n# corrected operator configuration\n")...), 0600); err != nil {
						t.Fatal(err)
					}
					observed = owner(t.Context(), nil)
					if observed == nil || observed.Constraint != "" || !observed.Manageable {
						t.Fatalf("old failure attributed to new configuration: observed=%+v", observed)
					}
					request.ExpectedConfigRevision = observed.Revision
					applied := owner(t.Context(), request)
					if applied == nil || applied.LocalLimit != 6 || applied.ClientLimit != 6 || !applied.Manageable || applied.Constraint != "Waiting for the existing configuration reload to apply the saved limits." {
						t.Fatalf("corrected request did not apply: %+v", applied)
					}
				}
				return
			}
			if result == nil || result.Constraint == "" || result.RuntimeLimit != 2 {
				t.Fatalf("premature runtime success=%+v", result)
			}
			saved, err := globalconfig.Read(path, globalconfig.WithProjectPathLiterals())
			if err != nil {
				t.Fatal(err)
			}
			if saved.Global.MaxConcurrentAgents != 6 || saved.Client.Capacity != 6 {
				t.Fatalf("saved capacity=%+v", saved.Client)
			}
			observed := owner(t.Context(), nil)
			if observed == nil || result.Revision != observed.Revision || result.LocalLimit != 6 || result.ClientLimit != 6 || result.RuntimeLimit != 2 {
				t.Fatalf("saved evidence=%+v observed=%+v", result, observed)
			}
			saved.Global.MaxConcurrentAgents, saved.Client.Capacity = 2, 2
			if !reflect.DeepEqual(saved, cfg) {
				t.Fatal("capacity edit changed unrelated settings")
			}
			providerAfter, err := os.ReadFile(providerPath)
			if err != nil || string(providerAfter) != string(provider) {
				t.Fatal("forged the external provider report")
			}
			saved, err = globalconfig.Read(path, globalconfig.WithProjectPathLiterals())
			if err != nil {
				t.Fatal(err)
			}
			gate, err := buildGlobalDispatchPools(cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			candidate := scheduler.ProjectCandidate{ID: "capacity-test", Weight: 1}
			var slots []scheduler.Slot
			for range 2 {
				slot, ok, err := gate.TryAcquire(t.Context(), candidate, scheduler.SlotRequest{State: "implement", Host: "local", Weight: 1}, time.Now())
				if err != nil || !ok {
					t.Fatalf("initial acquisition=%t error=%v", ok, err)
				}
				slots = append(slots, slot)
			}
			t.Cleanup(func() {
				for _, slot := range slots {
					if err := gate.Release(slot); err != nil {
						t.Error(err)
					}
				}
			})
			if err := applyGlobalRuntimeConfig(gate, nil, nil, saved); err != nil {
				t.Fatal(err)
			}
			for range 4 {
				slot, ok, err := gate.TryAcquire(t.Context(), candidate, scheduler.SlotRequest{State: "implement", Host: "local", Weight: 1}, time.Now())
				if err != nil || !ok {
					t.Fatalf("additional acquisition=%t error=%v", ok, err)
				}
				slots = append(slots, slot)
			}
			if snapshot := gate.PoolSnapshots(); len(snapshot) != 1 || snapshot[0].Capacity != 6 || snapshot[0].Used != 6 {
				t.Fatalf("reload slots=%+v", snapshot)
			}
			state.set(saved)
			result = owner(t.Context(), request)
			if result == nil || result.RuntimeLimit != 6 || result.Constraint != "" {
				t.Fatalf("applied=%+v", result)
			}
			repeated, err := os.ReadFile(path)
			if err != nil || string(repeated) != string(updated) {
				t.Fatal("idempotent request rewrote configuration")
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), root) {
				t.Fatalf("private configuration escaped: %s", encoded)
			}
		})
	}
}
