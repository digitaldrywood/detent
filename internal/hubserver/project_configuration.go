package hubserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e hubProjectExecutor) localProjectConfiguration(ctx context.Context, name string, args operatortool.LocalProjectArguments) (operatortool.Result, error) {
	return e.projectConfigurationCommand(ctx, name, args, false)
}

type configurationIntent struct {
	Arguments operatortool.LocalProjectArguments
	Previous  runnerauth.ProjectConfiguration
	Desired   runnerauth.ProjectConfiguration
	Issuer    hubscope.OperatorChatIdentity
}

type projectConfigurationPlan struct {
	Identity    runnerstore.Identity
	Header      postgres.ConfigurationHeader
	Record      postgres.ConfigurationProject
	Command     postgres.ConfigurationCommand
	View        runnerauth.ProjectConfiguration
	Diagnostics *runnerauth.ProjectDiagnostics
}

func (e hubProjectExecutor) projectConfigurationCommand(ctx context.Context, name string, args operatortool.LocalProjectArguments, preview bool) (operatortool.Result, error) {
	if e.service.config.TenantRepository == nil {
		return operatortool.Result{}, hubapi.ErrProjectServiceUnavailable
	}
	scope, err := e.projectScope(ctx)
	if err != nil {
		return operatortool.Result{}, err
	}
	scope.Project = tracker.ProjectID(args.ProjectID)
	if err := ctx.Err(); err != nil {
		return operatortool.Result{}, err
	}
	plan, err := e.service.readProjectConfigurationPlan(ctx, scope, args)
	if err != nil {
		return operatortool.Result{}, err
	}
	if name == operatortool.LocalProjectConfiguration || plan.Identity.RunnerID == "" {
		return hubProjectResult(plan.View)
	}
	operation := name + " " + args.RunnerID + " " + args.ProjectID
	hash, err := hubapi.CommandHash(args)
	if err != nil {
		return operatortool.Result{}, err
	}
	for _, op := range []string{"configuration result " + operation, operation} {
		raw, found, err := e.service.configurationReceipt(ctx, scope, scope.Credential.ID, op, args.RequestID, hash)
		if err != nil {
			return operatortool.Result{}, err
		}
		if found {
			return hubProjectResult(raw)
		}
	}
	if args.RunnerID != plan.Identity.RunnerID || args.ExpectedRunnerRevision != plan.Identity.Revision {
		return operatortool.Result{}, hubapi.NativeConflict(tracker.Revision(plan.Identity.Revision))
	}
	if plan.View.Constraint != "" || plan.View.ConfigRevision != args.ExpectedConfigRevision || plan.View.EffectivePolicy == nil || plan.View.EffectivePolicy.ID != args.ExpectedPolicyID {
		return operatortool.Result{}, hubapi.NativeConflict(tracker.Revision(plan.Identity.Revision))
	}
	if err := e.service.recheckProjectRecordMutation(ctx, scope); err != nil {
		return operatortool.Result{}, err
	}
	if err := e.service.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		return e.service.configurationRequestAuthority(ctx, r, scope, e.service.config.Clock(), name)
	}); err != nil {
		return operatortool.Result{}, err
	}
	desired := plan.View
	desired.LastOperation = nil
	desired.Diagnostics = nil
	desired.RequestID = args.RequestID
	desired.RunnerRevision = plan.Identity.Revision + 1
	desired.Constraint = ""
	desired.Applied = false
	desired.Saved = false
	desired.Pending = true
	desired.Authority = "hub_configuration"
	switch name {
	case "apply_local_project_policy":
		approval, err := e.service.readTenantProjectPolicy(ctx, scope.Organization, scope.Project)
		if err != nil || approval.Policy.ID != args.PolicyID || approval.Policy.SourceRevision != args.SourceRevision {
			return operatortool.Result{}, hubapi.PolicyMismatch("Approved policy changed; read its current identity before applying")
		}
		if args.AllowLocalBinding != nil {
			if plan.View.UnsettledAttempts != 0 {
				return operatortool.Result{}, hubapi.PolicyMismatch("Active or deferred work must settle before changing allow_local_binding")
			}
			candidate := plan.View.RestrictedBindingPolicy
			if *args.AllowLocalBinding {
				candidate = plan.View.LocalBindingPolicy
			}
			if candidate == nil || candidate.ID != args.PolicyID {
				return operatortool.Result{}, hubapi.PolicyMismatch("The selected binding policy changed")
			}
			desired.AllowLocalBinding = *args.AllowLocalBinding
		}
		desired.EffectivePolicy = &approval.Policy
		desired.SelectedPolicy = &approval.Policy
	case "resume_local_project":
		if plan.View.UnsettledAttempts != 0 {
			return operatortool.Result{}, hubapi.PolicyMismatch("Active or deferred work must settle before resuming")
		}
		desired.Paused = false
		desired.Draining = false
	case "drain_local_project":
		desired.Draining = true
	case "detach_local_project":
		if plan.View.UnsettledAttempts != 0 || !plan.View.Draining && !plan.View.Paused {
			return operatortool.Result{}, hubapi.PolicyMismatch("Active or deferred work must settle before detach")
		}
		desired.Registered = false
		desired.RuntimeRegistered = false
	default:
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	desired.ConfigRevision, err = projectConfigurationDigest(desired)
	if err != nil {
		return operatortool.Result{}, err
	}
	if preview {
		return hubProjectResult(plan.View)
	}
	origin, _ := ctx.Value(hubscope.OperatorEntryOriginKey{}).(string)
	if origin == "" {
		origin = operatortool.CurrentConnection(ctx).Origin
	}
	if scope.Credential.EntryKey != nil && origin == "" {
		return operatortool.Result{}, operatortool.ErrAccessDenied
	}
	intent := configurationIntent{Arguments: args, Previous: plan.View, Desired: desired, Issuer: hubscope.OperatorChatIdentity{Identity: hubscope.OperatorIdentity(scope.Credential, string(scope.Organization)), Origin: origin}}
	intentRef, err := e.service.prepareConfigurationPayload(ctx, scope.Organization, intent)
	if err != nil {
		return operatortool.Result{}, err
	}
	desiredRef, err := e.service.prepareConfigurationPayload(ctx, scope.Organization, desired)
	if err != nil {
		return operatortool.Result{}, err
	}
	pending := plan.View
	pending.RunnerRevision = plan.Identity.Revision + 1
	pending.RequestID = args.RequestID
	pending.Pending = true
	pending.Applied = false
	pending.Saved = false
	raw, err := json.Marshal(pending)
	if err != nil {
		return operatortool.Result{}, err
	}
	now := e.service.config.Clock().UTC()
	receipt, err := eventstore.NewContent(e.service.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: args.RequestID, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
	if err != nil {
		return operatortool.Result{}, err
	}
	var superseded eventstore.Receipt
	if plan.Command.Status == "pending" {
		var previous configurationIntent
		if err := e.service.readConfigurationPayload(ctx, scope.Organization, plan.Command.Intent, &previous); err != nil {
			return operatortool.Result{}, err
		}
		outcome := previous.Desired
		outcome.Pending = false
		outcome.Applied = false
		outcome.Saved = false
		outcome.Constraint = "A newer authorized request superseded this request; its runtime effects were not confirmed."
		outcome.ObservedAt = now
		body, err := json.Marshal(outcome)
		if err != nil {
			return operatortool.Result{}, err
		}
		superseded, err = eventstore.NewContent(e.service.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: plan.Command.ActorID, Operation: "configuration result " + plan.Command.Operation, CommandKey: plan.Command.RequestID, RequestHash: plan.Command.RequestHash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, body)
		if err != nil {
			return operatortool.Result{}, err
		}
	}
	var replay eventstore.Receipt
	err = e.service.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		locker, ok := w.(runnerstore.UpdateLocker)
		if !ok {
			return hubapi.ErrProjectServiceUnavailable
		}
		if err := locker.LockRunnerUpdate(ctx, plan.Identity.RunnerID); err != nil {
			return err
		}
		identity, err := w.RunnerIdentity(ctx, plan.Identity.RunnerID)
		if err != nil {
			return err
		}
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		if err := e.service.configurationRequestAuthority(ctx, w, scope, now, name); err != nil {
			return err
		}
		previous, err := w.Receipt(ctx, scope.Credential.ID, operation, args.RequestID)
		if err == nil {
			if previous.RequestHash != hash {
				return configurationIdempotencyConflict()
			}
			replay = previous
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		owner, ok := w.(postgres.ConfigurationWriter)
		if !ok {
			return hubapi.ErrProjectServiceUnavailable
		}
		if name == "resume_local_project" || name == "detach_local_project" || args.AllowLocalBinding != nil {
			unsettled, err := owner.ConfigurationUnsettled(ctx, identity.RunnerID, scope.Project, now)
			if err != nil {
				return err
			}
			if unsettled {
				return hubapi.PolicyMismatch("Active or deferred work must settle before changing configuration")
			}
		}
		current, err := owner.ConfigurationProject(ctx, identity.RunnerID, scope.Project)
		if err != nil {
			return err
		}
		if current.Revision != plan.Record.Revision || identity.Revision != plan.Identity.Revision {
			return hubapi.NativeConflict(tracker.Revision(identity.Revision))
		}
		if superseded.CommandKey != "" {
			command, err := owner.ConfigurationCommand(ctx, identity.RunnerID, scope.Project)
			if err != nil {
				return err
			}
			if command.Status != "pending" || command.RequestID != plan.Command.RequestID || command.RequestHash != plan.Command.RequestHash {
				return hubapi.NativeConflict(tracker.Revision(identity.Revision))
			}
			if _, err := w.PutReceipt(ctx, superseded); err != nil {
				return err
			}
		}
		if name == "apply_local_project_policy" {
			approved, err := w.ProjectPolicy(ctx, scope.Project)
			if err != nil || approved.PolicyID != args.PolicyID {
				return hubapi.PolicyMismatch("Approved policy changed")
			}
		}
		if err := w.UpdateRunnerRouting(ctx, plan.Identity.Revision, identity); err != nil {
			return err
		}
		identity.Revision++
		header, err := owner.ConfigurationHeader(ctx, identity.RunnerID)
		if err != nil {
			return err
		}
		row := configurationProjectRow(scope.Organization, identity.RunnerID, desired, desiredRef, now)
		_, err = owner.ApplyConfiguration(ctx, postgres.ConfigurationChange{OrganizationID: scope.Organization, Authority: postgres.ConfigurationAuthority{RunnerID: identity.RunnerID, EnrollmentID: identity.EnrollmentID, Revision: identity.Revision, At: now}, ExpectedRevision: header.Revision, ExpectedSourceRevision: header.SourceRevision, SourceRevision: desired.ConfigRevision, Projects: []postgres.ConfigurationProject{row}})
		if err != nil {
			return err
		}
		current, err = owner.ConfigurationProject(ctx, identity.RunnerID, scope.Project)
		if err != nil {
			return err
		}
		if err := owner.SetConfigurationCommand(ctx, postgres.ConfigurationCommand{OrganizationID: scope.Organization, RunnerID: identity.RunnerID, ProjectID: scope.Project, RequestID: args.RequestID, Operation: operation, ActorID: scope.Credential.ID, RequestHash: hash, RunnerRevision: identity.Revision, Status: "pending", Intent: intentRef}, current.Revision); err != nil {
			return err
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if errors.Is(err, corestore.ErrSerializationFailure) {
		return operatortool.Result{}, hubapi.NativeConflict(tracker.Revision(plan.Identity.Revision))
	}
	if err != nil {
		return operatortool.Result{}, err
	}
	if replay.CommandKey != "" {
		raw, err = eventstore.NewContent(e.service.runDetailStore()).Receipt(ctx, replay)
		if err != nil {
			return operatortool.Result{}, err
		}
	}
	return hubProjectResult(json.RawMessage(raw))
}

func (s *Service) readProjectConfigurationPlan(ctx context.Context, scope hubscope.NativeScope, args operatortool.LocalProjectArguments) (projectConfigurationPlan, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	var plan projectConfigurationPlan
	plan.View = project.MissingConfigurationOwner(args.ProjectID)
	now := s.config.Clock()
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		current, err := r.Project(ctx, scope.Project)
		if errors.Is(err, corestore.ErrNotFound) || err == nil && current.DeletedAt != nil {
			return hubapi.NativeNotFound()
		}
		if err != nil {
			return err
		}
		runner := args.RunnerID
		if runner == "" {
			reader, ok := r.(runnerstore.ProjectObservationRunnerPageReader)
			if !ok {
				return hubapi.ErrProjectServiceUnavailable
			}
			runners, more, err := reader.RunnerProjectObservationRunners(ctx, scope.Project, now, 2)
			if err != nil {
				return err
			}
			if more || len(runners) > 1 {
				return operatortool.ErrInvalidArguments
			}
			if len(runners) == 0 {
				return nil
			}
			runner = runners[0]
		}
		identity, err := r.RunnerIdentity(ctx, runner)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		allowed, err := r.RunnerProjectAuthorized(ctx, runner, scope.Project, now)
		if err != nil {
			return err
		}
		if !allowed || identity.RemovedAt != nil || !identity.ExpiresAt.After(now) || identity.LastHeartbeatAt.Before(now.Add(-runnerauth.HeartbeatTimeout)) {
			return nil
		}
		owner, ok := r.(postgres.ConfigurationReader)
		if !ok {
			return hubapi.ErrProjectServiceUnavailable
		}
		plan.Record, err = owner.ConfigurationProject(ctx, runner, scope.Project)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		plan.Header, err = owner.ConfigurationHeader(ctx, runner)
		if err != nil {
			return err
		}
		plan.Command, err = owner.ConfigurationCommand(ctx, runner, scope.Project)
		if err != nil {
			return err
		}
		observation, _, err := r.RunnerProjectObservation(ctx, runner, scope.Project, "")
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		plan.Diagnostics = observation.Diagnostics
		plan.Identity = identity
		unsettled, err := owner.ConfigurationUnsettled(ctx, runner, scope.Project, now)
		if err != nil {
			return err
		}
		if unsettled {
			plan.View.UnsettledAttempts = 1
		}
		return nil
	})
	if err != nil || plan.Identity.RunnerID == "" {
		return plan, err
	}
	unsettled := plan.View.UnsettledAttempts
	var stored runnerauth.ProjectConfiguration
	if err := s.readConfigurationPayload(ctx, scope.Organization, eventstore.Reference(plan.Record.PayloadObjectKey, plan.Record.PayloadSHA256, plan.Record.PayloadBytes, plan.Record.PayloadContentBytes), &stored); err != nil {
		return plan, err
	}
	plan.View = stored
	if plan.Command.Status == "pending" {
		var intent configurationIntent
		if err := s.readConfigurationPayload(ctx, scope.Organization, plan.Command.Intent, &intent); err != nil {
			return plan, err
		}
		plan.View = intent.Previous
		plan.View.RequestID = plan.Command.RequestID
		plan.View.Pending = true
		plan.View.Applied = false
		plan.View.Saved = false
	}
	if plan.Command.Observation.Key != "" {
		var observation runnerauth.ProjectConfiguration
		if err := s.readConfigurationPayload(ctx, scope.Organization, plan.Command.Observation, &observation); err != nil {
			return plan, err
		}
		plan.View.Source = observation.Source
		plan.View.SelectedPolicy = observation.SelectedPolicy
		plan.View.LocalBindingPolicy = observation.LocalBindingPolicy
		plan.View.RestrictedBindingPolicy = observation.RestrictedBindingPolicy
		plan.View.Diagnostics = observation.Diagnostics
		plan.View.RuntimeRegistered = observation.RuntimeRegistered
	}
	plan.View.Diagnostics = plan.Diagnostics
	plan.View.Authority = "hub_configuration"
	plan.View.RunnerID = plan.Identity.RunnerID
	plan.View.RunnerRevision = plan.Identity.Revision
	plan.View.UnsettledAttempts = unsettled
	plan.View.ObservedAt = now
	if plan.Command.Status == "completed" {
		plan.View.Constraint = ""
		raw, found, err := s.configurationReceipt(ctx, scope, plan.Command.ActorID, "configuration result "+plan.Command.Operation, plan.Command.RequestID, plan.Command.RequestHash)
		if err != nil {
			return plan, err
		}
		if found {
			var completed runnerauth.ProjectConfiguration
			if json.Unmarshal(raw, &completed) != nil {
				return plan, hubapi.ErrProjectServiceUnavailable
			}
			receipt := &runnerauth.ProjectConfigurationReceipt{RequestID: completed.RequestID, Operation: strings.Split(plan.Command.Operation, " ")[0], ConfigRevision: completed.ConfigRevision, Applied: completed.Applied, Saved: completed.Saved, Constraint: completed.Constraint, ObservedAt: completed.ObservedAt}
			if completed.EffectivePolicy != nil {
				receipt.PolicyID = completed.EffectivePolicy.ID
			}
			plan.View.LastOperation = receipt
		}
	}
	return plan, nil
}

func (s *Service) configurationRequestAuthority(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, _ time.Time, operation string) error {
	if scope.Credential.Runner.RunnerID != "" {
		return hubapi.NativeNotFound()
	}
	if scope.Credential.Hosted == nil && scope.Credential.EntryKey == nil && scope.Credential.Scope != hubapi.APIScopeAdmin {
		return hubapi.NativeNotFound()
	}
	scope.RequireHostedAdmin = true
	if err := s.authorizeWorkItemRecordScope(ctx, r, scope); err != nil {
		return err
	}
	if operation == "resume_local_project" && scope.Credential.Hosted != nil {
		grant, err := r.AccessProjectGrant(ctx, scope.Credential.Hosted.Subject, scope.Project)
		if err != nil {
			return err
		}
		if !grant.ManageRunner {
			return hubapi.NativeNotFound()
		}
	}
	return nil
}
func configurationIdempotencyConflict() error {
	return &hubapi.NativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", Status: 409, PublicMessage: true}
}
func (s *Service) configurationReceipt(ctx context.Context, scope hubscope.NativeScope, actor, operation, key, hash string) (json.RawMessage, bool, error) {
	var receipt eventstore.Receipt
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		var err error
		receipt, err = r.Receipt(ctx, actor, operation, key)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if receipt.RequestHash != hash {
		return nil, false, configurationIdempotencyConflict()
	}
	if receipt.ResponseExpired || receipt.ResponseReplayUntilNs > 0 && s.config.Clock().UnixNano() >= receipt.ResponseReplayUntilNs {
		return nil, false, receiptstore.ErrExpired
	}
	raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, receipt)
	return raw, err == nil, err
}
func (s *Service) prepareConfigurationPayload(ctx context.Context, organization tracker.OrganizationID, value any) (attachment.ArchiveReference, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	store := s.runDetailStore()
	if store == nil {
		return attachment.ArchiveReference{}, errors.Join(errConfigurationPayloadUnavailable, artifact.ErrStorage)
	}
	expected, _, err := attachment.PrepareArchive(string(organization), 1, bytes.NewReader(raw))
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	if err := s.registerConfigurationContent(ctx, string(organization), expected); err != nil {
		return attachment.ArchiveReference{}, err
	}
	ref, err := eventstore.NewContent(hubrecords.RunEventDetailStore{DetailStore: store}).PreparePayload(ctx, organization, raw)
	if err != nil {
		return attachment.ArchiveReference{}, errors.Join(errConfigurationPayloadUnavailable, err)
	}
	if ref != expected {
		return attachment.ArchiveReference{}, errors.Join(errConfigurationPayloadUnavailable, artifact.ErrIntegrity)
	}
	return ref, nil
}
func (s *Service) readConfigurationPayload(ctx context.Context, organization tracker.OrganizationID, ref attachment.ArchiveReference, value any) error {
	if ref.Validate(string(organization)) != nil {
		return errors.Join(errConfigurationPayloadUnavailable, hubapi.ErrProjectServiceUnavailable)
	}
	raw, err := s.runDetailStore().ReadDetail(ctx, string(organization), ref)
	if err != nil {
		return errors.Join(errConfigurationPayloadUnavailable, err)
	}
	if json.Unmarshal(raw, value) != nil {
		return errors.Join(errConfigurationPayloadUnavailable, hubapi.ErrProjectServiceUnavailable)
	}
	return nil
}
func configurationProjectRow(organization tracker.OrganizationID, runner string, view runnerauth.ProjectConfiguration, ref attachment.ArchiveReference, now time.Time) postgres.ConfigurationProject {
	state := "attached"
	if view.Draining {
		state = "draining"
	}
	if !view.Registered {
		state = "detached"
	}
	policyID := ""
	if view.EffectivePolicy != nil {
		policyID = view.EffectivePolicy.ID
	}
	return postgres.ConfigurationProject{OrganizationID: organization, RunnerID: runner, ProjectID: tracker.ProjectID(view.ProjectID), ConfigRevision: view.ConfigRevision, PolicyID: policyID, State: state, Paused: view.Paused, AllowLocalBinding: view.AllowLocalBinding, LocalIntakeEnabled: view.LocalIntakeEnabled, PayloadObjectKey: ref.Key, PayloadSHA256: ref.SHA256, PayloadBytes: ref.CompressedSize, PayloadContentBytes: ref.UncompressedSize, UpdatedAt: now}
}

func pruneExpiredProjectDiagnostics(diagnostics *runnerauth.ProjectDiagnostics, now time.Time) bool {
	previous := len(diagnostics.Records)
	diagnostics.Records = slices.DeleteFunc(diagnostics.Records, func(record runnerauth.DiagnosticAttempt) bool {
		return !record.OperationPending && !record.LeaseExpiresAt.After(now) && !slices.ContainsFunc(record.Membership, func(membership string) bool {
			return membership != "durable_active"
		})
	})
	return len(diagnostics.Records) != previous
}

func projectConfigurationDigest(view runnerauth.ProjectConfiguration) (string, error) {
	view.ObservedAt = time.Time{}
	if view.Diagnostics != nil {
		diagnostics := *view.Diagnostics
		diagnostics.ObservedAt, diagnostics.RuntimeObservedAt = time.Time{}, time.Time{}
		view.Diagnostics = &diagnostics
	}
	raw, err := json.Marshal(view)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

var errConfigurationPayloadUnavailable = errors.New("configuration payload unavailable")

type configurationHeartbeat struct {
	record           postgres.ConfigurationProject
	command          postgres.ConfigurationCommand
	view             runnerauth.ProjectConfiguration
	intent           configurationIntent
	payload          attachment.ArchiveReference
	observation      attachment.ArchiveReference
	completion       eventstore.Receipt
	issuer           hubscope.NativeScope
	deliver          bool
	settle           bool
	initialize       bool
	identityRevision int64
	reported         *runnerauth.ProjectConfiguration
	policy           *policystore.Observation
}

func (s *Service) prepareConfigurationHeartbeat(ctx context.Context, credential hubapi.APICredential, projectID tracker.ProjectID, reported *runnerauth.ProjectConfiguration) (*configurationHeartbeat, error) {
	if projectID == "" {
		return nil, nil
	}
	p := &configurationHeartbeat{reported: reported}
	assigned := false
	err := s.config.TenantRepository.ReadTenant(ctx, credential.Runner.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		identity, err := r.RunnerIdentityByCredential(ctx, credential.ID)
		if err != nil {
			return err
		}
		if identity.RunnerID != credential.Runner.RunnerID {
			return hubscope.RunnerUnauthorized()
		}
		if err := validateConfigurationRunner(reported, identity); err != nil {
			return err
		}
		p.identityRevision = identity.Revision
		assigned, err = r.RunnerProjectAuthorized(ctx, identity.RunnerID, projectID, s.config.Clock())
		if err != nil {
			return err
		}
		if !assigned {
			if reported == nil {
				return nil
			}
			return hubapi.NativeNotFound()
		}
		owner, ok := r.(postgres.ConfigurationReader)
		if !ok {
			return hubapi.ErrProjectServiceUnavailable
		}
		p.record, err = owner.ConfigurationProject(ctx, identity.RunnerID, projectID)
		if errors.Is(err, corestore.ErrNotFound) {
			p.initialize = true
			return nil
		}
		if err != nil {
			return err
		}
		p.command, err = owner.ConfigurationCommand(ctx, identity.RunnerID, projectID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if !assigned {
		return nil, nil
	}
	org := credential.Runner.OrganizationID
	if p.initialize {
		if reported == nil {
			return nil, nil
		}
		approval, err := s.readTenantProjectPolicy(ctx, org, projectID)
		if err != nil {
			return nil, err
		}
		p.view = runnerauth.ProjectConfiguration{ProjectID: string(projectID), Authority: "hub_configuration", Registered: true, Source: "cloud_workflow", EffectivePolicy: &approval.Policy, SelectedPolicy: &approval.Policy, ObservedAt: s.config.Clock().UTC()}
		p.view.ConfigRevision, err = projectConfigurationDigest(p.view)
		if err != nil {
			return nil, err
		}
		p.payload, err = s.prepareConfigurationPayload(ctx, org, p.view)
		if err != nil {
			return nil, err
		}
	} else {
		if err := s.readConfigurationPayload(ctx, org, eventstore.Reference(p.record.PayloadObjectKey, p.record.PayloadSHA256, p.record.PayloadBytes, p.record.PayloadContentBytes), &p.view); err != nil {
			return nil, err
		}
	}
	if reported != nil {
		if reported.SelectedPolicy != nil {
			observation, err := s.prepareConfigurationPolicyObservation(ctx, hubscope.NativeScope{Organization: org, Project: projectID, Credential: credential}, policy.Observation{Descriptor: *reported.SelectedPolicy})
			if err == nil {
				p.policy = &observation
			} else {
				var limit *hostedplans.LimitError
				if errors.As(err, &limit) {
					return nil, err
				}
				var failure *hubapi.NativeError
				if errors.As(err, &failure) {
					if failure.Code != "policy_not_approved" {
						return nil, err
					}
				} else {
					return nil, errors.Join(errConfigurationPayloadUnavailable, err)
				}
			}
		}
		observation := *reported
		observation.Diagnostics = nil
		observation.ObservedAt = time.Time{}
		observation.RunnerID = ""
		observation.RunnerRevision = 0
		raw, err := json.Marshal(observation)
		if err != nil {
			return nil, err
		}
		ref, _, err := attachment.PrepareArchive(string(org), 1, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		if ref.SHA256 != p.command.Observation.SHA256 {
			p.observation, err = s.prepareConfigurationPayload(ctx, org, observation)
			if err != nil {
				return nil, err
			}
		}

	}
	if p.command.Status != "pending" {
		return p, nil
	}
	if err := s.readConfigurationPayload(ctx, org, p.command.Intent, &p.intent); err != nil {
		return nil, err
	}
	p.view = p.intent.Previous
	p.view.Pending = true
	operation := strings.Split(p.command.Operation, " ")[0]
	authority, authErr := s.resolveOperatorChatAuthority(ctx, p.intent.Issuer.Identity, p.intent.Issuer.Origin)
	if authErr == nil {
		requirement := operatortool.Requirement{Scope: apikey.ScopeAdmin, OrganizationID: string(org), ProjectID: string(projectID)}
		if operation == "resume_local_project" {
			requirement.ResourceKind = "runners"
		}
		authErr = authority.Check(ctx, requirement)
		if authErr == nil {
			p.issuer, authErr = (hubProjectExecutor{s}).projectScope(authority.BindContext(ctx))
			p.issuer.Project = projectID
		}
	}
	refusal := "The issuing operator no longer has permission; the requested configuration was not confirmed."
	if authErr == nil && p.identityRevision != p.command.RunnerRevision {
		authErr = corestore.ErrRevisionConflict
		refusal = "The runner revision changed; the requested configuration was not confirmed."
	}
	if authErr == nil && operation == "apply_local_project_policy" {
		approved, err := s.readTenantProjectPolicy(ctx, org, projectID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			var failure *hubapi.NativeError
			if !errors.As(err, &failure) || failure.Code != "policy_not_approved" {
				return nil, err
			}
		}
		if err != nil || p.intent.Desired.EffectivePolicy == nil || approved.Policy.ID != p.intent.Desired.EffectivePolicy.ID {
			authErr = operatortool.ErrAccessDenied
			refusal = "The approved policy changed; the requested configuration was not confirmed."
		}
	}
	if authErr != nil && !errors.Is(authErr, operatortool.ErrAccessDenied) && !errors.Is(authErr, corestore.ErrRevisionConflict) {
		return nil, authErr
	}
	p.deliver = authErr == nil
	var result runnerauth.ProjectConfiguration
	if authErr != nil {
		result = p.intent.Previous
		result.Constraint = refusal
		p.settle = true
	} else if reported != nil && reported.RequestID == p.command.RequestID {
		if reported.Applied && reported.Constraint == "" && configurationRuntimeMatches(*reported, p.intent.Desired) {
			result = p.intent.Desired
			result.Applied = true
			result.Saved = true
			p.settle = true
		} else if !reported.Applied && reported.Constraint != "" && configurationRuntimeMatches(*reported, p.intent.Previous) {
			result = p.intent.Previous
			result.Constraint = reported.Constraint
			p.settle = true
		}
	}
	if !p.settle {
		return p, nil
	}
	result.Pending = false
	result.RequestID = p.command.RequestID
	result.Authority = "hub_configuration"
	result.ObservedAt = s.config.Clock().UTC()
	p.view = result
	p.payload, err = s.prepareConfigurationPayload(ctx, org, result)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	now := s.config.Clock().UTC()
	p.completion, err = eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: org, ActorID: p.command.ActorID, Operation: "configuration result " + p.command.Operation, CommandKey: p.command.RequestID, RequestHash: p.command.RequestHash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
	if err != nil {
		return nil, errors.Join(errConfigurationPayloadUnavailable, err)
	}
	return p, err
}

func configurationRuntimeMatches(actual, desired runnerauth.ProjectConfiguration) bool {
	return actual.ConfigRevision == desired.ConfigRevision && actual.Registered == desired.Registered && actual.RuntimeRegistered == desired.Registered && actual.Paused == desired.Paused && actual.Draining == desired.Draining && actual.AllowLocalBinding == desired.AllowLocalBinding && actual.EffectivePolicy != nil && desired.EffectivePolicy != nil && actual.EffectivePolicy.ID == desired.EffectivePolicy.ID
}

func (s *Service) applyConfigurationHeartbeat(ctx context.Context, w tenantstore.Writer, identity runnerstore.Identity, projectID tracker.ProjectID, p *configurationHeartbeat, snapshot *runnerauth.RoutingSnapshot) error {
	if p == nil {
		return nil
	}
	owner, ok := w.(postgres.ConfigurationWriter)
	if !ok {
		return hubapi.ErrProjectServiceUnavailable
	}
	now := s.config.Clock().UTC()
	authority := postgres.ConfigurationAuthority{RunnerID: identity.RunnerID, EnrollmentID: identity.EnrollmentID, Revision: identity.Revision, At: now}
	row, err := owner.ConfigurationProject(ctx, identity.RunnerID, projectID)
	if p.initialize {
		if err == nil {
			return corestore.ErrRevisionConflict
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		_, err = owner.InitializeConfiguration(ctx, postgres.ConfigurationHeader{OrganizationID: identity.OrganizationID, RunnerID: identity.RunnerID, SourceRevision: p.view.ConfigRevision, CreatedAt: now}, authority)
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if row.Revision != p.record.Revision {
		return corestore.ErrRevisionConflict
	}
	if !p.initialize {
		current, err := owner.ConfigurationCommand(ctx, identity.RunnerID, projectID)
		if err != nil {
			return err
		}
		if current.RequestID != p.command.RequestID || current.Status != p.command.Status || current.RequestHash != p.command.RequestHash {
			return corestore.ErrRevisionConflict
		}
	}
	if p.command.Status == "pending" && p.deliver {
		if identity.Revision != p.command.RunnerRevision {
			return corestore.ErrRevisionConflict
		}
		if err := s.configurationRequestAuthority(ctx, w, p.issuer, now, strings.Split(p.command.Operation, " ")[0]); err != nil {
			return err
		}
		if strings.Split(p.command.Operation, " ")[0] == "apply_local_project_policy" {
			approved, err := w.ProjectPolicy(ctx, projectID)
			if err != nil {
				return err
			}
			if p.intent.Desired.EffectivePolicy == nil || approved.PolicyID != p.intent.Desired.EffectivePolicy.ID {
				return corestore.ErrRevisionConflict
			}
		}
	}
	current := !p.initialize
	if p.settle && !p.view.Applied {
		current = false
		if err := owner.RestoreConfiguration(ctx, configurationProjectRow(identity.OrganizationID, identity.RunnerID, p.view, p.payload, now), p.command, authority, row.Revision); err != nil {
			return err
		}
	} else if p.initialize || p.settle {
		current = false
		header, err := owner.ConfigurationHeader(ctx, identity.RunnerID)
		if err != nil {
			return err
		}
		_, err = owner.ApplyConfiguration(ctx, postgres.ConfigurationChange{OrganizationID: identity.OrganizationID, Authority: authority, ExpectedRevision: header.Revision, ExpectedSourceRevision: header.SourceRevision, SourceRevision: p.view.ConfigRevision, Projects: []postgres.ConfigurationProject{configurationProjectRow(identity.OrganizationID, identity.RunnerID, p.view, p.payload, now)}})
		if err != nil {
			return err
		}
	}
	readCurrent := func() error {
		if current {
			return nil
		}
		var err error
		row, err = owner.ConfigurationProject(ctx, identity.RunnerID, projectID)
		current = err == nil
		return err
	}
	if p.observation.Key != "" {
		if err := readCurrent(); err != nil {
			return err
		}
		if err := owner.StoreConfigurationObservationPayload(ctx, identity.RunnerID, projectID, p.observation, row.Revision, now); err != nil {
			return err
		}
	}
	if p.settle {
		if _, err := w.PutReceipt(ctx, p.completion); err != nil {
			return err
		}
		if err := readCurrent(); err != nil {
			return err
		}
		p.command.Status = "completed"
		if err := owner.SetConfigurationCommand(ctx, p.command, row.Revision); err != nil {
			return err
		}
	}
	if p.policy != nil {
		if err := writeConfigurationPolicyObservation(ctx, w, identity.RunnerID, *p.policy, now); err != nil {
			return err
		}
	}
	if p.reported != nil {
		if err := readCurrent(); err != nil {
			return err
		}
		reported := p.reported
		if reported.ConfigRevision == row.ConfigRevision && reported.EffectivePolicy != nil && reported.EffectivePolicy.ID == row.PolicyID {
			if _, err := owner.ObserveConfiguration(ctx, postgres.ConfigurationObservation{OrganizationID: identity.OrganizationID, RunnerID: identity.RunnerID, ProjectID: projectID, ConfigRevision: reported.ConfigRevision, PolicyID: reported.EffectivePolicy.ID, Registered: reported.RuntimeRegistered, Paused: reported.Paused, Draining: reported.Draining, ObservedAt: new(now)}, authority); err != nil {
				return err
			}
		}
	}
	view := p.view
	view.RunnerID = identity.RunnerID
	view.RunnerRevision = 0
	view.ObservedAt = now
	snapshot.ProjectConfiguration = &view
	if p.command.Status == "pending" && p.deliver && !p.settle {
		a := p.intent.Arguments
		desired := p.intent.Desired
		snapshot.ProjectConfigurationRequest = &runnerauth.ProjectConfigurationRequest{RequestID: a.RequestID, Operation: strings.Split(p.command.Operation, " ")[0], AllowLocalBinding: a.AllowLocalBinding, ProjectID: a.ProjectID, ExpectedConfigRevision: a.ExpectedConfigRevision, ExpectedPolicyID: a.ExpectedPolicyID, PolicyID: a.PolicyID, SourceRevision: a.SourceRevision, Checkpoint: a.Checkpoint, Desired: &desired}
	}
	return nil
}

func (s *Service) prepareConfigurationPolicyObservation(ctx context.Context, scope hubscope.NativeScope, observation policy.Observation) (policystore.Observation, error) {
	content := eventstore.NewContent(s.runDetailStore())
	revision, found, err := s.tenantProjectPolicyRevisionForDescriptor(ctx, scope.Organization, scope.Project, observation.Descriptor, content)
	if err != nil {
		return policystore.Observation{}, err
	}
	if !found {
		ref, err := s.prepareConfigurationPayload(ctx, scope.Organization, observation.Descriptor)
		if err != nil {
			return policystore.Observation{}, err
		}
		gate := observation.Gates.GitHubPullRequest
		revision = policystore.Revision{OrganizationID: scope.Organization, ProjectID: scope.Project, PolicyID: observation.ID, GitHubPullRequest: &gate, Compatibility: policystore.CompatibilityOf(observation.Descriptor), PayloadObjectKey: ref.Key, PayloadSHA256: ref.SHA256, PayloadBytes: ref.CompressedSize, PayloadContentBytes: ref.UncompressedSize}
	}
	return policystore.Observation{OrganizationID: scope.Organization, ProjectID: scope.Project, PolicyID: observation.ID, Source: observation.Source, Revision: revision}, nil
}

func (s *Service) recordConfigurationPolicyObservation(ctx context.Context, scope hubscope.NativeScope, observation policy.Observation) error {
	credential := scope.Credential
	prepared, err := s.prepareConfigurationPolicyObservation(ctx, scope, observation)
	if err != nil {
		return err
	}
	now := s.config.Clock().UTC()
	return s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		locker, ok := w.(runnerstore.UpdateLocker)
		if !ok {
			return hubapi.ErrProjectServiceUnavailable
		}
		if err := locker.LockRunnerUpdate(ctx, credential.Runner.RunnerID); err != nil {
			return err
		}
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		identity, err := w.RunnerIdentityByCredential(ctx, credential.ID)
		if err != nil {
			return err
		}
		if identity.RunnerID != credential.Runner.RunnerID || identity.OrganizationID != scope.Organization || identity.RemovedAt != nil || !identity.ExpiresAt.After(now) {
			return hubscope.RunnerUnauthorized()
		}
		return writeConfigurationPolicyObservation(ctx, w, identity.RunnerID, prepared, now)
	})
}

func writeConfigurationPolicyObservation(ctx context.Context, w tenantstore.Writer, runnerID string, observation policystore.Observation, now time.Time) error {
	allowed, err := w.RunnerProjectAuthorized(ctx, runnerID, observation.ProjectID, now)
	if err != nil {
		return err
	}
	if !allowed {
		return hubapi.NativeNotFound()
	}
	observation.RunnerID, observation.ObservedAt = runnerID, now
	_, err = w.RecordProjectPolicyObservation(ctx, observation)
	return err
}

func validateConfigurationRunner(view *runnerauth.ProjectConfiguration, identity runnerstore.Identity) error {
	if view != nil && (view.RunnerID != "" && view.RunnerID != identity.RunnerID || view.RunnerRevision != 0 && view.RunnerRevision != identity.Revision) {
		return hubapi.NativePublicInvalid("Project diagnostics do not match the current runner identity")
	}
	return nil
}
