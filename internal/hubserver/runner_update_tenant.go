package hubserver

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
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/update"
)

func (s *Service) storeRunnerUpdateObservationTenant(ctx context.Context, writer tenantstore.Writer, identity runnerstore.Identity, report *runnerauth.UpdateObservation, protocol int, version, platform, architecture string, now time.Time, releasePublished bool) error {
	if err := validateRunnerUpdateObservation(report, protocol, version, platform, architecture); err != nil {
		return err
	}
	state, err := writer.RunnerUpdateState(ctx, identity.RunnerID)
	if err != nil {
		return err
	}
	if report == nil {
		if err := writer.ReplaceRunnerUpdateObservation(ctx, identity.RunnerID, nil); err != nil {
			return err
		}
		return nil
	}
	report.ReceivedAt = now
	if report.Receipt != nil {
		urgent, err := writer.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		accepted := state.Desired != nil && state.Desired.Matches(report.Receipt.Request)
		if state.Observation != nil && state.Observation.Receipt != nil && state.Observation.Receipt.Request.Matches(report.Receipt.Request) {
			accepted = true
		}
		if urgent.Request != nil && urgent.Request.Matches(report.Receipt.Request) {
			accepted = true
		}
		if !accepted {
			report.Receipt = nil
		}
	}
	if report.Receipt != nil && state.Observation != nil {
		report.Receipt.RetainPreviousFailure(state.Observation.Receipt)
	}
	if err := writer.ReplaceRunnerUpdateObservation(ctx, identity.RunnerID, report); err != nil {
		return err
	}
	if !report.Supported || !releasePublished {
		return nil
	}
	return s.followHubRunnerUpdateTenant(ctx, writer, identity, state, *report, now)
}

func (s *Service) followHubRunnerUpdateTenant(ctx context.Context, writer tenantstore.Writer, identity runnerstore.Identity, state runnerstore.UpdateState, report runnerauth.UpdateObservation, now time.Time) error {
	version := runnerUpdateTarget(s.config.Version)
	comparison, err := update.CompareVersions(version, report.Running.Version)
	repair := comparison == 0 && report.Running.Source == "release" && !report.Running.VerifiedRelease
	if version == "" || err != nil || comparison < 0 || comparison == 0 && !repair {
		return nil
	}
	current := state.Desired
	if current != nil {
		staleRefusal := repair && current.ExpectedBuildRevision != report.Revision && report.Receipt != nil && report.Receipt.Request.Matches(*current) && report.Receipt.Status == "refused"
		if current.FollowHub && current.Version == strings.TrimPrefix(version, "v") && !staleRefusal {
			return nil
		}
		if !runnerUpdateSuperseded(*current, report.Receipt, version) {
			return nil
		}
	}
	urgent, err := writer.RunnerUrgentUpdate(ctx)
	if err != nil {
		return err
	}
	if urgent.Request != nil {
		comparison, err := update.CompareVersions(urgent.Request.Version, report.Running.Version)
		if err == nil && comparison > 0 {
			return nil
		}
	}
	request := runnerauth.UpdateRequest{ID: newNativeID("update"), RequestedAt: canonicalRunnerUpdateTime(now), Service: "detent", Version: strings.TrimPrefix(version, "v"), Release: true, FromRelease: true, FollowHub: true, ExpectedBuildRevision: report.Revision}
	if err := request.Validate(); err != nil {
		return nativePublicInvalid("The current runner update request is invalid")
	}
	if err := writer.CreateRunnerUpdateRequest(ctx, runnerstore.UpdateRequestRecord{OrganizationID: identity.OrganizationID, RunnerID: identity.RunnerID, Request: request}); err != nil {
		return err
	}
	return writer.SetRunnerUpdateDesired(ctx, identity.RunnerID, identity.Revision, request.ID)
}

func (s *Service) runnerUpdateViewTenant(ctx context.Context, scope nativeScope, runnerID string) (runnerauth.UpdateView, error) {
	var view runnerauth.UpdateView
	scope.managedRunner = runnerID
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		now := s.config.now().UTC()
		if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
			return err
		}
		runner, err := s.runnerUpdateRunnerTenant(ctx, reader, runnerID, now)
		if err != nil {
			return err
		}
		view = runner.UpdateView(now)
		return nil
	})
	return view, err
}

func (s *Service) runnerUpdateRunnerTenant(ctx context.Context, reader tenantstore.Reader, runnerID string, now time.Time) (runnerauth.Runner, error) {
	identity, err := reader.RunnerIdentity(ctx, runnerID)
	if errors.Is(err, corestore.ErrNotFound) {
		return runnerauth.Runner{}, nativeNotFound()
	}
	if err != nil {
		return runnerauth.Runner{}, err
	}
	if identity.RemovedAt != nil {
		return runnerauth.Runner{}, nativeNotFound()
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
	applyUrgentRunnerRouting(&runner, urgentRunnerUpdate{Revision: urgent.Revision, Request: urgent.Request}, machine.Version)
	return runner, nil
}

func (s *Service) applyRunnerUpdateCommandTenant(ctx context.Context, scope nativeScope, runnerID string, request runnerUpdateChange) (any, error) {
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.ExpectedRevision < 1 || request.delivery("validate", s.config.now()).Validate() != nil || request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 {
		return nil, nativePublicInvalid("A selected service, observed runner/build revision, release version and idempotency key are required")
	}
	if !request.Confirm {
		return nil, &nativeError{Code: "confirmation_required", Message: "Confirm the selected runner update and coordinated restart", status: http.StatusPreconditionRequired, publicMessage: true}
	}
	scope.managedRunner = runnerID
	ctx = s.contentContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	operation := "runner_update " + runnerID
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	var existing *eventstore.Receipt
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, true); err != nil {
			return err
		}
		receipt, err := reader.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
		if err == nil {
			if receipt.RequestHash != hash {
				return nativeConflict(0)
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
		raw, err := content.Receipt(ctx, *existing)
		return raw, err
	}
	preparedAt := canonicalRunnerUpdateTime(s.config.now())
	delivery := request.delivery(newNativeID("update"), preparedAt)
	var preparedView runnerauth.UpdateView
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, true); err != nil {
			return err
		}
		runner, err := s.runnerUpdateRunnerTenant(ctx, reader, runnerID, preparedAt)
		if err != nil {
			return err
		}
		if err := runnerUpdateReady(runner, request, preparedAt); err != nil {
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
	preparedReceipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: preparedAt}, responseRaw)
	if err != nil {
		return nil, err
	}
	usageHash, err := nativeCommandHash([]string{string(scope.organization), scope.credential.ID, operation, request.IdempotencyKey})
	if err != nil {
		return nil, err
	}
	var replay *eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(writer tenantstore.Writer) error {
		now := s.config.now().UTC()
		if err := s.authorizeAccessScope(ctx, writer, scope, true); err != nil {
			return err
		}
		receipt, err := writer.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
		if err == nil {
			if receipt.RequestHash != hash {
				return nativeConflict(0)
			}
			replay = &receipt
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		identity, err := writer.RunnerIdentity(ctx, runnerID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nativeNotFound()
		}
		if err != nil {
			return err
		}
		if identity.OrganizationID != scope.organization || identity.RemovedAt != nil {
			return nativeNotFound()
		}
		runner, err := s.runnerUpdateRunnerTenant(ctx, writer, runnerID, now)
		if err != nil {
			return err
		}
		if err := runnerUpdateReady(runner, request, now); err != nil {
			return err
		}
		if !reflect.DeepEqual(runner.Update, preparedView.Observation) {
			return runnerUpdateConflict(runner.Revision, "The runner update observation changed; read get_runner_update and retry with its current revision")
		}
		if err := writer.CreateRunnerUpdateRequest(ctx, runnerstore.UpdateRequestRecord{OrganizationID: scope.organization, RunnerID: runnerID, Request: delivery}); err != nil {
			return err
		}
		if err := writer.SetRunnerUpdateDesired(ctx, runnerID, identity.Revision, delivery.ID); err != nil {
			return err
		}
		if err := s.applyRunnerTenantUsage(ctx, writer, now, "runner_"+usageHash, false, false); err != nil {
			return err
		}
		_, err = writer.PutReceipt(ctx, preparedReceipt)
		return err
	})
	if err == nil {
		if replay != nil {
			raw, err := content.Receipt(ctx, *replay)
			return raw, err
		}
		return json.RawMessage(responseRaw), nil
	}
	latestReceipt, found, readErr := s.readRunnerUpdateCommandReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
	if readErr != nil {
		return nil, readErr
	}
	if found {
		raw, readErr := content.Receipt(ctx, latestReceipt)
		return raw, readErr
	}
	return nil, err
}

func (s *Service) markUrgentRunnerUpdateTenant(ctx context.Context, scope nativeScope, request urgentRunnerUpdateChange) (any, error) {
	ctx = s.contentContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	operation := "urgent_runner_update"
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	var current runnerstore.UrgentUpdate
	var existing *eventstore.Receipt
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, true); err != nil {
			return err
		}
		receipt, err := reader.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
		if err == nil {
			if receipt.RequestHash != hash {
				return nativeConflict(0)
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
			return nativeConflict(tracker.Revision(current.Revision))
		}
		if current.Request != nil {
			comparison, err := update.CompareVersions(request.Version, current.Request.Version)
			if err != nil || comparison < 0 {
				return nativePublicInvalid("An urgent release cannot lower the existing update target")
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if existing != nil {
		raw, err := content.Receipt(ctx, *existing)
		return raw, err
	}
	preparedAt := canonicalRunnerUpdateTime(s.config.now())
	value := urgentRunnerUpdate{Revision: current.Revision + 1}
	if current.Request != nil && current.Request.Version == request.Version {
		value.Request = current.Request
	} else {
		value.Request = &runnerauth.UpdateRequest{ID: newNativeID("update"), RequestedAt: preparedAt, Service: "detent", Version: request.Version, Release: true, Urgent: true}
	}
	if err := value.Request.Validate(); err != nil {
		return nil, nativeInvalid(err.Error())
	}
	responseRaw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	preparedReceipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: preparedAt}, responseRaw)
	if err != nil {
		return nil, err
	}
	usageHash, err := nativeCommandHash([]string{string(scope.organization), scope.credential.ID, operation, request.IdempotencyKey})
	if err != nil {
		return nil, err
	}
	var replay *eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(writer tenantstore.Writer) error {
		now := s.config.now().UTC()
		if err := s.authorizeAccessScope(ctx, writer, scope, true); err != nil {
			return err
		}
		receipt, err := writer.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
		if err == nil {
			if receipt.RequestHash != hash {
				return nativeConflict(0)
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
			return nativeConflict(tracker.Revision(latest.Revision))
		}
		if latest.Request != nil {
			comparison, err := update.CompareVersions(request.Version, latest.Request.Version)
			if err != nil || comparison < 0 {
				return nativePublicInvalid("An urgent release cannot lower the existing update target")
			}
		}
		if latest.Request == nil || latest.Request.Version != request.Version {
			if err := writer.CreateRunnerUpdateRequest(ctx, runnerstore.UpdateRequestRecord{OrganizationID: scope.organization, Request: *value.Request}); err != nil {
				return err
			}
		}
		if err := writer.SetRunnerUrgentUpdate(ctx, latest.Revision, value.Request.ID, now); err != nil {
			return err
		}
		if err := s.applyRunnerTenantUsage(ctx, writer, now, "runner_"+usageHash, false, false); err != nil {
			return err
		}
		_, err = writer.PutReceipt(ctx, preparedReceipt)
		return err
	})
	if err == nil {
		if replay != nil {
			raw, readErr := content.Receipt(ctx, *replay)
			return raw, readErr
		}
		return value, nil
	}
	if errors.Is(err, corestore.ErrUniqueViolation) || errors.Is(err, corestore.ErrSerializationFailure) {
		readErr := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			if authErr := s.authorizeAccessScope(ctx, reader, scope, true); authErr != nil {
				return authErr
			}
			receipt, readErr := reader.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
			if readErr == nil {
				if receipt.RequestHash != hash {
					return nativeConflict(0)
				}
				replay = &receipt
			}
			return readErr
		})
		if readErr != nil && !errors.Is(readErr, corestore.ErrNotFound) {
			return nil, readErr
		}
		if replay != nil {
			raw, readErr := content.Receipt(ctx, *replay)
			return raw, readErr
		}
	}
	latestReceipt, found, readErr := s.readRunnerUpdateCommandReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
	if readErr != nil {
		return nil, readErr
	}
	if found {
		raw, readErr := content.Receipt(ctx, latestReceipt)
		return raw, readErr
	}
	if errors.Is(err, corestore.ErrRevisionConflict) {
		var latest runnerstore.UrgentUpdate
		if readErr := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			if authErr := s.authorizeAccessScope(ctx, reader, scope, true); authErr != nil {
				return authErr
			}
			var readErr error
			latest, readErr = reader.RunnerUrgentUpdate(ctx)
			return readErr
		}); readErr != nil {
			return nil, readErr
		}
		return nil, nativeConflict(tracker.Revision(latest.Revision))
	}
	return nil, err
}

func (s *Service) readRunnerUpdateCommandReceipt(ctx context.Context, scope nativeScope, operation, key, hash string) (eventstore.Receipt, bool, error) {
	var receipt eventstore.Receipt
	found := false
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, true); err != nil {
			return err
		}
		var err error
		receipt, err = reader.Receipt(ctx, scope.credential.ID, operation, key)
		if err != nil {
			if errors.Is(err, corestore.ErrNotFound) {
				return nil
			}
			return err
		}
		found = true
		if receipt.RequestHash != hash {
			return nativeConflict(0)
		}
		return nil
	})
	if err != nil {
		return eventstore.Receipt{}, false, err
	}
	return receipt, found, nil
}
