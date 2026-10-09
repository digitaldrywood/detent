package hubclient

import (
	"context"
	"fmt"
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
		problem := runnerauth.NewProblem("tier_unavailable")
		unavailable := []string{}
		for backend, tiers := range report {
			if !slices.Contains(tiers, tier) {
				unavailable = append(unavailable, backend)
			}
		}
		slices.Sort(unavailable)
		if len(unavailable) == 0 {
			problem.Message = fmt.Sprintf("Agent access %s is unavailable: no backend has reported isolation support.", tier)
		} else {
			problem.Message = fmt.Sprintf("Agent access %s is unavailable: %s did not report support for this tier.", tier, strings.Join(unavailable, ", "))
		}
		if len(problem.Message) > 1000 {
			problem.Message = strings.ToValidUTF8(problem.Message[:997], "") + "..."
		}
		problem.FixHint = "Repair the listed backend or its sandbox support, then let the runner refresh."
		other, label := isolation.NativeTrusted, "Full access"
		if tier == isolation.NativeTrusted {
			other, label = isolation.Sandbox, "Sandbox"
		}
		if report.Supports(other) {
			problem.FixHint += " Or switch Agent access to " + label + " in the runner sheet."
		}
		problems = append(problems, problem)
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
	for _, problem := range isolationProblems(machine.BackendIsolation, routing.IsolationTier) {
		index := slices.IndexFunc(problems, func(p runnerauth.Problem) bool { return p.Code == problem.Code })
		if index >= 0 {
			problems[index].FixHint = problem.FixHint
		} else {
			problems = append(problems, problem)
		}
	}
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
