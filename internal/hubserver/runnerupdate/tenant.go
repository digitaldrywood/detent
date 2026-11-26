package runnerupdate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/update"
)

func (s *Service) StoreObservation(ctx context.Context, writer tenantstore.Writer, identity runnerstore.Identity, report *runnerauth.UpdateObservation, protocol int, version, platform, architecture string, now time.Time, releasePublished bool) error {
	if err := ValidateObservation(report, protocol, version, platform, architecture); err != nil {
		return err
	}
	if locker, ok := writer.(runnerstore.UpdateLocker); ok {
		if err := locker.LockRunnerUpdate(ctx, identity.RunnerID); err != nil {
			return err
		}
	}
	state, err := writer.RunnerUpdateState(ctx, identity.RunnerID)
	if err != nil {
		return err
	}
	if report == nil {
		return writer.ReplaceRunnerUpdateObservation(ctx, identity.RunnerID, nil)
	}
	report.ReceivedAt = now
	if report.Receipt == nil && state.Observation != nil && state.Observation.Receipt != nil {
		report.Receipt = state.Observation.Receipt
		if err := report.Validate(); err != nil {
			return corestore.ErrRevisionConflict
		}
	}
	if report.Receipt != nil {
		urgent, err := writer.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		known, accepted, err := s.acceptStoredReceipt(ctx, writer, identity.RunnerID, state, urgent, report.Receipt, report.Running)
		if err != nil {
			return err
		}
		if !known {
			report.Receipt = nil
			if state.Observation != nil {
				report.Receipt = state.Observation.Receipt
			}
		} else if !accepted {
			return nil
		}
	}
	if err := writer.ReplaceRunnerUpdateObservation(ctx, identity.RunnerID, report); err != nil {
		return err
	}
	lifecycleOwner, ok := writer.(runnerstore.LifecycleReader)
	var lifecycle *runnerauth.Lifecycle
	if ok {
		lifecycle, err = lifecycleOwner.RunnerLifecycle(ctx, identity.RunnerID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
	}
	if lifecycle == nil && !report.Supported {
		return nil
	}
	return s.reconcileTarget(ctx, writer, identity, state, *report, now, lifecycle != nil, releasePublished)
}

func (s *Service) reconcileTarget(ctx context.Context, writer tenantstore.Writer, identity runnerstore.Identity, state runnerstore.UpdateState, report runnerauth.UpdateObservation, now time.Time, lifecycle bool, published bool) error {
	version, held, err := EffectiveTarget(ctx, writer, identity.RunnerID, Target(s.deps.Version))
	if err != nil {
		return err
	}
	if !published && !held {
		return nil
	}
	probe := &runnerauth.UpdateRequest{Version: version}
	allowed, err := offerAllowedForTarget(ctx, writer, probe, version, held)
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}
	comparison, err := update.CompareVersions(version, report.Running.Version)
	repair := comparison == 0 && report.Running.Source == "release" && !report.Running.VerifiedRelease
	if version == "" || err != nil || comparison < 0 && !held || comparison == 0 && !repair {
		return nil
	}
	current := state.Desired
	if current != nil {
		sameTarget := current.Version == strings.TrimPrefix(version, "v")
		staleBuild := lifecycle && sameTarget && current.ExpectedBuildRevision != report.Revision
		staleRefusal := repair && current.ExpectedBuildRevision != report.Revision && report.Receipt != nil && report.Receipt.Request.Matches(*current) && report.Receipt.Status == "refused"
		if sameTarget && !staleRefusal && !staleBuild {
			return nil
		}
		if !held && !staleBuild && !superseded(*current, report.Receipt, version) {
			return nil
		}
	}
	urgent, err := writer.RunnerUrgentUpdate(ctx)
	if err != nil {
		return err
	}
	urgentAllowed, err := offerAllowedForTarget(ctx, writer, urgent.Request, version, held)
	if err != nil {
		return err
	}
	if urgentAllowed && !held {
		comparison, err := update.CompareVersions(urgent.Request.Version, report.Running.Version)
		if err == nil && comparison > 0 {
			return nil
		}
	}
	request := runnerauth.UpdateRequest{ID: hubapi.NewID("update"), RequestedAt: canonicalTime(now), Service: "detent", Version: strings.TrimPrefix(version, "v"), Release: true, FromRelease: true, FollowHub: !lifecycle && !held, ExpectedBuildRevision: report.Revision}
	if err := request.Validate(); err != nil {
		return hubapi.PublicInvalid("The current runner update request is invalid")
	}
	if err := writer.CreateRunnerUpdateRequest(ctx, runnerstore.UpdateRequestRecord{OrganizationID: identity.OrganizationID, RunnerID: identity.RunnerID, Request: request}); err != nil {
		return err
	}
	return writer.SetRunnerUpdateDesired(ctx, identity.RunnerID, identity.Revision, request.ID)
}

func (s *Service) View(ctx context.Context, scope hubapi.Scope, runnerID string) (runnerauth.UpdateView, error) {
	var view runnerauth.UpdateView
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		now := s.deps.Now().UTC()
		if err := scope.Authorize(ctx, reader, false); err != nil {
			return err
		}
		runner, err := s.runner(ctx, reader, runnerID, now)
		if err != nil {
			return err
		}
		view = runner.UpdateView(now)
		view.TargetVersion, view.TargetHeld, err = EffectiveTarget(ctx, reader, runnerID, Target(s.deps.Version))
		return err
	})
	return view, err
}

func (s *Service) runner(ctx context.Context, reader tenantstore.Reader, runnerID string, now time.Time) (runnerauth.Runner, error) {
	identity, err := reader.RunnerIdentity(ctx, runnerID)
	if errors.Is(err, corestore.ErrNotFound) {
		return runnerauth.Runner{}, hubapi.NotFound()
	}
	if err != nil {
		return runnerauth.Runner{}, err
	}
	if identity.RemovedAt != nil {
		return runnerauth.Runner{}, hubapi.NotFound()
	}
	state, err := reader.RunnerUpdateState(ctx, runnerID)
	if err != nil {
		return runnerauth.Runner{}, err
	}
	machine, err := reader.Machine(ctx, string(identity.MachineID))
	if err != nil {
		return runnerauth.Runner{}, err
	}
	urgent, err := reader.RunnerUrgentUpdate(ctx)
	if err != nil {
		return runnerauth.Runner{}, err
	}
	runner := runnerauth.Runner{Binding: runnerauth.Binding{RunnerID: identity.RunnerID, MachineID: identity.MachineID}, Routing: runnerauth.Routing{UpdateRequest: state.Desired}, OrganizationID: identity.OrganizationID, Revision: identity.Revision, LastHeartbeatAt: identity.LastHeartbeatAt, Update: state.Observation}
	if !identity.ExpiresAt.After(now) {
		runner.Health = "expired"
	} else {
		runner.Health = "active"
	}
	if !now.Before(identity.LastHeartbeatAt) && now.Before(identity.LastHeartbeatAt.Add(runnerauth.HeartbeatTimeout)) {
		runner.ConnectionHealth = "online"
	} else {
		runner.ConnectionHealth = "offline"
	}
	if owner, ok := reader.(runnerstore.LifecycleReader); ok {
		lifecycle, err := owner.RunnerLifecycle(ctx, runnerID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return runnerauth.Runner{}, err
		}
		runner.Lifecycle = lifecycle
		runner.ApplyLifecycle(now)
	}
	if err := ApplyAvailableRouting(ctx, reader, &runner, urgent, machine.Version); err != nil {
		return runnerauth.Runner{}, err
	}
	return runner, nil
}

func (s *Service) Apply(ctx context.Context, scope hubapi.Scope, runnerID string, request Change) (any, error) {
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.ExpectedRevision < 1 || request.Delivery("validate", s.deps.Now()).Validate() != nil || request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 {
		return nil, hubapi.PublicInvalid("A selected service, observed runner/build revision, release version and idempotency key are required")
	}
	if !request.Confirm {
		return nil, &hubapi.Error{Code: "confirmation_required", Message: "Confirm the selected runner update and coordinated restart", Status: http.StatusPreconditionRequired, Public: true}
	}
	ctx = s.contentContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	operation := "runner_update " + runnerID
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.details())
	var existing *eventstore.Receipt
	err = s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := scope.Authorize(ctx, reader, true); err != nil {
			return err
		}
		receipt, err := reader.Receipt(ctx, scope.CredentialID, operation, request.IdempotencyKey)
		if err == nil {
			if receipt.RequestHash != hash {
				return &hubapi.Error{Code: "idempotency_conflict", Message: "Idempotency key has different content", Status: http.StatusConflict, Public: true}
			}
			existing = &receipt
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if existing != nil {
		raw, err := s.replayReceipt(ctx, content, *existing)
		return raw, err
	}
	preparedAt := canonicalTime(s.deps.Now())
	delivery := request.Delivery(hubapi.NewID("update"), preparedAt)
	var preparedView runnerauth.UpdateView
	err = s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := scope.Authorize(ctx, reader, true); err != nil {
			return err
		}
		runner, err := s.runner(ctx, reader, runnerID, preparedAt)
		if err != nil {
			return err
		}
		if err := Ready(runner, request, preparedAt); err != nil {
			return err
		}
		runner.UpdateRequest = &delivery
		runner.Revision++
		preparedView = runner.UpdateView(preparedAt)
		return nil
	})
	if err != nil {
		return nil, err
	}
	responseRaw, err := json.Marshal(preparedView)
	if err != nil {
		return nil, err
	}
	preparedReceipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.CredentialID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: preparedAt}, responseRaw)
	if err != nil {
		return nil, err
	}
	preparedReceipt.ResponseReplayUntilNs = preparedAt.Add(receiptstore.ReplayRetention).UnixNano()
	usageHash, err := hubapi.CommandHash([]string{string(scope.Organization), scope.CredentialID, operation, request.IdempotencyKey})
	if err != nil {
		return nil, err
	}
	var replay *eventstore.Receipt
	err = s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(writer tenantstore.Writer) error {
		now := s.deps.Now().UTC()
		if err := scope.Authorize(ctx, writer, true); err != nil {
			return err
		}
		receipt, err := writer.Receipt(ctx, scope.CredentialID, operation, request.IdempotencyKey)
		if err == nil {
			if receipt.RequestHash != hash {
				return &hubapi.Error{Code: "idempotency_conflict", Message: "Idempotency key has different content", Status: http.StatusConflict, Public: true}
			}
			replay = &receipt
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		identity, err := writer.RunnerIdentity(ctx, runnerID)
		if errors.Is(err, corestore.ErrNotFound) {
			return hubapi.NotFound()
		}
		if err != nil {
			return err
		}
		if identity.OrganizationID != scope.Organization || identity.RemovedAt != nil {
			return hubapi.NotFound()
		}
		runner, err := s.runner(ctx, writer, runnerID, now)
		if err != nil {
			return err
		}
		if err := Ready(runner, request, now); err != nil {
			return err
		}
		if !reflect.DeepEqual(runner.Update, preparedView.Observation) {
			return Conflict(runner.Revision, "The runner update observation changed; read get_runner_update and retry with its current revision")
		}
		allowed, err := OfferAllowed(ctx, writer, runnerID, &delivery)
		if err != nil {
			return err
		}
		if !allowed {
			return hubapi.Conflict(0)
		}
		if err := writer.CreateRunnerUpdateRequest(ctx, runnerstore.UpdateRequestRecord{OrganizationID: scope.Organization, RunnerID: runnerID, Request: delivery}); err != nil {
			return err
		}
		if err := writer.SetRunnerUpdateDesired(ctx, runnerID, identity.Revision, delivery.ID); err != nil {
			return err
		}
		if err := s.deps.ApplyUsage(ctx, writer, now, "runner_"+usageHash, false, false); err != nil {
			return err
		}
		_, err = writer.PutReceipt(ctx, preparedReceipt)
		return err
	})
	if err == nil {
		if replay != nil {
			raw, err := s.replayReceipt(ctx, content, *replay)
			return raw, err
		}
		return json.RawMessage(responseRaw), nil
	}
	latestReceipt, found, readErr := s.readCommandReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
	if readErr != nil {
		return nil, readErr
	}
	if found {
		raw, readErr := s.replayReceipt(ctx, content, latestReceipt)
		return raw, readErr
	}
	return nil, err
}

func (s *Service) markUrgent(ctx context.Context, scope hubapi.Scope, request UrgentChange) (any, error) {
	ctx = s.contentContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	operation := "urgent_runner_update"
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.details())
	var current runnerstore.UrgentUpdate
	var existing *eventstore.Receipt
	err = s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := scope.Authorize(ctx, reader, true); err != nil {
			return err
		}
		receipt, err := reader.Receipt(ctx, scope.CredentialID, operation, request.IdempotencyKey)
		if err == nil {
			if receipt.RequestHash != hash {
				return &hubapi.Error{Code: "idempotency_conflict", Message: "Idempotency key has different content", Status: http.StatusConflict, Public: true}
			}
			existing = &receipt
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		current, err = reader.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		if current.Revision != request.ExpectedRevision {
			return hubapi.Conflict(tracker.Revision(current.Revision))
		}
		if current.Request != nil {
			comparison, err := update.CompareVersions(request.Version, current.Request.Version)
			if err != nil || comparison < 0 {
				return hubapi.PublicInvalid("An urgent release cannot lower the existing update target")
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if existing != nil {
		raw, err := s.replayReceipt(ctx, content, *existing)
		return raw, err
	}
	preparedAt := canonicalTime(s.deps.Now())
	value := UrgentUpdate{Revision: current.Revision + 1}
	if current.Request != nil && current.Request.Version == request.Version {
		value.Request = current.Request
	} else {
		value.Request = &runnerauth.UpdateRequest{ID: hubapi.NewID("update"), RequestedAt: preparedAt, Service: "detent", Version: request.Version, Release: true, Urgent: true}
	}
	if err := value.Request.Validate(); err != nil {
		return nil, hubapi.Invalid(err.Error())
	}
	responseRaw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	preparedReceipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.CredentialID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: preparedAt}, responseRaw)
	if err != nil {
		return nil, err
	}
	preparedReceipt.ResponseReplayUntilNs = preparedAt.Add(receiptstore.ReplayRetention).UnixNano()
	preparedReceipt.ResponseReconciliationHash, err = hubapi.CommandHash(*value.Request)
	if err != nil {
		return nil, err
	}
	usageHash, err := hubapi.CommandHash([]string{string(scope.Organization), scope.CredentialID, operation, request.IdempotencyKey})
	if err != nil {
		return nil, err
	}
	var replay *eventstore.Receipt
	err = s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(writer tenantstore.Writer) error {
		now := s.deps.Now().UTC()
		if err := scope.Authorize(ctx, writer, true); err != nil {
			return err
		}
		receipt, err := writer.Receipt(ctx, scope.CredentialID, operation, request.IdempotencyKey)
		if err == nil {
			if receipt.RequestHash != hash {
				return &hubapi.Error{Code: "idempotency_conflict", Message: "Idempotency key has different content", Status: http.StatusConflict, Public: true}
			}
			replay = &receipt
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		latest, err := writer.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		if latest.Revision != request.ExpectedRevision {
			return hubapi.Conflict(tracker.Revision(latest.Revision))
		}
		if latest.Request != nil {
			comparison, err := update.CompareVersions(request.Version, latest.Request.Version)
			if err != nil || comparison < 0 {
				return hubapi.PublicInvalid("An urgent release cannot lower the existing update target")
			}
		}
		allowed, err := OfferAllowed(ctx, writer, "", value.Request)
		if err != nil {
			return err
		}
		if !allowed {
			return hubapi.Conflict(0)
		}
		if latest.Request == nil || latest.Request.Version != request.Version {
			if err := writer.CreateRunnerUpdateRequest(ctx, runnerstore.UpdateRequestRecord{OrganizationID: scope.Organization, Request: *value.Request}); err != nil {
				return err
			}
		}
		if err := writer.SetRunnerUrgentUpdate(ctx, latest.Revision, value.Request.ID, now); err != nil {
			return err
		}
		if err := s.deps.ApplyUsage(ctx, writer, now, "runner_"+usageHash, false, false); err != nil {
			return err
		}
		_, err = writer.PutReceipt(ctx, preparedReceipt)
		return err
	})
	if err == nil {
		if replay != nil {
			raw, readErr := s.replayReceipt(ctx, content, *replay)
			return raw, readErr
		}
		return value, nil
	}
	if errors.Is(err, corestore.ErrUniqueViolation) || errors.Is(err, corestore.ErrSerializationFailure) {
		readErr := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
			if authErr := scope.Authorize(ctx, reader, true); authErr != nil {
				return authErr
			}
			receipt, readErr := reader.Receipt(ctx, scope.CredentialID, operation, request.IdempotencyKey)
			if readErr == nil {
				if receipt.RequestHash != hash {
					return &hubapi.Error{Code: "idempotency_conflict", Message: "Idempotency key has different content", Status: http.StatusConflict, Public: true}
				}
				replay = &receipt
			}
			return readErr
		})
		if readErr != nil && !errors.Is(readErr, corestore.ErrNotFound) {
			return nil, readErr
		}
		if replay != nil {
			raw, readErr := s.replayReceipt(ctx, content, *replay)
			return raw, readErr
		}
	}
	latestReceipt, found, readErr := s.readCommandReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
	if readErr != nil {
		return nil, readErr
	}
	if found {
		raw, readErr := s.replayReceipt(ctx, content, latestReceipt)
		return raw, readErr
	}
	if errors.Is(err, corestore.ErrRevisionConflict) {
		var latest runnerstore.UrgentUpdate
		if readErr := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
			if authErr := scope.Authorize(ctx, reader, true); authErr != nil {
				return authErr
			}
			var readErr error
			latest, readErr = reader.RunnerUrgentUpdate(ctx)
			return readErr
		}); readErr != nil {
			return nil, readErr
		}
		return nil, hubapi.Conflict(tracker.Revision(latest.Revision))
	}
	return nil, err
}

func (s *Service) readCommandReceipt(ctx context.Context, scope hubapi.Scope, operation, key, hash string) (eventstore.Receipt, bool, error) {
	var receipt eventstore.Receipt
	found := false
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := scope.Authorize(ctx, reader, true); err != nil {
			return err
		}
		var err error
		receipt, err = reader.Receipt(ctx, scope.CredentialID, operation, key)
		if err != nil {
			if errors.Is(err, corestore.ErrNotFound) {
				return nil
			}
			return err
		}
		found = true
		if receipt.RequestHash != hash {
			return &hubapi.Error{Code: "idempotency_conflict", Message: "Idempotency key has different content", Status: http.StatusConflict, Public: true}
		}
		return nil
	})
	if err != nil {
		return eventstore.Receipt{}, false, err
	}
	return receipt, found, nil
}

func (s *Service) replayReceipt(ctx context.Context, content *eventstore.Content, receipt eventstore.Receipt) (json.RawMessage, error) {
	if receipt.ResponseExpired || receipt.ResponseReplayUntilNs > 0 && s.deps.Now().UnixNano() >= receipt.ResponseReplayUntilNs {
		return nil, &hubapi.Error{Code: "idempotency_expired", Message: receiptstore.ErrExpired.Error(), Status: http.StatusGone, Public: true}
	}
	return content.Receipt(ctx, receipt)
}
