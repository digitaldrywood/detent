package healthsignals

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
)

type ProductivityEvidence = healthstore.ProductivityEvidence

func healthFailureStage(stage string) string {
	switch stage {
	case "pre_provider", "workspace_preparation", "acceptance_publication", "security_audit_publication", "event_publication", "finalization", "lease_release":
		return stage
	default:
		return "unknown"
	}
}

func healthProductivityFindings(now time.Time, p RateProject) []Finding {
	out := []Finding{}
	from := now.Add(-FlowWindow)
	attempts, landings, done := 0, 0, 0
	sessions := map[string]bool{}
	ids := []string{}
	spansWindow := false
	latestFailure := time.Time{}
	stage := "unknown"
	type streak struct {
		ids                      []string
		from, to                 time.Time
		stage, signature, source string
	}
	streaks := map[string]*streak{}
	for _, a := range p.Attempts {
		if a.Landed && !a.LandingAt.Before(from) && !a.LandingAt.After(now) {
			landings++
		}
		if a.Status == "" || a.Started.IsZero() || a.Started.After(now) || a.At.After(now) {
			continue
		}
		if !a.At.Before(from) || a.Status == "running" {
			attempts++
			if len(ids) < EvidenceLimit {
				ids = append(ids, a.ID)
			}
			if a.Session != "" {
				sessions[a.Session] = true
			}
			spansWindow = spansWindow || !a.Started.After(from)
			if a.Signature != "" && !a.At.Before(latestFailure) {
				latestFailure, stage = a.At, healthFailureStage(a.Stage)
			}
		}
		s := streaks[a.Item]
		if s == nil {
			s = &streak{}
			streaks[a.Item] = s
		}
		if a.SourceVersion != "" && a.SourceVersion != s.source {
			s.source, s.ids = a.SourceVersion, nil
			continue
		}
		if a.Status == "running" {
			continue
		}
		if a.Signature == "" || a.Status == "cancelled" || a.Landed {
			s.ids = nil
			continue
		}
		failureStage := healthFailureStage(a.Stage)
		if len(s.ids) == 0 || s.stage != failureStage || s.signature != a.Signature {
			s.ids = nil
			s.from = a.Started
		}
		s.stage, s.signature, s.to = failureStage, a.Signature, a.At
		s.ids = append(s.ids, a.ID)
	}
	for _, timeline := range p.Timelines {
		for _, at := range timeline.Done {
			if !at.Before(from) && !at.After(now) {
				done++
			}
		}
	}
	if attempts > 0 && (spansWindow || attempts >= healthRetryCount) && landings == 0 && done == 0 && !p.ResidencePartial {
		evidence := Evidence{
			Productivity: &ProductivityEvidence{WindowFrom: from, WindowTo: now, LatestFailureStage: stage},
			Counts:       map[string]int{"attempts": attempts, "sessions": len(sessions), "landings": landings, "done_transitions": done},
			AttemptIDs:   ids,
		}
		summary := fmt.Sprintf("%d attempts ran during thirty minutes with no landings or Done transitions.", attempts)
		out = append(out, NewFinding("activity_without_delivery", "human", "project", p.ID, summary, "The project operator checks the latest attempt failure stage and delivery evidence.", []string{p.ID}, evidence))
	}
	for item, s := range streaks {
		if len(s.ids) < healthRetryCount {
			continue
		}
		signature := sha256.Sum256([]byte(s.signature))
		evidence := Evidence{
			Productivity: &ProductivityEvidence{WindowFrom: s.from, WindowTo: s.to, LatestFailureStage: s.stage},
			Counts:       map[string]int{"consecutive_unproductive_attempts": len(s.ids)},
			AttemptIDs:   s.ids,
			Signatures:   []string{hex.EncodeToString(signature[:])},
		}
		summary := fmt.Sprintf("%d consecutive attempts ended with the same failure stage and signature without verified new source.", len(s.ids))
		out = append(out, NewFinding("unproductive_attempts", "human", "work_item", item, summary, "The project operator checks the repeated failure stage and recorded source finalization.", []string{p.ID}, evidence))
	}
	return out
}
