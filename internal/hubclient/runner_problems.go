package hubclient

import (
	"context"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func isolationProblems(report isolation.Report, tier string) []runnerauth.Problem {
	problems := []runnerauth.Problem{}
	if len(report) == 0 {
		problems = append(problems, runnerauth.NewProblem("backend_missing"))
	}
	for backend, tiers := range report {
		if len(tiers) == 0 && strings.HasSuffix(backend, "/workflow") {
			problems = append(problems, runnerauth.NewProblem("settings_invalid"))
		} else if len(tiers) == 0 {
			problems = append(problems, runnerauth.NewProblem("backend_missing"))
		}
	}
	if !report.Supports(tier) {
		problems = append(problems, runnerauth.NewProblem("tier_unavailable"))
	}
	return runnerauth.MergeProblems(nil, problems, time.Time{})
}

func runnerHostServicesReachable(ctx context.Context, services []string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for _, service := range services {
		network, address := "tcp", strings.TrimPrefix(service, "tcp:")
		if path, ok := strings.CutPrefix(service, "unix:"); ok {
			network, address = "unix", path
		}
		connection, err := (&net.Dialer{Timeout: 500 * time.Millisecond}).DialContext(ctx, network, address)
		if err != nil {
			return false
		}
		if err := connection.Close(); err != nil {
			return false
		}
	}
	return true
}

func (r *runnerCredentialSource) heartbeatProblems(ctx context.Context, machine Machine) ([]runnerauth.Problem, bool) {
	r.routingMu.Lock()
	var routing runnerauth.Routing
	if r.routing != nil {
		routing = r.routing.Routing
	} else if snapshot, err := runnerauth.LoadRoutingCache(r.path); err == nil {
		routing = snapshot.Routing
	}
	rejected := r.settingsRejected
	r.routingMu.Unlock()
	problems := slices.Clone(machine.Problems)
	if routing.IsolationTier == "" {
		problems = append(problems, runnerauth.NewProblem("settings_invalid"))
		routing.IsolationTier = isolation.Sandbox
	}
	problems = append(problems, isolationProblems(machine.BackendIsolation, routing.IsolationTier)...)
	if !runnerHostServicesReachable(ctx, routing.HostServices) {
		problems = append(problems, runnerauth.NewProblem("host_service_unreachable"))
	}
	r.routingMu.Lock()
	defer r.routingMu.Unlock()
	r.problems = runnerauth.MergeProblems(r.problems, problems, time.Now())
	return slices.Clone(r.problems), rejected
}

func (r *runnerCredentialSource) rejectSettings(rejected bool) {
	r.routingMu.Lock()
	defer r.routingMu.Unlock()
	r.settingsRejected = rejected
}
