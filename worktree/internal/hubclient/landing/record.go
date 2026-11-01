package landing

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Writer interface {
	LandChangeVersion(context.Context, tracker.NativeWorkItemID, string, string, tracker.LandChangeVersion) (tracker.ChangeRequest, error)
}

func Record(ctx context.Context, client Writer, lease tracker.NativeLease, attempt string, landing runner.NativeLanding) error {
	if !landing.Landed {
		return errors.New("only a landed version is recorded")
	}
	receipt := &tracker.NativeLandingReceipt{Incorporation: landing.Incorporation, ConflictFiles: landing.ConflictFiles, SecurityAudit: landing.SecurityAudit, Pipeline: landing.Pipeline, PipelineDropped: landing.PipelineDropped, Path: landing.Path, Packages: landing.Packages, ChangeID: landing.ChangeID, VersionID: landing.VersionID, HeadSHA: landing.HeadSHA, BaseSHA: landing.BaseSHA, MergeSHA: landing.MergeSHA, BaseRef: landing.BaseRef, Method: landing.Method, Gate: landing.Gate, Landed: true, Rebased: landing.Rebased}
	var objects []byte
	if landing.Incorporation != nil {
		objects = landing.Incorporation.Objects
	}
	_, err := client.LandChangeVersion(ctx, lease.WorkItemID, landing.ChangeID, landing.VersionID, tracker.LandChangeVersion{
		Mutation:             tracker.Mutation{IdempotencyKey: attempt + ":landing", LeaseID: lease.ID, FencingToken: lease.FencingToken},
		Receipt:              receipt,
		IncorporationObjects: objects,
		MergeSHA:             landing.MergeSHA, BaseRef: landing.BaseRef, Method: landing.Method, Rebased: landing.Rebased,
	})
	return err
}
