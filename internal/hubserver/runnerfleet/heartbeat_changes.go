package runnerfleet

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workpage"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type HeartbeatPosition struct {
	Version  int       `json:"cursor_version"`
	Sequence int64     `json:"sequence"`
	Observed time.Time `json:"observed_at"`
}

const postgresHeartbeatCursorVersion = 2

func ReadHeartbeatChangesTenant(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, cursor workpage.Cursor, key []byte, reset bool, digest, policyID string, now time.Time) (*runnerauth.HeartbeatChanges, error) {
	changes := &runnerauth.HeartbeatChanges{Reset: reset, CapabilitiesDigest: digest, PolicyID: policyID, Items: []runnerauth.HeartbeatItem{}}
	var previous HeartbeatPosition
	if !reset && (json.Unmarshal([]byte(cursor.After), &previous) != nil || previous.Version != postgresHeartbeatCursorVersion || previous.Sequence < 0 || previous.Observed.IsZero() || previous.Observed.After(now)) {
		changes.Reset = true
	}
	sequence, err := reader.ProjectEventSequence(ctx, scope.Project)
	if err != nil {
		return nil, err
	}
	if !changes.Reset && (previous.Sequence > sequence || sequence-previous.Sequence > 200) {
		changes.Reset = true
	}
	if !changes.Reset {
		projectChanges, err := reader.ProjectChanges(ctx, scope.Project, previous.Sequence, sequence, previous.Observed, 201)
		if err != nil {
			return nil, err
		}
		if len(projectChanges) > 200 {
			changes.Reset = true
		} else {
			for _, projectChange := range projectChanges {
				changes.Items = append(changes.Items, runnerauth.HeartbeatItem{ID: projectChange.WorkItemID, Revision: tracker.Revision(projectChange.Revision), LastActivityAt: projectChange.LastActivityAt})
			}
		}
	}
	if changes.Reset {
		changes.Items = []runnerauth.HeartbeatItem{}
	}
	candidates, err := reader.CandidatePage(ctx, claimstore.CandidateQuery{ProjectID: scope.Project, Scope: string(scope.Project), RunnerSelection: true, AvailableAt: now, Limit: 1})
	if err != nil {
		return nil, err
	}
	changes.Claimable = len(candidates.Items) > 0
	position, err := json.Marshal(HeartbeatPosition{Version: postgresHeartbeatCursorVersion, Sequence: sequence, Observed: now})
	if err != nil {
		return nil, err
	}
	cursor.After, cursor.Expires = string(position), now.Add(time.Hour).Unix()
	changes.Cursor, err = workpage.Encode(cursor, key)
	return changes, err
}
