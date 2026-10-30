package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatorchanges"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type operatorChangeSource struct {
	nativeQueryer
	scope nativeScope
}

func (s operatorChangeSource) Change(ctx context.Context, item, id string) (tracker.ChangeRequest, error) {
	return readChange(ctx, s.nativeQueryer, s.scope, item, id)
}
func (s operatorChangeSource) Archive(ctx context.Context, item, id string) (operatorchanges.Source, error) {
	q, err := archiveQuery(ctx, s.nativeQueryer, item, "change", id, 0)
	if err != nil {
		return nil, err
	}
	return operatorChangeSource{q, s.scope}, nil
}
func (s operatorChangeSource) ArchiveDescriptor(ctx context.Context) *tracker.ArchiveDescriptor {
	return nativeArchiveDescriptor(ctx)
}
func (s operatorChangeSource) Detail(ctx context.Context, c tracker.ChangeRequest, at time.Time) (tracker.ChangeDetail, error) {
	return readCurrentChangeDetail(ctx, s.nativeQueryer, s.scope, c, at)
}
func (s operatorChangeSource) SourceIssues(ctx context.Context, id string) ([]tracker.ExternalReference, error) {
	return readChangeSourceIssues(ctx, s.nativeQueryer, s.scope, id)
}
func (s operatorChangeSource) Version(ctx context.Context, change, id string) (tracker.ChangeVersion, error) {
	return readChangeVersion(ctx, s.nativeQueryer, change, id)
}
func (s operatorChangeSource) Publication(ctx context.Context, item string, v tracker.ChangeVersion) (tracker.ChangePublicationPush, bool, error) {
	return resolveVersionPublicationPush(ctx, s.nativeQueryer, s.scope, item, v)
}
func (s operatorChangeSource) Resolve(ctx context.Context, b []byte) ([]byte, error) {
	return resolveStorageJSON(ctx, b)
}
func (s operatorChangeSource) Hydrate(ctx context.Context, v any) error {
	return hydrateChangeRecord(ctx, s.nativeQueryer, v)
}
func (s operatorChangeSource) Fragment(p *tracker.NativeRecoveryPage, b []byte, offset int64, digest string) error {
	return recoveryRecordFragment(p, b, offset, digest)
}
func readBoundedChange(ctx context.Context, q nativeQueryer, scope nativeScope, args operatortool.ChangeArguments, r operatortool.ChangeResult) (operatortool.ChangeResult, error) {
	return operatorchanges.Read(ctx, operatorChangeSource{q, scope}, args, r)
}

func boundedChangeCommand(c *gate.CommandResult) *gate.CommandResult {
	return operatorchanges.BoundedCommand(c)
}
func boundedChangeReviewResult(r *gate.ValidatorResult) *gate.ValidatorResult {
	return operatorchanges.BoundedReview(r)
}
func boundedChangeLanding(r *tracker.NativeLandingReceipt) *tracker.NativeLandingReceipt {
	return operatorchanges.BoundedLanding(r)
}
func boundedChangeVersion(v tracker.ChangeVersion) tracker.ChangeVersion {
	return operatorchanges.BoundedVersion(v)
}
