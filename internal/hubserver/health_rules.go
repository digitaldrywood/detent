package hubserver

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"time"
)

const (
	healthInterval       = time.Minute
	healthInstanceWindow = 5 * time.Minute
	healthFlowWindow     = 30 * time.Minute
	healthReopenWindow   = time.Hour
	healthHistoryWindow  = 24 * time.Hour
	healthProjectLimit   = 100
	healthRunnerLimit    = 1000
	healthReadLimit      = 10000
	healthEvidenceLimit  = 20
)

type healthSubject struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type healthEvidence struct {
	EventIDs   []string       `json:"event_ids,omitempty"`
	AttemptIDs []string       `json:"attempt_ids,omitempty"`
	Counts     map[string]int `json:"counts,omitempty"`
}

type healthFinding struct {
	ID          string         `json:"id"`
	Fingerprint string         `json:"fingerprint"`
	Signal      string         `json:"signal"`
	Class       string         `json:"class"`
	Subject     healthSubject  `json:"subject"`
	Projects    []string       `json:"-"`
	OpenedAt    time.Time      `json:"opened_at"`
	LastSeenAt  time.Time      `json:"last_seen_at"`
	ResolvedAt  *time.Time     `json:"resolved_at"`
	Severity    string         `json:"severity"`
	Summary     string         `json:"summary"`
	NextAction  string         `json:"next_action"`
	Evidence    healthEvidence `json:"evidence"`
}

type healthRunner struct {
	ID        string
	Projects  []string
	Heartbeat time.Time
	CreatedAt time.Time
	Leases    int
	FreeSlots int
}

type healthProject struct {
	ID              string
	Candidates      int
	CandidateIDs    map[string]bool
	CandidateSince  time.Time
	LastLanding     time.Time
	FreeSlots       int
	RefreshAt       time.Time
	RefreshDuration time.Duration
	RefusalReasons  []string
	RefusalEvents   []string
	Claims          int
}

type healthWait struct {
	Item    string
	Project string
	Since   time.Time
	Attempt string
	Event   string
	Active  bool
}

type healthLimit struct {
	Item    string
	Project string
	Event   string
	Active  bool
}

type healthSnapshot struct {
	Runners  []healthRunner
	Projects []healthProject
	Waits    []healthWait
	Limits   []healthLimit
}

func newHealthFinding(signal, class, kind, id, summary, action string, projects []string, evidence healthEvidence) healthFinding {
	sum := sha256.Sum256([]byte(signal + "\x00" + kind + "\x00" + id))
	evidence.EventIDs = slices.Clone(evidence.EventIDs[:min(len(evidence.EventIDs), healthEvidenceLimit)])
	evidence.AttemptIDs = slices.Clone(evidence.AttemptIDs[:min(len(evidence.AttemptIDs), healthEvidenceLimit)])
	return healthFinding{Fingerprint: hex.EncodeToString(sum[:]), Signal: signal, Class: class, Subject: healthSubject{kind, id}, Projects: slices.Clone(projects), Severity: "attention", Summary: summary, NextAction: action, Evidence: evidence}
}

func heartbeatFinding(now time.Time, r healthRunner) (healthFinding, bool) {
	baseline := r.Heartbeat
	if baseline.IsZero() {
		baseline = r.CreatedAt
	}
	active := (r.Leases > 0 || len(r.Projects) > 0) && !baseline.IsZero() && now.Sub(baseline) >= healthInstanceWindow
	return newHealthFinding("runner_heartbeat_gap", "instance", "runner", r.ID, "Enrolled runner has not sent a heartbeat for five minutes.", "The runner operator checks the runner process and its Hub connection.", r.Projects, healthEvidence{Counts: map[string]int{"leases": r.Leases}}), active
}

func deadManFinding(now time.Time, p healthProject) (healthFinding, bool) {
	baseline := p.LastLanding
	if baseline.IsZero() {
		baseline = p.CandidateSince
	}
	active := p.Candidates > 0 && p.FreeSlots > 0 && !baseline.IsZero() && now.Sub(baseline) >= healthFlowWindow
	return newHealthFinding("dead_man", "flow", "project", p.ID, "No work has landed for thirty minutes despite candidates and a free slot.", "The project operator checks scheduler admission and landing evidence.", []string{p.ID}, healthEvidence{Counts: map[string]int{"candidates": p.Candidates, "free_slots": p.FreeSlots}}), active
}

func schedulerFinding(now time.Time, p healthProject) (healthFinding, bool) {
	active := !p.RefreshAt.IsZero() && (now.Sub(p.RefreshAt) > healthInstanceWindow || p.RefreshDuration > healthInstanceWindow)
	return newHealthFinding("scheduler_loop_behind", "instance", "project", p.ID, "The project scheduler refresh is more than five minutes behind.", "The runner operator checks project refresh timing and errors.", []string{p.ID}, healthEvidence{Counts: map[string]int{"refresh_seconds": int(p.RefreshDuration / time.Second)}}), active
}

func refusalFinding(p healthProject) (healthFinding, bool) {
	active := p.Candidates > 0 && p.Claims == 0 && len(p.RefusalReasons) == p.Candidates
	reason := ""
	for _, value := range p.RefusalReasons {
		if value == "" || reason != "" && value != reason {
			active = false
		}
		reason = value
	}
	return newHealthFinding("every_candidate_refused", "instance", "project", p.ID, fmt.Sprintf("Every candidate was refused with the same reason: %s.", reason), "The runner operator corrects the reported capacity or policy refusal.", []string{p.ID}, healthEvidence{EventIDs: p.RefusalEvents, Counts: map[string]int{"candidates": p.Candidates, "claims": p.Claims}}), active
}

func humanWaitFinding(now time.Time, w healthWait) (healthFinding, bool) {
	evidence := healthEvidence{}
	if w.Attempt != "" {
		evidence.AttemptIDs = []string{w.Attempt}
	}
	if w.Event != "" {
		evidence.EventIDs = []string{w.Event}
	}
	kind, id := "work_item", w.Item
	if id == "" {
		kind, id = "project", w.Project
	}
	return newHealthFinding("unanswered_human_wait", "human", kind, id, "A human question or permission wait has been unanswered for thirty minutes.", "The responsible human answers the question or performs the recorded permission action.", []string{w.Project}, evidence), w.Active && !w.Since.IsZero() && now.Sub(w.Since) > healthFlowWindow
}

func lifetimeFinding(l healthLimit) (healthFinding, bool) {
	return newHealthFinding("lifetime_limit_hit", "instance", "work_item", l.Item, "The work item has reached its lifetime execution limit.", "The runner operator reviews the linked work item and its lifetime accounting.", []string{l.Project}, healthEvidence{EventIDs: []string{l.Event}}), l.Active
}

func evaluateHealth(now time.Time, snapshot healthSnapshot) []healthFinding {
	findings := []healthFinding{}
	add := func(f healthFinding, active bool) {
		if active {
			findings = append(findings, f)
		}
	}
	for _, r := range snapshot.Runners {
		add(heartbeatFinding(now, r))
	}
	for _, p := range snapshot.Projects {
		add(deadManFinding(now, p))
		add(schedulerFinding(now, p))
		add(refusalFinding(p))
	}
	for _, w := range snapshot.Waits {
		add(humanWaitFinding(now, w))
	}
	for _, l := range snapshot.Limits {
		add(lifetimeFinding(l))
	}
	slices.SortFunc(findings, func(a, b healthFinding) int {
		if a.Fingerprint < b.Fingerprint {
			return -1
		}
		if a.Fingerprint > b.Fingerprint {
			return 1
		}
		return 0
	})
	return slices.CompactFunc(findings, func(a, b healthFinding) bool { return a.Fingerprint == b.Fingerprint })
}
