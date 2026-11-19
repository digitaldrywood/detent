package runnerfleet_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerRemoval(t *testing.T) {
	t.Parallel()
	var wallClock atomic.Int64
	now := func() time.Time { return time.UnixMicro(wallClock.Load()).UTC() }
	wallClock.Store(time.Now().UnixMicro())
	h := hubfixture.Open(t, hubfixture.Config{Clock: now})
	for _, test := range []struct {
		name     string
		attempt  bool
		finished bool
		expired  bool
		revoked  bool
		sibling  bool
		mcp      bool
		want     int
	}{
		{name: "idle", want: http.StatusNoContent},
		{name: "already revoked", revoked: true, want: http.StatusNoContent},
		{name: "claimed work", want: http.StatusUnprocessableEntity},
		{name: "running attempt", attempt: true, want: http.StatusUnprocessableEntity},
		{name: "expired running attempt", attempt: true, expired: true, want: http.StatusNoContent},
		{name: "mcp expired running attempt", attempt: true, expired: true, mcp: true, want: http.StatusNoContent},
		{name: "mcp running attempt", attempt: true, mcp: true, want: http.StatusUnprocessableEntity},
		{name: "finished attempt", attempt: true, finished: true, want: http.StatusNoContent},
		{name: "busy sibling", attempt: true, sibling: true, want: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := hubfixture.NewNative(t, h, h.Primary, test.name)
			repository := h.Repository
			organization := f.Project.OrganizationID
			r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.Enroll(t)
			worker := r
			if test.sibling {
				worker = hubfixture.SharedRunner(t, r)
			}
			item := f.Create(t, "Removal history")
			if test.attempt || test.name == "claimed work" {
				report := map[string]any{
					"display_name": worker.Redemption.DisplayName, "capacity": 2, "version": "test",
					"backend_isolation": r.Redemption.BackendIsolation,
					"capacity_configuration": runnerauth.CapacityConfig{
						Revision: strings.Repeat("c", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: now(),
					},
				}
				hubfixture.RequireStatus(t, h.Request(t, http.MethodPost, f.Base+"/machines/"+string(worker.Binding.MachineID)+"/heartbeat", worker.Redemption.Credential, report), http.StatusOK)
				response := h.Request(t, http.MethodPost, f.Base+"/claims", worker.Redemption.Credential, tracker.NativeClaim{
					PolicyID: hubfixture.Policy().ID, WorkItemID: item.WorkItemID, MachineID: worker.Binding.MachineID, SessionID: "remove-" + string(item.WorkItemID),
					TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability},
				})
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				hubfixture.Decode(t, response, &lease)
				if test.attempt {
					event := hubfixture.StartedEvent(lease)
					path := f.Base + "/work-items/" + string(item.WorkItemID) + "/events"
					hubfixture.RequireStatus(t, h.Request(t, http.MethodPost, path, worker.Redemption.Credential, event), http.StatusOK)
					if test.finished {
						event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome = "run.finished", "finish", 2, "succeeded"
						hubfixture.RequireStatus(t, h.Request(t, http.MethodPost, path, worker.Redemption.Credential, event), http.StatusOK)
					}
				}
				if test.expired || test.finished {
					wallClock.Store(lease.ExpiresAt.Add(time.Second).UnixMicro())
				}
			}
			if test.revoked {
				err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(t.Context(), r.Binding.RunnerID)
					if err != nil {
						return err
					}
					return w.RevokeAccessCredential(t.Context(), identity.TokenID, now())
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			removed := test.want == http.StatusNoContent
			if test.mcp {
				call := hubfixture.MCP(t, h.Handler(), hubfixture.MCPPath(organization), h.AdminToken())
				reply := call("tools/call", operatortool.RevokeRunnerIdentity, map[string]string{"request_id": r.Binding.RunnerID, "runner_id": r.Binding.RunnerID})
				if removed {
					var action struct {
						Status chatpkg.ActionStatus `json:"status"`
					}
					if data := hubfixture.MCPData(t, reply, false); json.Unmarshal(data, &action) != nil || action.Status != chatpkg.ActionSucceeded {
						t.Fatalf("MCP removal = %s", data)
					}
				} else {
					hubfixture.MCPData(t, reply, true)
					if result := string(reply.Result) + string(reply.Error); !strings.Contains(result, "active work") {
						t.Fatalf("MCP refusal = %s", result)
					}
				}
			} else {
				response := h.Request(t, http.MethodDelete, r.IdentityPath(), h.AdminToken(), nil)
				hubfixture.RequireStatus(t, response, test.want)
				if !removed && !strings.Contains(response.Body.String(), "active work") {
					t.Fatalf("missing active-work explanation: %s", response.Body.String())
				}
			}
			err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
				identity, err := reader.RunnerIdentity(t.Context(), r.Binding.RunnerID)
				if err != nil {
					return err
				}
				credential, err := reader.AccessCredential(t.Context(), identity.TokenID)
				if err != nil {
					return err
				}
				if (identity.RemovedAt != nil) != removed || (credential.RevokedAt != nil) != removed || identity.DisplayName != "Runner" {
					return fmt.Errorf("identity after removal: removed=%v revoked=%v name=%q", identity.RemovedAt, credential.RevokedAt, identity.DisplayName)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			listing := h.Request(t, http.MethodGet, r.OrgBase+"/runners?limit=100", h.AdminToken(), nil)
			hubfixture.RequireStatus(t, listing, http.StatusOK)
			var runners []runnerauth.Runner
			hubfixture.Decode(t, listing, &runners)
			if listed := slices.ContainsFunc(runners, func(runner runnerauth.Runner) bool { return runner.RunnerID == r.Binding.RunnerID }); listed == removed {
				t.Fatalf("runner routing listed=%v after removal=%v", listed, removed)
			}
			fleetResponse := h.Request(t, http.MethodGet, r.OrgBase+"/fleet", h.AdminToken(), nil)
			hubfixture.RequireStatus(t, fleetResponse, http.StatusOK)
			var fleet struct {
				Runners []struct{ ID string } `json:"runners"`
			}
			hubfixture.Decode(t, fleetResponse, &fleet)
			listed := false
			for _, runner := range fleet.Runners {
				listed = listed || runner.ID == r.Binding.RunnerID
			}
			if listed == removed {
				t.Fatalf("runner fleet listed=%v after removal=%v", listed, removed)
			}
			result := f.Request(t, http.MethodGet, f.Base+"/work-items/"+string(item.WorkItemID)+"/runtime", nil)
			hubfixture.RequireStatus(t, result, http.StatusOK)
			var runtime tracker.NativeRuntimeEvidence
			hubfixture.Decode(t, result, &runtime)
			if slices.Contains(runtime.Unavailable, "approved_dispatch_policy") {
				t.Fatalf("runtime admission unavailable: %v", runtime.Unavailable)
			}
			if listed := slices.ContainsFunc(runtime.Admission, func(entry tracker.NativeRuntimeAdmission) bool { return entry.RunnerID == r.Binding.RunnerID }); listed == removed {
				t.Fatalf("runner runtime admission listed=%v after removal=%v", listed, removed)
			}
			if test.sibling && !slices.ContainsFunc(runtime.Admission, func(entry tracker.NativeRuntimeAdmission) bool { return entry.RunnerID == worker.Binding.RunnerID }) {
				t.Fatal("removing one runner hid its busy sibling")
			}
			status := http.StatusOK
			if removed {
				status = http.StatusUnauthorized
				hubfixture.RequireStatus(t, h.Request(t, http.MethodDelete, r.IdentityPath(), h.AdminToken(), nil), http.StatusNoContent)
				hubfixture.RequireStatus(t, h.Request(t, http.MethodGet, r.IdentityPath()+"/routing", h.AdminToken(), nil), http.StatusNotFound)
			}
			hubfixture.RequireStatus(t, h.Request(t, http.MethodGet, r.IdentityPath(), r.Redemption.Credential, nil), status)
			if test.finished {
				namesResponse := h.Request(t, http.MethodGet, r.OrgBase+"/fleet?include=names", h.AdminToken(), nil)
				hubfixture.RequireStatus(t, namesResponse, http.StatusOK)
				var names struct {
					RunnerNames map[string]struct {
						DisplayName string `json:"display_name"`
					} `json:"runner_names"`
				}
				hubfixture.Decode(t, namesResponse, &names)
				if name := names.RunnerNames[r.Binding.RunnerID].DisplayName; name != "Runner" {
					t.Fatalf("historical fleet name = %q", name)
				}
				response := h.Request(t, http.MethodGet, f.Base+"/work-items/"+string(item.WorkItemID)+"/attempts", h.AdminToken(), nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var attempts tracker.Page[tracker.NativeAttempt]
				hubfixture.Decode(t, response, &attempts)
				if len(attempts.Items) != 1 || attempts.Items[0].Status != "succeeded" {
					t.Fatalf("history after removal: %#v", attempts)
				}
				historicalRunner := attempts.Items[0].RunnerID
				if historicalRunner != r.Binding.RunnerID {
					t.Fatalf("historical runner = %q, want %q", historicalRunner, r.Binding.RunnerID)
				}
				err = repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
					identity, err := reader.RunnerIdentity(t.Context(), historicalRunner)
					if err != nil {
						return err
					}
					if identity.DisplayName != "Runner" {
						return fmt.Errorf("historical runner name = %q", identity.DisplayName)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
