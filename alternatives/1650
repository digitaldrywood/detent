package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type healthProductivityEvidence struct {
	WindowFrom         time.Time `json:"window_from"`
	WindowTo           time.Time `json:"window_to"`
	LatestFailureStage string    `json:"latest_failure_stage"`
}

func healthFailureStage(stage string) string {
	switch stage {
	case "pre_provider", "workspace_preparation", "acceptance_publication", "security_audit_publication", "event_publication", "finalization", "lease_release":
		return stage
	default:
		return "unknown"
	}
}

func healthProductivityFindings(now time.Time, p healthRateProject) []healthFinding {
	out := []healthFinding{}
	from := now.Add(-healthFlowWindow)
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
			if len(ids) < healthEvidenceLimit {
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
		for _, at := range timeline.done {
			if !at.Before(from) && !at.After(now) {
				done++
			}
		}
	}
	if attempts > 0 && (spansWindow || attempts >= healthRetryCount) && landings == 0 && done == 0 && !p.ResidencePartial {
		evidence := healthEvidence{
			Productivity: &healthProductivityEvidence{WindowFrom: from, WindowTo: now, LatestFailureStage: stage},
			Counts:       map[string]int{"attempts": attempts, "sessions": len(sessions), "landings": landings, "done_transitions": done},
			AttemptIDs:   ids,
		}
		summary := fmt.Sprintf("%d attempts ran during thirty minutes with no landings or Done transitions.", attempts)
		out = append(out, newHealthFinding("activity_without_delivery", "human", "project", p.ID, summary, "The project operator checks the latest attempt failure stage and delivery evidence.", []string{p.ID}, evidence))
	}
	for item, s := range streaks {
		if len(s.ids) < healthRetryCount {
			continue
		}
		signature := sha256.Sum256([]byte(s.signature))
		evidence := healthEvidence{
			Productivity: &healthProductivityEvidence{WindowFrom: s.from, WindowTo: s.to, LatestFailureStage: s.stage},
			Counts:       map[string]int{"consecutive_unproductive_attempts": len(s.ids)},
			AttemptIDs:   s.ids,
			Signatures:   []string{hex.EncodeToString(signature[:])},
		}
		summary := fmt.Sprintf("%d consecutive attempts ended with the same failure stage and signature without verified new source.", len(s.ids))
		out = append(out, newHealthFinding("unproductive_attempts", "human", "work_item", item, summary, "The project operator checks the repeated failure stage and recorded source finalization.", []string{p.ID}, evidence))
	}
	return out
}

func readHealthProductivityAttempts(ctx context.Context, q nativeQueryer, scope nativeScope, attempts []healthRateAttempt) error {
	if len(attempts) == 0 {
		return nil
	}
	if len(attempts) > healthReadLimit {
		return errHealthReadLimit
	}
	positions := make(map[string]int, len(attempts))
	args := []any{scope.organization, scope.project}
	for i, a := range attempts {
		positions[a.ID] = i
		args = append(args, a.ID)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(attempts)), ",")
	rows, err := q.QueryContext(ctx, `SELECT id,started_at,session_id,failure_stage,
 CASE WHEN finalization_settled=1 AND finalization_changed=1 THEN finalization_version_id ELSE '' END,
 CASE WHEN hot_projection_version=0 THEN data_json ELSE '' END
 FROM native_attempts WHERE organization_id=? AND project_id=? AND id IN (`+placeholders+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, started, session, stage, source, raw string
		if err := rows.Scan(&id, &started, &session, &stage, &source, &raw); err != nil {
			return err
		}
		a := &attempts[positions[id]]
		if a.Started, err = parseTimeValue(started); err != nil {
			return err
		}
		a.Session, a.Stage, a.SourceVersion = session, stage, source
		if raw != "" {
			var data tracker.NativeRunData
			if err := json.Unmarshal([]byte(raw), &data); err != nil {
				return err
			}
			a.Session = data.SessionID
			if data.TerminalFailure != nil {
				a.Stage = data.TerminalFailure.Stage
			}
			if f := data.Finalization; f != nil && f.Settled && f.Changed {
				a.SourceVersion = f.VersionID
			}
		}
	}
	return rows.Err()
}
