package hubclient

import (
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestIsolationProblems(t *testing.T) {
	for _, test := range []struct {
		name, tier          string
		report              isolation.Report
		want                []string
		reason, alternative string
	}{
		{"healthy", isolation.Sandbox, isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}, []string{}, "", ""},
		{"native works", isolation.NativeTrusted, isolation.Report{"codex": {isolation.NativeTrusted}}, []string{}, "", ""},
		{"sandbox unavailable", isolation.Sandbox, isolation.Report{"codex": {isolation.NativeTrusted}}, []string{"tier_unavailable"}, "Agent access sandbox is unavailable: codex did not report support for this tier.", "Full access"},
		{"backend missing", isolation.Sandbox, isolation.Report{"codex": {}}, []string{"backend_missing", "tier_unavailable"}, "", ""},
		{"no backend", isolation.Sandbox, nil, []string{"backend_missing", "tier_unavailable"}, "", ""},
		{"workflow invalid", isolation.Sandbox, isolation.Report{"project/workflow": {}}, []string{"settings_invalid", "tier_unavailable"}, "", ""},
		{"workflow backend healthy", isolation.Sandbox, isolation.Report{"project/workflow": {isolation.Sandbox, isolation.NativeTrusted}}, []string{}, "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			codes := []string{}
			for _, problem := range isolationProblems(test.report, test.tier) {
				codes = append(codes, problem.Code)
				if problem.Code == "tier_unavailable" && test.reason != "" && (problem.Message != test.reason || !strings.Contains(problem.FixHint, "switch Agent access to "+test.alternative)) {
					t.Fatalf("tier problem=%+v", problem)
				}
			}
			if !reflect.DeepEqual(codes, test.want) {
				t.Fatalf("codes=%v want=%v", codes, test.want)
			}
		})
	}
}

func TestRunnerHostServicesReachable(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := "tcp:" + listener.Addr().String()
	if !runnerHostServicesReachable(t.Context(), []string{address}) {
		t.Fatal("running service was reported unreachable")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if runnerHostServicesReachable(t.Context(), []string{address}) {
		t.Fatal("stopped service was reported reachable")
	}
	if !runnerHostServicesReachable(t.Context(), nil) {
		t.Fatal("empty service list is not a problem")
	}
}

func TestHeartbeatProblemsClear(t *testing.T) {
	source := &runnerCredentialSource{routing: &runnerauth.RoutingSnapshot{Routing: runnerauth.Routing{IsolationTier: isolation.Sandbox}}}
	machine := Machine{BackendIsolation: isolation.Report{"codex": {isolation.NativeTrusted}}}
	first, _ := source.heartbeatProblems(t.Context(), machine)
	second, _ := source.heartbeatProblems(t.Context(), machine)
	if len(first) != 1 || len(second) != 1 || !first[0].FirstSeen.Equal(second[0].FirstSeen) {
		t.Fatalf("problem timestamps changed: %+v %+v", first, second)
	}
	source.rejectSettings(true)
	machine.BackendIsolation["codex"] = []string{isolation.Sandbox, isolation.NativeTrusted}
	problems, rejected := source.heartbeatProblems(t.Context(), machine)
	if len(problems) != 0 || !rejected {
		t.Fatalf("problems=%+v rejected=%v", problems, rejected)
	}
	source.rejectSettings(false)
	_, rejected = source.heartbeatProblems(t.Context(), machine)
	if rejected {
		t.Fatal("settings rejection did not clear")
	}
}
