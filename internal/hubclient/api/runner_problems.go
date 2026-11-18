package api

import (
	"context"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func IsolationProblems(report isolation.Report, tier string, observed []runnerauth.Problem) []runnerauth.Problem {
	problems := []runnerauth.Problem{}
	incomplete := func(backend string) bool {
		return slices.ContainsFunc(observed, func(p runnerauth.Problem) bool {
			return p.Code == "backend_probe_failed" && (backend == "" || p.Subject == "backend_isolation" || strings.TrimSuffix(p.Subject, "/sandbox") == backend)
		})
	}
	if len(report) == 0 && !incomplete("") {
		problems = append(problems, runnerauth.NewProblem("backend_missing"))
	}
	for backend, tiers := range report {
		if len(tiers) == 0 && strings.HasSuffix(backend, "/workflow") {
			problems = append(problems, runnerauth.NewProblem("settings_invalid"))
		} else if len(tiers) == 0 && !incomplete(backend) && !slices.ContainsFunc(observed, func(p runnerauth.Problem) bool {
			return strings.TrimSuffix(p.Subject, "/sandbox") == backend
		}) {
			p := runnerauth.NewProblem("backend_missing")
			p.Subject = backend
			problems = append(problems, p)
		}
	}
	if !report.Supports(tier) {
		problem := runnerauth.NewProblem("tier_unavailable")
		problem.Subject = tier
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
		if incomplete("") {
			problem.FixHint = "Check the runner's probe execution; isolation support has not been confirmed."
		}
		other, label := isolation.NativeTrusted, "Full access"
		if tier == isolation.NativeTrusted {
			other, label = isolation.Sandbox, "Sandbox"
		}
		if report.Supports(other) && !incomplete("") {
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

type ProbeRouting struct {
	isolation.Policy
	Generation uint64
}

func (p ProbeRouting) Matches(other ProbeRouting) bool {
	return p.Generation == other.Generation && p.MatchesProbeRouting(other.Tier, other.HostServices)
}

type IsolationProbe struct {
	Routing  ProbeRouting
	Report   isolation.Report
	Problems []runnerauth.Problem
}

func (r *RunnerState) probeRouting(routing runnerauth.Routing) ProbeRouting {
	return ProbeRouting{
		Policy:     isolation.Policy{Tier: routing.IsolationTier, HostServices: slices.Clone(routing.HostServices)},
		Generation: r.probeGeneration,
	}
}

func (r *RunnerState) isolationSettings() (ProbeRouting, bool) {
	if r == nil {
		return ProbeRouting{}, false
	}
	r.routingMu.Lock()
	defer r.routingMu.Unlock()
	var routing runnerauth.Routing
	if r.routing != nil {
		routing = r.routing.Routing
	} else if r.path != "" {
		if snapshot, err := runnerauth.LoadRoutingCache(r.path); err == nil {
			routing = snapshot.Routing
		}
	}
	return r.probeRouting(routing), r.settingsRejected
}

func (r *RunnerState) IsolationProbePolicy() isolation.Policy {
	return r.IsolationProbeRouting().Policy
}

func (r *RunnerState) IsolationProbeRouting() ProbeRouting {
	policy, _ := r.isolationSettings()
	if policy.Tier == "" {
		policy.Tier = isolation.Sandbox
	}
	return policy
}

type heartbeatStatus struct {
	currentIsolation func() IsolationProbe
	problems         []runnerauth.Problem
}

func (m Machine) isolationObservation() IsolationProbe {
	if m.CurrentIsolation != nil {
		return m.CurrentIsolation()
	}
	return IsolationProbe{Report: m.BackendIsolation}
}

func (r *RunnerState) prepareHeartbeatStatus(ctx context.Context, machine *Machine) ([]runnerauth.Problem, bool) {
	policy, rejected := r.isolationSettings()
	if machine.CurrentIsolation == nil {
		probe := IsolationProbe{Routing: policy, Report: machine.BackendIsolation}
		if probe.Routing.Tier == "" {
			probe.Routing.Tier = isolation.Sandbox
		}
		machine.CurrentIsolation = func() IsolationProbe { return probe }
	}
	status := heartbeatStatus{currentIsolation: machine.CurrentIsolation, problems: slices.Clone(machine.Problems)}
	if !runnerHostServicesReachable(ctx, policy.HostServices) {
		status.problems = append(status.problems, runnerauth.NewProblem("host_service_unreachable"))
	}
	r.routingMu.Lock()
	r.status = status
	r.routingMu.Unlock()
	report, problems := status.report(policy)
	machine.BackendIsolation = report
	return r.ObserveProblems(problems, time.Now()), rejected
}

func (s heartbeatStatus) report(policy ProbeRouting) (isolation.Report, []runnerauth.Problem) {
	problems := slices.Clone(s.problems)
	if policy.Tier == "" {
		problems = append(problems, runnerauth.NewProblem("settings_invalid"))
		policy.Tier = isolation.Sandbox
	}
	probe := s.currentIsolation()
	capabilities := probe.Report
	if !probe.Routing.Matches(policy) {
		capabilities = nil
		probe = probe.failuresAfterRoutingChange()
	}
	problems = append(problems, probe.Problems...)
	for _, generic := range IsolationProblems(probe.Report, policy.Tier, problems) {
		matched := false
		for i := range problems {
			if problems[i].Code == generic.Code && (problems[i].Subject == generic.Subject || generic.Subject == policy.Tier) {
				if generic.Code == "tier_unavailable" {
					problems[i].FixHint = generic.FixHint
				}
				matched = true
			}
		}
		if !matched {
			problems = append(problems, generic)
		}
	}
	return capabilities, problems
}

func (probe IsolationProbe) failuresAfterRoutingChange() IsolationProbe {
	failures := maps.Clone(probe.Report)
	maps.DeleteFunc(failures, func(_ string, tiers []string) bool { return len(tiers) > 0 })
	problems := slices.Clone(probe.Problems)
	problem := runnerauth.NewProblem("backend_probe_failed")
	problem.Subject = "backend_isolation"
	problem.Check = "runner isolation probe"
	problem.Message = "Runner isolation settings changed since the last probe."
	for i := range problems {
		if problems[i].CachedCapabilities {
			problems[i].CachedCapabilities = false
			problems[i].Message = problem.Message
		}
	}
	if len(probe.Report) > 0 && len(failures) == 0 {
		problems = append(problems, problem)
	}
	return IsolationProbe{Report: failures, Problems: problems}
}

func (r *RunnerState) RejectSettings(rejected bool) {
	r.routingMu.Lock()
	defer r.routingMu.Unlock()
	r.settingsRejected = rejected
}
