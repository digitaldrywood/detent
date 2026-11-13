package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/workpad"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/storageconversion"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type runDetailReference struct {
	AttachmentID string `json:"attachment_id"`
	Organization string `json:"organization_id"`
	Entry        string `json:"entry"`
	ObjectKey    string `json:"object_key,omitempty"`
}

func newRunDetailReference(ref attachment.ArchiveReference, organization, entry string) *runDetailReference {
	return &runDetailReference{AttachmentID: "att_" + artifact.Digest([]byte(organization + "\n" + ref.Key))[:32], Organization: organization, Entry: entry}
}

type storedRun struct {
	tracker.NativeRunData
	Detail *runDetailReference `json:"detail,omitempty"`
}

func runDetailEntry(data tracker.NativeRunData) string {
	return data.AttemptID + ":" + strconv.FormatInt(data.Sequence, 10)
}

func (s *Service) runDetailStore() attachment.DetailStore {
	if s.config.RunDetails != nil {
		return s.config.RunDetails
	}
	if s.config.Hosted != nil && s.config.Hosted.SharedEntry != nil {
		return s.config.Hosted.SharedEntry.RunDetails
	}
	return nil
}

func (s *Service) runDetailContext(ctx context.Context) context.Context {
	ctx = s.contentContext(ctx)
	if ctx.Value(hubcontent.RunDetailContextKey{}) != nil {
		return ctx
	}
	owner := s.runDetailStore()
	if owner == nil && s.storageArchiveContent() == nil {
		return ctx
	}
	organization := ""
	if s.config.Hosted != nil {
		organization = s.config.Hosted.OrganizationID
	}
	return context.WithValue(ctx, hubcontent.RunDetailContextKey{}, &hubcontent.RunDetailContext{Store: owner, Organization: organization, Cache: make(map[string]map[string]tracker.NativeRunData)})
}

func (s *Service) runDetailMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.SetRequest(c.Request().WithContext(s.runDetailContext(c.Request().Context())))
		return next(c)
	}
}

func decodeRunDetail(ctx context.Context, q hubapi.NativeQueryer, raw string, result *tracker.NativeRunData) error {
	ctx = payloadQueryContext(ctx, q)
	var stored storedRun
	if err := hubcontent.DecodeStorageJSON(ctx, []byte(raw), &stored); err != nil {
		return err
	}
	if stored.Detail == nil || hubcontent.ExplanationRead(ctx) {
		*result = stored.NativeRunData
		if hubcontent.ExplanationRead(ctx) {
			*result = explanationRun(*result)
		}
		return nil
	}
	runs, err := readRunDetailSegment(ctx, q, stored.Detail)
	if err != nil {
		return err
	}
	run, found := runs[stored.Detail.Entry]
	if !found || run.AttemptID != stored.AttemptID || run.RunID != stored.RunID || run.LeaseID != stored.LeaseID || run.FencingToken != stored.FencingToken || run.PolicyID != stored.PolicyID {
		return fmt.Errorf("match run detail entry: %w", artifact.ErrIntegrity)
	}
	run.MachineID, run.SessionID, run.RunnerID = stored.MachineID, stored.SessionID, stored.RunnerID
	run.Identity = stored.Identity
	run.Sequence, run.Outcome = stored.Sequence, stored.Outcome
	if stored.Sequence != runs[stored.Detail.Entry].Sequence {
		run.Handoff, run.ArtifactIDs = stored.Handoff, stored.ArtifactIDs
	}
	if stored.Outcome == "cancelled" && stored.TerminalFailure != nil {
		run.TerminalFailure = stored.TerminalFailure
	}
	if run.Runtime != nil && run.Runtime.Landing != nil && stored.Runtime != nil && stored.Runtime.Landing != nil {
		runtime := *run.Runtime
		landing := *runtime.Landing
		landing.FromState, landing.TargetState = stored.Runtime.Landing.FromState, stored.Runtime.Landing.TargetState
		runtime.Landing = &landing
		run.Runtime = &runtime
	}
	if storageconversion.Active(ctx) {
		encoded, err := json.Marshal(run)
		if err != nil {
			return err
		}
		if err := hubcontent.ExpandStorageConversion(ctx, len(encoded)); err != nil {
			return err
		}
	}
	*result = run
	return nil
}

func decodeRunHistory(ctx context.Context, q hubapi.NativeQueryer, raw string, result *tracker.CollaborationData) error {
	ctx = payloadQueryContext(ctx, q)
	if err := hubcontent.DecodeStorageJSON(ctx, []byte(raw), result); err != nil {
		return err
	}
	if result.Run == nil {
		return nil
	}
	var envelope struct {
		Run json.RawMessage `json:"run"`
	}
	if err := hubcontent.DecodeStorageJSON(ctx, []byte(raw), &envelope); err != nil {
		return err
	}
	return decodeRunDetail(ctx, q, string(envelope.Run), result.Run)
}

func compactRun(data tracker.NativeRunData) tracker.NativeRunData {
	result := data
	result.Evidence, result.Usage = nil, nil
	result.Steps = nil
	result.CompletionBody = ""
	if data.Disposition != nil {
		disposition := *data.Disposition
		disposition.FinalSummary, disposition.BlockerEvidence = "", nil
		if len(data.Disposition.BlockerEvidence) > 0 {
			disposition.BlockerEvidence = []workpad.Blocker{{}}
			for _, blocker := range data.Disposition.BlockerEvidence {
				if blocker.Predicate != nil && blocker.Predicate.Type == workpad.PredicateIssueState {
					disposition.BlockerEvidence[0].Predicate = &workpad.Predicate{Type: workpad.PredicateIssueState}
					break
				}
			}
		}
		result.Disposition = &disposition
	}
	if data.TerminalFailure != nil {
		failure := data.TerminalFailure.Public()
		result.TerminalFailure = &failure
	}
	if data.Finalization != nil {
		finalization := data.Finalization.Public()
		result.Finalization = &finalization
	}
	if data.Runtime != nil {
		runtime := *data.Runtime
		runtime.Activity, runtime.REST, runtime.GitHub = nil, nil, nil
		if data.Runtime.Activity != nil {
			activity := *data.Runtime.Activity
			activity.Spans, activity.Sources, activity.Summary, activity.CoverageNotes = nil, nil, nil, nil
			runtime.Activity = &activity
		}

		runtime.Phases, runtime.Pipeline, runtime.WorkerLaunch = nil, nil, nil
		if runtime.Validation != nil {
			validation := *runtime.Validation
			validation.Output, validation.Command, validation.Pipeline, validation.Evidence = "", "", nil, nil
			runtime.Validation = &validation
		}
		if runtime.Landing != nil {
			landing := *runtime.Landing
			landing.Pipeline, landing.Packages, landing.SecurityAudit = nil, nil, nil
			if landing.CI != nil {
				ci := *landing.CI
				ci.RequiredChecks, ci.PendingChecks, ci.MissingChecks, ci.FailedChecks = nil, nil, nil, nil
				landing.CI = &ci
			}
			if landing.Gate != nil {
				gate := *landing.Gate
				gate.Output, gate.Command, gate.Pipeline, gate.Evidence = "", "", nil, nil
				landing.Gate = &gate
			}
			runtime.Landing = &landing
		}
		result.Runtime = &runtime
	}
	return result
}

type storedRunCheckpoint struct {
	*tracker.NativeCheckpoint
	Detail *runDetailReference `json:"detail,omitempty"`
}

type checkpointPayload struct {
	Checkpoint  *tracker.NativeCheckpoint `json:"checkpoint"`
	ArtifactIDs []string                  `json:"artifact_ids"`
}

var errRunDetailStoreUnavailable = fmt.Errorf("run detail store unavailable in this context: %w", hubcontent.ErrContentStorageUnconfigured)

func readRunDetailSegment(ctx context.Context, q hubapi.NativeQueryer, detail *runDetailReference) (map[string]tracker.NativeRunData, error) {
	if detail.ObjectKey != "" {
		if !strings.HasPrefix(detail.ObjectKey, "orgs/"+detail.Organization+"/") || detail.AttachmentID != "att_"+artifact.Digest([]byte(detail.Organization + "\n" + detail.ObjectKey))[:32] {
			return nil, artifact.ErrIntegrity
		}
		raw, err := hubcontent.ReadStoredPayload(ctx, detail.ObjectKey)
		if err != nil {
			return nil, err
		}
		var run tracker.NativeRunData
		if err := json.Unmarshal(raw, &run); err != nil {
			return nil, err
		}
		if runDetailEntry(run) != detail.Entry {
			return nil, artifact.ErrIntegrity
		}
		runs := map[string]tracker.NativeRunData{detail.Entry: run}
		return runs, nil
	}
	owner, ok := ctx.Value(hubcontent.RunDetailContextKey{}).(*hubcontent.RunDetailContext)
	if !ok {
		return nil, errRunDetailStoreUnavailable
	}
	owner.Mu.Lock()
	defer owner.Mu.Unlock()
	if owner.Organization != "" && owner.Organization != detail.Organization {
		return nil, fmt.Errorf("match run detail organization: %w", artifact.ErrIntegrity)
	}
	if owner.Reference != nil && detail.AttachmentID == newRunDetailReference(*owner.Reference, owner.Organization, "").AttachmentID {
		return owner.Runs, nil
	}
	if runs := owner.Cache[detail.AttachmentID]; runs != nil {
		return runs, nil
	}
	if content, ok := ctx.Value(hubcontent.ChangeContentKey{}).(*hubcontent.ChangeContent); ok && content.Writing {
		return nil, &hubcontent.ClaimHydration{Run: func(ctx context.Context, q hubapi.NativeQueryer) error {
			_, err := readRunDetailSegment(ctx, q, detail)
			return err
		}}
	}
	if q == nil {
		return nil, errRunDetailStoreUnavailable
	}

	ref, err := scanAttachmentArchive(q.QueryRowContext(ctx, `SELECT object_key,sha256,size,uncompressed_size,codec,schema_version,kind,retention_class,storage_binding_id,customer_owned FROM attachments WHERE id=? AND organization_id=? AND kind='archive'`, detail.AttachmentID, detail.Organization))
	if err != nil {
		return nil, fmt.Errorf("read run detail attachment: %w", errors.Join(artifact.ErrIntegrity, err))
	}
	if err := ref.Validate(detail.Organization); err != nil {
		return nil, fmt.Errorf("validate run detail attachment: %w", errors.Join(artifact.ErrIntegrity, err))
	}
	raw, err := hubcontent.ReadRuntimeObject(ctx, hubcontent.NativeRuntimeObjectKey{Kind: "run_detail", Organization: detail.Organization, Reference: ref}, func() ([]byte, error) {
		if owner.Store != nil {
			raw, err := owner.Store.ReadDetail(ctx, detail.Organization, ref)
			if err != nil {
				return nil, hubcontent.ImmutableContentStorageError(err)
			}
			return raw, nil
		}
		return hubcontent.ReadChangeContent(ctx, detail.Organization, ref)
	})
	if err != nil {
		return nil, fmt.Errorf("read run detail archive: %w", err)
	}
	var runs map[string]tracker.NativeRunData
	if err := json.Unmarshal(raw, &runs); err != nil {
		return nil, fmt.Errorf("decode run detail: %w", err)
	}
	if len(owner.Cache) >= 2 {
		clear(owner.Cache)
	}
	owner.Cache[detail.AttachmentID] = runs
	return runs, nil
}

func (s *Service) freshRunDetailContext(ctx context.Context) context.Context {
	return s.runDetailContext(context.WithValue(context.WithValue(ctx, hubcontent.RunDetailContextKey{}, nil), hubcontent.ChangeContentKey{}, nil))
}
