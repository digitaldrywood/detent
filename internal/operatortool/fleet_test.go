package operatortool

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

// Catch direct-call schema bypasses before any application service is reached.
func TestFleetArgumentBoundary(t *testing.T) {
	for _, tt := range []struct {
		name, tool, input string
		valid             bool
	}{
		{"health has no mode input", InstanceHealth, `{"mode":"yolo"}`, false},
		{"capability identifiers bounded", NativeCapabilities, `{"project_id":"` + strings.Repeat("x", 257) + `"}`, false},
		{"outbox cursor bounded", OutboxHealth, `{"cursor":"` + strings.Repeat("x", 2049) + `"}`, false},
		{"debug scope bounded", AIDebugPrompt, `{"scope":"shell"}`, false},
		{"update read", GetRunnerUpdate, `{"runner_id":"r"}`, true},
		{"urgent update read", GetUrgentRunnerUpdate, `{}`, true},
		{"urgent release", MarkUrgentRunnerUpdate, `{"request_id":"urgent","change":{"expected_revision":0,"version":"1.2.4"}}`, true},
		{"urgent update refuses kill path", MarkUrgentRunnerUpdate, `{"request_id":"urgent","change":{"expected_revision":0,"version":"1.2.4","force":true}}`, false},
		{"local update", UpdateApply, `{"request_id":"r","release":true}`, true},
		{"enrolled update", UpdateApply, `{"request_id":"r","runner_id":"runner","change":{"expected_revision":1,"expected_build_revision":"` + strings.Repeat("a", 64) + `","service":"detent","version":"1.2.4","release":true}}`, true},
		{"hub is not selected runner service", UpdateApply, `{"request_id":"r","runner_id":"runner","change":{"expected_revision":1,"expected_build_revision":"` + strings.Repeat("a", 64) + `","service":"hub","version":"1.2.4"}}`, false},
		{"artifact input forbidden", UpdateApply, `{"request_id":"r","runner_id":"runner","change":{"expected_revision":1,"expected_build_revision":"` + strings.Repeat("a", 64) + `","service":"detent","version":"1.2.4","url":"https://other.test/artifact"}}`, false},
		{"capacity read", GetRunnerCapacity, `{"runner_id":"r","backend":"codex"}`, true},
		{"capacity request", UpdateRunnerCapacity, `{"request_id":"r","runner_id":"r","change":{"expected_revision":1,"expected_config_revision":"` + strings.Repeat("a", 64) + `","capacity":6,"backend":"codex"}}`, true},
		{"capacity overflow", UpdateRunnerCapacity, `{"request_id":"r","runner_id":"r","change":{"expected_revision":1,"expected_config_revision":"` + strings.Repeat("a", 64) + `","capacity":10001}}`, false},
		{"capacity raw path", UpdateRunnerCapacity, `{"request_id":"r","runner_id":"r","change":{"expected_revision":1,"expected_config_revision":"` + strings.Repeat("a", 64) + `","capacity":6,"path":"/etc/config"}}`, false},
		{"report population cursor", Reports, `{"population_cursor":"attempts:1000:0"}`, true},
		{"report cursor bounded", Reports, `{"population_cursor":"` + strings.Repeat("x", 129) + `"}`, false},
		{"bounded read", RunnerFleet, `{"limit":200,"offset":0}`, true},
		{"limit overflow", RunnerFleet, `{"limit":201}`, false},
		{"negative offset", RunnerFleet, `{"offset":-1}`, false},
		{"fractional limit", RunnerFleet, `{"limit":1.5}`, false},
		{"auth is not an argument", Refresh, `{"request_id":"r","yolo":true}`, false},
		{"request required", Refresh, `{}`, false},
		{"ordinary write", Refresh, `{"request_id":"r"}`, true},
		{"invalid recovery", RecoverAttempt, `{"request_id":"r","project_id":"p","attempt_id":1,"action":"restart"}`, false},
		{"inspect", RecoverAttempt, `{"request_id":"r","project_id":"p","attempt_id":1,"action":"inspect"}`, true},
		{"trailing value", RunnerFleet, `{} {}`, false},
		{"null", RunnerFleet, `null`, false},
		{"oversized id", RevokeRunnerIdentity, `{"request_id":"r","runner_id":"` + strings.Repeat("x", 257) + `"}`, false},
		{"unknown nested field", UpdateRunnerHost, `{"request_id":"r","machine_id":"m","change":{"expected_revision":1,"display_name":"h","capacity":1,"confirm":true}}`, false},
		{"empty routing timezone readback", UpdateRunnerRouting, `{"request_id":"r","runner_id":"runner","change":{"expected_revision":56,"display_name":"Mac runner","state":"draining","capacity_limit":8,"project_ids":["p1","p2"],"availability":{"timezone":"","windows":[],"hard_deadline":""}}}`, true},
		{"configured routing timezone", UpdateRunnerRouting, `{"request_id":"r","runner_id":"runner","change":{"expected_revision":56,"display_name":"Mac runner","state":"draining","capacity_limit":8,"project_ids":["p1","p2"],"availability":{"timezone":"America/Chicago","windows":["Mon-Fri 09:00-17:00"],"hard_deadline":"1h"}}}`, true},
		{"window slots", UpdateRunnerRouting, `{"request_id":"r","runner_id":"runner","change":{"expected_revision":56,"display_name":"Mac runner","state":"active","capacity_limit":8,"scope":"projects","project_ids":["p1"],"project_ranks":{"p1":1},"availability":{"timezone":"UTC","windows":["Mon-Sun 00:00-24:00"],"window_slots":{"Mon-Sun 00:00-24:00":2}}}}`, true},
		{"zero window slots", UpdateRunnerRouting, `{"request_id":"r","runner_id":"runner","change":{"expected_revision":56,"display_name":"Mac runner","state":"active","capacity_limit":8,"project_ids":["p1"],"availability":{"timezone":"UTC","windows":["Mon-Sun 00:00-24:00"],"window_slots":{"Mon-Sun 00:00-24:00":0}}}}`, true},
		{"negative window slots", UpdateRunnerRouting, `{"request_id":"r","runner_id":"runner","change":{"expected_revision":56,"display_name":"Mac runner","state":"active","capacity_limit":8,"project_ids":["p1"],"availability":{"timezone":"UTC","windows":["Mon-Sun 00:00-24:00"],"window_slots":{"Mon-Sun 00:00-24:00":-1}}}}`, false},
		{"excessive window slots", UpdateRunnerRouting, `{"request_id":"r","runner_id":"runner","change":{"expected_revision":56,"display_name":"Mac runner","state":"active","capacity_limit":8,"project_ids":["p1"],"availability":{"timezone":"UTC","windows":["Mon-Sun 00:00-24:00"],"window_slots":{"Mon-Sun 00:00-24:00":10001}}}}`, false},
		{"fractional window slots", UpdateRunnerRouting, `{"request_id":"r","runner_id":"runner","change":{"expected_revision":56,"display_name":"Mac runner","state":"active","capacity_limit":8,"project_ids":["p1"],"availability":{"timezone":"UTC","windows":["Mon-Sun 00:00-24:00"],"window_slots":{"Mon-Sun 00:00-24:00":1.5}}}}`, false},
		{"string window slots", UpdateRunnerRouting, `{"request_id":"r","runner_id":"runner","change":{"expected_revision":56,"display_name":"Mac runner","state":"active","capacity_limit":8,"project_ids":["p1"],"availability":{"timezone":"UTC","windows":["Mon-Sun 00:00-24:00"],"window_slots":{"Mon-Sun 00:00-24:00":"2"}}}}`, false},
		{"routing timezone bounded", UpdateRunnerRouting, `{"request_id":"r","runner_id":"runner","change":{"expected_revision":56,"display_name":"Mac runner","state":"draining","capacity_limit":8,"project_ids":["p1","p2"],"availability":{"timezone":"` + strings.Repeat("x", 257) + `","windows":[],"hard_deadline":""}}}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateFleetArguments(tt.tool, json.RawMessage(tt.input)); (err == nil) != tt.valid {
				t.Fatalf("valid=%t error=%v", tt.valid, err)
			}
		})
	}
}

// Catch ordinary display edits being forced through approval and material
// access/scheduling changes being treated as cosmetic.
func TestRoutingApprovalClassification(t *testing.T) {
	before := runnerauth.Routing{DisplayName: "old", State: "active", CapacityLimit: 2}
	for _, tt := range []struct {
		name     string
		change   func(*runnerauth.Routing)
		material bool
	}{
		{"rename", func(r *runnerauth.Routing) { r.DisplayName = "new" }, false},
		{"tags", func(r *runnerauth.Routing) { r.Tags = []string{"build"} }, false},
		{"capacity", func(r *runnerauth.Routing) { r.CapacityLimit = 3 }, true},
		{"disable", func(r *runnerauth.Routing) { r.State = "disabled" }, true},
		{"isolation", func(r *runnerauth.Routing) { r.IsolationTier = "native-trusted" }, true},
		{"schedule", func(r *runnerauth.Routing) { r.Availability.HardDeadline = "1h" }, true},
		{"window count", func(r *runnerauth.Routing) { r.Availability.WindowSlots = map[string]int{"Mon-Sun 00:00-24:00": 2} }, true},
		{"host services", func(r *runnerauth.Routing) { r.HostServices = []string{"tcp:127.0.0.1:8080"} }, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			after := before
			tt.change(&after)
			if got := RoutingRequiresApproval(before, after); got != tt.material {
				t.Fatalf("material=%t want %t", got, tt.material)
			}
		})
	}
}
