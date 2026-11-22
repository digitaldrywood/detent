package runnerupdate

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/update"
)

type Deps struct {
	Repository      tenantstore.Repository
	Now             func() time.Time
	Version         string
	ReleaseClient   update.ReleaseClient
	VerifySignature update.ChecksumSignatureVerifier
	Published       *sync.Map
	ApplyUsage      func(ctx context.Context, writer billingstore.Writer, now time.Time, receiptID string, completion, heartbeat bool) error
	Details         func() attachment.DetailStore
	ContentContext  func(context.Context) context.Context
}

type Service struct {
	deps Deps
}

func New(deps Deps) *Service {
	return &Service{deps: deps}
}

type Change struct {
	ExpectedRevision      int64  `json:"expected_revision"`
	ExpectedBuildRevision string `json:"expected_build_revision"`
	Service               string `json:"service"`
	Version               string `json:"version"`
	Release               bool   `json:"release"`
	FromRelease           bool   `json:"from_release"`
	Confirm               bool   `json:"confirm,omitempty"`
	IdempotencyKey        string `json:"idempotency_key,omitempty"`
}

func (r Change) Delivery(id string, at time.Time) runnerauth.UpdateRequest {
	return runnerauth.UpdateRequest{RequestedAt: canonicalTime(at), ID: id, Service: r.Service, Version: r.Version, Release: r.Release, FromRelease: r.FromRelease, ExpectedBuildRevision: r.ExpectedBuildRevision}
}

func canonicalTime(at time.Time) time.Time {
	return time.UnixMicro(at.UnixMicro()).UTC()
}

func Ready(r runnerauth.Runner, request Change, now time.Time) error {
	view := r.UpdateView(now)
	if view.Status == "unavailable" || r.Update != nil && r.Update.Discovery == "unknown" {
		return &hubapi.Error{Code: "unavailable", Message: "The enrolled runner has no current installed update owner; upgrade or reconnect that runner", Status: http.StatusServiceUnavailable}
	}
	if view.Status == "requested" || view.Status == "applying" || view.Status == "draining" || view.Status == "uncertain" || view.Status == "applied" && (r.Update.Receipt == nil || !r.Update.Receipt.ExecutionReady()) || view.Status == "restart_requested" {
		return Conflict(r.Revision, "The existing runner update has not settled; read get_runner_update and wait for its running or refused receipt")
	}
	if request.ExpectedRevision != r.Revision {
		return Conflict(r.Revision, "The runner revision has changed; read get_runner_update and use its current revision")
	}
	if request.ExpectedBuildRevision != r.Update.Revision {
		return Conflict(r.Revision, "The observed build revision has changed; read get_runner_update and use its current observation revision")
	}
	repair := request.Release && strings.TrimPrefix(request.Version, "v") == strings.TrimPrefix(r.Update.Running.Version, "v") && r.Update.Running.Source == "release" && !r.Update.Running.VerifiedRelease
	if request.Version != r.Update.AvailableVersion && !repair {
		return Conflict(r.Revision, "The selected version differs from the observed available version; read get_runner_update and select its available version")
	}
	if !request.Release && !r.Update.Pending {
		return Conflict(r.Revision, "The runner has no pending automatic update; read get_runner_update and explicitly select a release")
	}
	return nil
}

func Conflict(revision int64, message string) error {
	return &hubapi.Error{Code: "revision_conflict", Message: message, CurrentRevision: tracker.Revision(revision), Status: http.StatusConflict, Public: true}
}

func ValidateObservation(report *runnerauth.UpdateObservation, protocol int, version, platform, architecture string) error {
	if report != nil && (report.Validate() != nil || protocol != 2 || strings.TrimPrefix(report.Running.Version, "v") != strings.TrimPrefix(version, "v") || report.Running.OS != platform || report.Running.Architecture != architecture) {
		return hubapi.PublicInvalid("Update observation must identify this runner's actual process and supported protocol")
	}
	return nil
}

func superseded(current runnerauth.UpdateRequest, receipt *runnerauth.UpdateReceipt, target string) bool {
	reported := receipt != nil && receipt.Request.Matches(current)
	completed := reported && (receipt.ExecutionReady() || receipt.Status == "applied" && receipt.Running != nil && receipt.Applied != nil && receipt.Running.Matches(*receipt.Applied))
	if reported && !completed && !slices.Contains([]string{"running", "refused", "uncertain"}, receipt.Status) {
		return false
	}
	comparison, err := update.CompareVersions(current.Version, target)
	return err != nil || comparison < 0 || comparison == 0 && (reported || receipt.Refused(current))
}

func (s *Service) contentContext(ctx context.Context) context.Context {
	if s.deps.ContentContext == nil {
		return ctx
	}
	return s.deps.ContentContext(ctx)
}

func (s *Service) details() attachment.DetailStore {
	if s.deps.Details == nil {
		return nil
	}
	return s.deps.Details()
}
