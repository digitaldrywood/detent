package hubrecords

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type StoredRunCheckpoint struct {
	*tracker.NativeCheckpoint
	Detail *RunDetailReference `json:"detail,omitempty"`
}

type CheckpointPayload struct {
	Checkpoint  *tracker.NativeCheckpoint `json:"checkpoint"`
	ArtifactIDs []string                  `json:"artifact_ids"`
}

func DecodeAttemptCheckpoint(ctx context.Context, attempt leasestore.Attempt, raw []byte, resolve func(context.Context, *RunDetailReference) ([]byte, error)) (*tracker.NativeCheckpoint, error) {
	var wrapper CheckpointPayload
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, errors.Join(artifact.ErrIntegrity, err)
	}
	if wrapper.Checkpoint != nil {
		return wrapper.Checkpoint, nil
	}
	var stored StoredRunCheckpoint
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, errors.Join(artifact.ErrIntegrity, err)
	}
	if stored.Detail == nil {
		if stored.NativeCheckpoint == nil || stored.Resume == "" || stored.WorktreeState == "" {
			return nil, artifact.ErrIntegrity
		}
		return stored.NativeCheckpoint, nil
	}
	detail := stored.Detail
	if detail.Organization != string(attempt.OrganizationID) || detail.AttachmentID == "" || detail.Entry == "" {
		return nil, artifact.ErrIntegrity
	}
	if detail.ObjectKey != "" && (!strings.HasPrefix(detail.ObjectKey, "orgs/"+detail.Organization+"/") || detail.AttachmentID != "att_"+artifact.Digest([]byte(detail.Organization + "\n" + detail.ObjectKey))[:32]) {
		return nil, artifact.ErrIntegrity
	}
	resolved, err := resolve(ctx, detail)
	if err != nil {
		return nil, err
	}
	var run tracker.NativeRunData
	if detail.ObjectKey != "" {
		err = json.Unmarshal(resolved, &run)
	} else {
		var entries map[string]tracker.NativeRunData
		err = json.Unmarshal(resolved, &entries)
		run = entries[detail.Entry]
	}
	if err != nil || RunDetailEntry(run) != detail.Entry || run.AttemptID != attempt.ID || string(run.LeaseID) != attempt.LeaseID || int64(run.FencingToken) != attempt.FencingToken || run.RunID != attempt.RunID || run.Sequence < 1 || run.Sequence > attempt.Sequence || run.Handoff == nil {
		return nil, errors.Join(artifact.ErrIntegrity, err)
	}
	return run.Handoff, nil
}
