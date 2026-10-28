package telemetry

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type DispatchEvidence struct {
	Ready      bool
	Status     string
	Detail     string
	ObservedAt time.Time
}

func TodoDispatchEvidence(snapshot Snapshot, issue Issue) DispatchEvidence {
	var latest *SchedulerDecision
	for i := range snapshot.SchedulerDecisions {
		decision := &snapshot.SchedulerDecisions[i]
		if !dispatchDecisionMatches(snapshot, issue, *decision) {
			continue
		}
		if latest == nil || decision.DecisionAt.After(latest.DecisionAt) {
			latest = decision
		}
	}
	result := DispatchEvidence{Status: "Awaiting scheduler", Detail: "Scheduler evidence unavailable; readiness unknown"}
	if latest == nil || latest.DecisionAt.IsZero() {
		return result
	}
	result.ObservedAt = latest.DecisionAt
	refresh, observed := snapshot.Refresh, snapshot.Dispatch.ObservedAt
	for _, project := range snapshot.Projects {
		if project.Project.ID == issue.ProjectID {
			refresh, observed = project.Refresh, project.Dispatch.ObservedAt
			break
		}
	}
	maxAge := max(2*time.Duration(refresh.PollIntervalSeconds)*time.Second, time.Minute)
	age := snapshot.GeneratedAt.Sub(latest.DecisionAt)
	stale := snapshot.LastKnown || snapshot.GeneratedAt.IsZero() || age < 0 || age > maxAge || latest.DecisionAt.Before(observed)
	if issue.UpdatedAt != nil && latest.DecisionAt.Before(*issue.UpdatedAt) {
		stale = true
	}
	if !strings.EqualFold(strings.TrimSpace(latest.Lane), "Todo") {
		stale = true
	}
	if stale {
		result.Status = "Scheduler evidence stale"
		result.Detail = "Scheduler evidence stale; readiness unknown"
	} else if latest.Result == "selected" && latest.Selected {
		result.Ready = true
		result.Status = "Ready"
		result.Detail = "Selected by scheduler"
	} else if latest.Result == "skipped" {
		result.Status = "Waiting"
		result.Detail = strings.TrimSpace(latest.WaitReason)
		if result.Detail == "" {
			result.Detail = strings.ReplaceAll(latest.Reason, "_", " ")
		}
		if latest.Reason == "blocked_by_dependency" && (latest.WaitReason == "" || latest.WaitReason == latest.Reason) {
			refs := make([]string, 0, len(issue.BlockedBy))
			for _, ref := range issue.BlockedBy {
				if ref.Identifier != "" && ref.TrackerState != "closed" {
					refs = append(refs, ref.Identifier)
				}
			}
			if len(refs) > 0 {
				result.Detail = "Waiting on " + strings.Join(refs, ", ")
			}
		}
		if latest.Reason == "global_capacity_full" || latest.Reason == "global_slot_unavailable" {
			result.Status = "Capacity wait"
			result.Detail = dispatchCapacityDetail(latest.CapacitySnapshotJSON, result.Detail)
			for _, decision := range snapshot.SchedulerDecisions {
				if dispatchDecisionMatches(snapshot, issue, decision) && decision.DecisionAt.Equal(latest.DecisionAt) && decision.Reason == "global_capacity_full" {
					result.Detail = dispatchCapacityDetail(decision.CapacitySnapshotJSON, result.Detail)
					break
				}
			}
		}
	}
	result.Detail += " · observed " + latest.DecisionAt.UTC().Format(time.RFC3339)
	return result
}

func dispatchDecisionMatches(snapshot Snapshot, issue Issue, decision SchedulerDecision) bool {
	project := strings.TrimSpace(issue.ProjectID)
	if project == "" {
		project = strings.TrimSpace(snapshot.Project.ID)
	}
	decisionProject := strings.TrimSpace(decision.ProjectID)
	if decisionProject == "" {
		decisionProject = strings.TrimSpace(snapshot.Project.ID)
	}
	if project != decisionProject {
		return false
	}
	if issue.ID != "" && decision.IssueID != "" {
		return issue.ID == decision.IssueID
	}
	return issue.Identifier != "" && issue.Identifier == decision.Identifier
}

func dispatchCapacityDetail(raw string, fallback string) string {
	var capacity struct {
		Pool            string `json:"pool"`
		Limit           int    `json:"global_capacity"`
		Used            int    `json:"global_used"`
		SharedCapacity  int    `json:"shared_capacity"`
		SharedUsed      int    `json:"shared_used"`
		SharedAvailable int    `json:"shared_available"`
	}
	if json.Unmarshal([]byte(raw), &capacity) != nil || capacity.Pool == "" || capacity.Limit <= 0 {
		return fallback + "; capacity evidence unavailable"
	}
	detail := fmt.Sprintf("Pool %s: %d/%d workers used", capacity.Pool, capacity.Used, capacity.Limit)
	if capacity.SharedCapacity > 0 {
		detail += fmt.Sprintf("; fleet %d/%d", capacity.SharedUsed, capacity.SharedCapacity)
		if capacity.SharedAvailable > 0 && capacity.Used >= capacity.Limit {
			detail += fmt.Sprintf("; %d idle fleet slots unavailable to this pool", capacity.SharedAvailable)
		}
	} else {
		detail += "; fleet capacity evidence unavailable"
	}
	return detail
}
