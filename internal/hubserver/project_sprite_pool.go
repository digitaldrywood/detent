package hubserver

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type spritePoolCommand struct {
	tracker.Mutation
	spritePoolSettings
	Bootstrap         *string `json:"bootstrap"`
	IsolationTier     *string `json:"isolation_tier"`
	ShareIdleCapacity *bool   `json:"share_idle_capacity"`
}

type spritePoolMember struct {
	Name                string            `json:"name"`
	AllocationProjectID tracker.ProjectID `json:"allocation_project_id,omitempty"`
	State               string            `json:"state"`
	BootstrapLog        string            `json:"bootstrap_log"`
	RunnerID            string            `json:"runner_id"`
	IdleSince           time.Time         `json:"idle_since"`
	Status              string            `json:"status,omitempty"`
	WaitingFor          []spriteLoginStep `json:"waiting_for,omitempty"`
	EnrollmentID        string            `json:"-"`
}

type spriteLoginStep struct {
	Check   string `json:"check"`
	Command string `json:"command"`
}

type spritePoolView struct {
	spritePoolSettings
	IsolationTierUnavailableReason string                        `json:"isolation_tier_unavailable_reason,omitempty"`
	Members                        []spritePoolMember            `json:"members"`
	Decision                       *hubrecords.PlacementDecision `json:"placement_decision,omitempty"`
	RunnerSetupDeclared            *bool                         `json:"runner_setup_declared"`
}

func (s *Service) registerSpritePoolRoutes(e *echo.Echo) {
	read := s.requireNativeScope(hubapi.APIScopeOperator, hubapi.APIScopeAdmin)
	e.GET(hubapi.NativeBase+"/sprite-pool", s.getSpritePool, read)
	e.PUT(hubapi.NativeBase+"/sprite-pool", s.setSpritePool, read)
	e.GET(hubapi.NativeBase+"/sprite-pool/allocation", s.getProjectSpriteAllocation, read)
	e.PUT(hubapi.NativeBase+"/sprite-pool/allocation", s.setProjectSpriteAllocation, read)
}

func (s *Service) getSpritePool(c echo.Context) error {
	view, err := s.spritePoolResponse(c.Request().Context(), hubscope.NativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, view)
}

func (s *Service) spritePoolResponse(ctx context.Context, scope hubscope.NativeScope) (spritePoolView, error) {
	view, err := s.readSpritePool(ctx, scope)
	if err != nil {
		return view, err
	}
	decision, err := s.readOrganizationPlacementSnapshot(ctx, scope, s.config.Clock())
	if err != nil {
		return view, err
	}
	view.Decision = &decision.PlacementDecision
	if err := s.describeSpritePoolMembers(ctx, scope, view.Members); err != nil {
		return view, err
	}
	if scope.Project != "" {
		view.RunnerSetupDeclared, err = s.projectRunnerSetupDeclaration(ctx, scope)
	}
	return view, err
}

func (s *Service) readSpritePool(ctx context.Context, scope hubscope.NativeScope) (spritePoolView, error) {
	view, _, err := s.readSpritePoolRecord(ctx, scope, true)
	return view, err
}

func (s *Service) setSpritePool(c echo.Context) error {
	scope := hubscope.NativeRequestScope(c)
	if !hubscope.CanManageProjectSecrets(scope.Credential) {
		return s.refusedAPIError(c, http.StatusForbidden, "forbidden", "Sprite pool settings require owner or admin access", hubapi.APIErrorResponse{Code: "forbidden", Message: "Sprite pool settings require owner or admin access"})
	}
	var input spritePoolCommand
	if err := hubapi.DecodeAPIJSON(c, &input); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	view, err := s.setSpritePoolCommand(c.Request().Context(), scope, input)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, view)
}

func (s *Service) setSpritePoolCommand(ctx context.Context, scope hubscope.NativeScope, input spritePoolCommand) (spritePoolView, error) {
	if input.ShareIdleCapacity != nil || input.IdempotencyKey != "" {
		if err := s.executeSpritePoolCommand(ctx, scope, input); err != nil {
			return spritePoolView{}, err
		}
		s.startSpritePoolForQueue(ctx, scope)
		return s.spritePoolResponse(ctx, scope)
	}
	settings := input.spritePoolSettings
	view, err := s.readSpritePool(ctx, scope)
	if err != nil {
		return view, err
	}
	settings.IsolationTier = view.IsolationTier
	settings.ShareIdleCapacity = view.ShareIdleCapacity
	if input.Bootstrap != nil {
		settings.Bootstrap = *input.Bootstrap
	} else {
		settings.Bootstrap = view.Bootstrap
	}
	if err := s.updateSpritePool(ctx, scope, settings, input.IsolationTier); err != nil {
		return view, err
	}
	s.startSpritePoolForQueue(ctx, scope)
	return s.spritePoolResponse(ctx, scope)
}

func (s *Service) executeSpritePoolCommand(ctx context.Context, scope hubscope.NativeScope, command spritePoolCommand) error {
	return s.executeSpritePoolSettingsCommand(ctx, scope, command.Mutation, command, func(current spritePoolSettings) (spritePoolSettings, *string, error) {
		settings := command.spritePoolSettings
		settings.Bootstrap, settings.IsolationTier, settings.ShareIdleCapacity = current.Bootstrap, current.IsolationTier, current.ShareIdleCapacity
		if command.Bootstrap != nil {
			settings.Bootstrap = *command.Bootstrap
		}
		if command.ShareIdleCapacity != nil {
			settings.ShareIdleCapacity = *command.ShareIdleCapacity
		}
		return settings, command.IsolationTier, nil
	})
}

func (s *Service) executeSpritePoolSettingsCommand(ctx context.Context, scope hubscope.NativeScope, command tracker.Mutation, input any, resolve func(spritePoolSettings) (spritePoolSettings, *string, error)) error {
	scope.Project = ""
	scope.RequireHostedAdmin = true
	if s.config.TenantRepository == nil || s.config.AccessRepository == nil {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return err
	}
	keyed := input != nil
	var hash string
	var err error
	if keyed {
		if strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 128 {
			return hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
		}
		hash, err = hubapi.CommandHash(input)
		if err != nil {
			return err
		}
		found, err := s.spritePoolCommandReceipt(ctx, scope, command.IdempotencyKey, hash)
		if err != nil || found {
			return err
		}
	}
	current, row, err := s.readSpritePoolRecord(ctx, scope, false)
	if err != nil {
		return err
	}
	settings, tier, err := resolve(current.spritePoolSettings)
	if err != nil {
		return err
	}
	if err := validateSpritePoolAccess(tier); err != nil {
		return err
	}
	if tier != nil {
		settings.IsolationTier = *tier
	}
	if !hubscope.CanManageProjectSecrets(scope.Credential) {
		return spritePoolInvalid("Sprite pool settings require owner or admin access")
	}
	if err := validateSpritePoolSettings(&settings); err != nil {
		return err
	}
	if settings.MaxRunners > 0 && (s.config.SecretKeys == nil || s.config.Hosted == nil) {
		return spritePoolInvalid("Sprite pools require hosted configuration and the project secret store")
	}
	if settings.MaxRunners > 100 {
		return spritePoolInvalid("Sprite pools support at most 100 runners")
	}
	if settings.Placement != nil && (int64(settings.Placement.TodoThreshold) > 2147483647 || int64(settings.Placement.OverflowSlots) > 2147483647 || int64(settings.Placement.MaxSpriteSlots) > 2147483647) {
		return spritePoolInvalid("Sprite placement values exceed the supported integer range")
	}
	if settings.Revision != row.Revision {
		return hubapi.NativeConflict(tracker.Revision(row.Revision))
	}
	ctx = s.contentContext(ctx)
	var bootstrap attachment.ArchiveReference
	bootstrapChanged := settings.Bootstrap != current.Bootstrap
	if bootstrapChanged && settings.Bootstrap != "" {
		bootstrap, err = prepareChangeContent(ctx, string(scope.Organization), []byte(settings.Bootstrap))
		if err != nil {
			return err
		}
	}
	var receipt eventstore.Receipt
	if keyed {
		receipt, err = eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: "set_sprite_pool", CommandKey: command.IdempotencyKey, RequestHash: hash}, []byte("true"))
		if err != nil {
			return err
		}
	}
	oldBootstrapKey := row.BootstrapObjectKey
	row.OrganizationID = scope.Organization
	row.MinRunners, row.MaxRunners, row.IdleSeconds = int32(settings.MinRunners), int32(settings.MaxRunners), int32(settings.IdleSeconds)
	row.IsolationTier, row.ShareIdleCapacity = settings.IsolationTier, settings.ShareIdleCapacity
	row.ConfiguredBy = scope.Credential.ID
	if scope.Credential.HostedPrincipal != "" {
		row.ConfiguredBy = scope.Credential.HostedPrincipal
	}
	if settings.Placement != nil {
		placement := settings.Placement.Resolved()
		row.PlacementMode, row.TodoThreshold, row.OverflowSlots, row.MaxSpriteSlots = placement.Mode, int32(placement.TodoThreshold), int32(placement.OverflowSlots), int32(placement.MaxSpriteSlots)
	}
	if bootstrapChanged {
		row.BootstrapObjectKey = nil
		if bootstrap.Key != "" {
			row.BootstrapObjectKey = new(bootstrap.Key)
		}
	}
	var replay *eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		if keyed {
			previous, err := w.Receipt(ctx, scope.Credential.ID, "set_sprite_pool", command.IdempotencyKey)
			if err == nil {
				if previous.RequestHash != hash {
					return hubapi.NativeConflict(0)
				}
				replay = &previous
				return nil
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
		}
		owner, ok := w.(postgres.SpriteWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if keyed {
			now := s.config.Clock()
			if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, w, "collaboration", now)); err != nil {
				return err
			}
			usageID := "sprite-pool/" + artifact.Digest([]byte(scope.Credential.ID+"\x00set_sprite_pool\x00"+command.IdempotencyKey))
			if err := s.applyRunnerTenantUsage(ctx, w, now, usageID, false, false); err != nil {
				return err
			}
		}
		if keyed || bootstrapChanged {
			if err := w.LockArchiveAccounting(ctx); err != nil {
				return err
			}
		}
		if _, err := owner.PutSpritePool(ctx, row, settings.Revision); err != nil {
			return err
		}
		if bootstrapChanged {
			if bootstrap.Key != "" {
				object, err := w.ArchiveObjectByReference(ctx, bootstrap)
				if err != nil {
					return err
				}
				if err := w.AddArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: scope.Organization, ArchiveID: object.ID, Kind: "sprite-pool", ID: string(scope.Organization), CreatedAt: s.config.Clock()}); err != nil {
					return err
				}
			}
			if oldBootstrapKey != nil {
				archives, ok := w.(hubrecords.AttemptArchiveReader)
				if !ok {
					return hubapi.NativeWorkItemStorageUnavailable()
				}
				object, err := archives.ArchiveObjectByKey(ctx, *oldBootstrapKey)
				if err != nil {
					return err
				}
				if err := w.RemoveArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: scope.Organization, ArchiveID: object.ID, Kind: "sprite-pool", ID: string(scope.Organization)}); err != nil {
					return err
				}
			}
		}
		if keyed {
			if scope.Credential.Hosted != nil {
				audit := projectRecordActor(scope).Audit
				audit.Event, audit.Route, audit.Status = "action", "set_sprite_pool", http.StatusOK
				audit.RecordedAt = s.config.Clock()
				audit.RecordedAtNS = audit.RecordedAt.UnixNano()
				if _, err := w.RecordAccessAudit(ctx, audit); err != nil {
					return err
				}
			}
			_, err := w.PutReceipt(ctx, receipt)
			return err
		}
		return nil
	})
	if replay != nil && err == nil {
		_, err = eventstore.NewContent(s.runDetailStore()).Receipt(ctx, *replay)
		return err
	}
	if err != nil && keyed {
		found, readErr := s.spritePoolCommandReceipt(ctx, scope, command.IdempotencyKey, hash)
		if readErr != nil {
			return readErr
		}
		if found {
			return nil
		}
	}
	if errors.Is(err, corestore.ErrRevisionConflict) {
		fresh, _, readErr := s.readSpritePoolRecord(ctx, scope, false)
		if readErr != nil {
			return readErr
		}
		return hubapi.NativeConflict(tracker.Revision(fresh.Revision))
	}
	return hubapi.ProjectRecordError(err)
}

func (s *Service) updateSpritePool(ctx context.Context, scope hubscope.NativeScope, settings spritePoolSettings, tier *string) error {
	return s.executeSpritePoolSettingsCommand(ctx, scope, tracker.Mutation{}, nil, func(spritePoolSettings) (spritePoolSettings, *string, error) { return settings, tier, nil })
}

func (s *Service) spritePoolSnapshot(ctx context.Context, scope hubscope.NativeScope, view spritePoolView) (spriteScaleInput, []spritePoolMember, error) {
	allocation, err := s.readSpriteAllocationController(ctx, scope)
	if err != nil {
		return spriteScaleInput{}, nil, err
	}
	if !allocation.Managed {
		scope.Project = ""
	}
	input := spriteScaleInput{Floor: view.MinRunners, Ceiling: view.MaxRunners}
	now := s.config.Clock()
	placement, err := s.readOrganizationPlacementSnapshot(ctx, scope, now)
	if err != nil {
		return input, nil, err
	}
	input.Depth, input.Free, input.Pending = placement.SpriteTarget, placement.SpriteFree, placement.Pending
	input.CreateLimit = placement.ProviderFree
	allowed, err := monthlySpriteAllowed(ctx, nil, scope, now, s.database.billingRepository)
	if err != nil {
		return input, nil, err
	}
	decision, err := checkMonthlyBudget(ctx, nil, scope, budget.CostExposure{SpriteInfrastructure: true}, false, now, s.database.billingRepository)
	if err != nil {
		return input, nil, err
	}
	if !decision.Allowed {
		input.CreateLimit = new(placement.Provisionable)
	}
	if !allowed {
		input.CreateLimit = new(0)
	}
	if placement.Policy.Mode == "local_first" {
		input.Floor = min(input.Floor, placement.Policy.OverflowSlots)
	}
	if scope.Project != "" {
		allocation, err := s.readSpriteAllocationController(ctx, scope)
		if err != nil {
			return input, nil, err
		}
		input.Depth, input.Free, input.Pending = 0, 0, 0
		input.Floor, input.Ceiling = allocation.DesiredCount, allocation.DesiredCount
	}
	otherMembers := 0
	for _, member := range view.Members {
		if member.AllocationProjectID != scope.Project && member.State != "deleted" && member.State != "deleting" {
			otherMembers++
		}
	}
	input.Ceiling = min(input.Ceiling, max(view.MaxRunners-otherMembers, 0))
	input.Floor = min(input.Floor, input.Ceiling)
	idle := []spritePoolMember{}
	retirement := map[string]int{}
	for _, member := range view.Members {
		if member.State == "deleted" || member.State == "deleting" || member.AllocationProjectID != scope.Project {
			continue
		}
		if member.State == "bootstrapping" {
			if scope.Project == "" {
				input.Count++
			}
			continue
		}
		input.Count++
		if member.RunnerID == "" {
			if scope.Project != "" {
				idle = append(idle, member)
			}
			continue
		}
		runner, retained, err := s.spritePoolMemberRetention(ctx, scope, member.RunnerID)
		if err != nil {
			return input, nil, err
		}
		if runner.HostUsed > 0 || retained {
			err := s.updateSpritePoolMember(ctx, scope, member.Name, func(current *runnerstore.SpriteMembership) error {
				if current.EnrollmentID != member.EnrollmentID {
					return corestore.ErrRevisionConflict
				}
				current.IdleSince = now
				return nil
			})
			if err != nil {
				return input, nil, err
			}
			if scope.Project != "" {
				retirement[member.Name] = 2
				idle = append(idle, member)
			}
			continue
		}
		if scope.Project != "" {
			ready, err := s.spriteAllocationRunnerAllowed(ctx, scope, runner.RunnerID)
			if err != nil {
				return input, nil, err
			}
			if ready {
				retirement[member.Name] = 1
			}
			idle = append(idle, member)
			continue
		}
		if now.Sub(member.IdleSince) >= time.Duration(view.IdleSeconds)*time.Second {
			idle = append(idle, member)
		}
	}
	if scope.Project != "" {
		slices.Reverse(idle)
		slices.SortStableFunc(idle, func(a, b spritePoolMember) int { return retirement[a.Name] - retirement[b.Name] })
	}
	input.Idle = len(idle)
	return input, idle, nil
}

func (s *Service) spritePoolMemberRetention(ctx context.Context, scope hubscope.NativeScope, runnerID string) (runnerauth.Runner, bool, error) {
	runner, err := s.spritePoolControllerRunner(ctx, scope, runnerID)
	if err != nil {
		return runner, false, err
	}
	retained, err := monthlyBudgetRetainsRunner(ctx, nil, scope, runner.MachineID, s.database.billingRepository)
	return runner, retained, err
}

func (s *Service) scaleSpritePool(ctx context.Context, scope hubscope.NativeScope, allowCreate bool) error {
	scope.Project = ""
	view, err := s.readSpritePoolController(ctx, scope)
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil || view.Revision == 0 {
		return err
	}
	_, err = s.spritePoolControllerAuthority(ctx, scope)
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	for _, member := range view.Members {
		if member.State == "bootstrapping" {
			expires, err := s.spriteEnrollmentExpiry(ctx, scope, member.EnrollmentID)
			if err != nil {
				return err
			}
			end, err := hubapi.ParseTimeValue(expires)
			if err != nil {
				return err
			}
			if !end.After(s.config.Clock()) {
				if err := s.retirePoolSpriteReservation(ctx, scope, member.Name, "Sprite provisioning did not finish before its enrollment expired\n", true); err != nil {
					return err
				}
				member.State = "deleting"
			}
		}
		if member.State == "deleting" {
			if err := s.deletePoolSprite(ctx, hubscope.NativeScope{Organization: scope.Organization, Project: member.AllocationProjectID}, member); err != nil {
				return err
			}
		}
	}
	view, err = s.readSpritePoolController(ctx, scope)
	if err != nil {
		return err
	}
	projects, err := s.readSpriteAllocationProjects(ctx, scope)
	if err != nil {
		return err
	}
	retirement := append([]tracker.ProjectID{""}, projects...)
	slices.Reverse(retirement[1:])
	for _, project := range retirement {
		owner := scope
		owner.Project = project
		input, idle, err := s.spritePoolSnapshot(ctx, owner, view)
		if err != nil {
			return err
		}
		decision := decideSpriteScale(input)
		for i := range decision.Delete {
			if err := s.deletePoolSprite(ctx, owner, idle[i]); err != nil {
				return err
			}
		}
		if decision.Delete > 0 {
			if view, err = s.readSpritePoolController(ctx, scope); err != nil {
				return err
			}
		}
	}
	if !allowCreate && len(projects) == 0 {
		return nil
	}
	for range view.MaxRunners {
		view, err = s.readSpritePoolController(ctx, scope)
		if err != nil {
			return err
		}
		created, err := s.createPoolSprite(ctx, scope, view.spritePoolSettings, allowCreate)
		if err != nil {
			return err
		}
		if !created {
			break
		}
	}
	return nil
}

func (s *Service) spriteEnrollmentExpiry(ctx context.Context, scope hubscope.NativeScope, enrollmentID string) (string, error) {
	var expires time.Time
	err := s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error {
		enrollment, err := r.RunnerEnrollment(ctx, enrollmentID)
		expires = enrollment.ExpiresAt
		return err
	})
	return hubapi.FormatHubTime(expires), err
}

func (s *Service) describeSpritePoolMembers(ctx context.Context, scope hubscope.NativeScope, members []spritePoolMember) error {
	now := s.config.Clock()
	for i := range members {
		member := &members[i]
		member.Status, member.WaitingFor = member.State, nil
		switch member.State {
		case "bootstrapping":
			member.Status = "provisioning"
			continue
		case "deleting":
			held := false
			err := s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error {
				owner, ok := r.(postgres.SpriteReader)
				if !ok {
					return hubapi.NativeWorkItemStorageUnavailable()
				}
				var err error
				held, err = owner.SpriteMemberHoldsWork(ctx, member.EnrollmentID)
				return err
			})
			if err != nil {
				return err
			}
			if held {
				member.Status = "draining"
			}
			continue
		case "enrolled":
		default:
			continue
		}
		member.Status = "unreachable"
		if member.RunnerID == "" {
			continue
		}
		runner, err := s.spritePoolRunner(ctx, scope, member.RunnerID)
		if err != nil {
			return err
		}
		facts, err := s.readSpriteMachineFacts(ctx, scope.Organization, runner.MachineID)
		if err != nil {
			return err
		}
		failed := spriteFactValue(facts.WakeFailedAt)
		var checks runnerauth.LocalChecks
		observed := false
		if err := s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error {
			var err error
			checks, err = r.RunnerLocalChecks(ctx, member.RunnerID, member.AllocationProjectID)
			observed = err == nil
			if errors.Is(err, corestore.ErrNotFound) {
				return nil
			}
			return err
		}); err != nil {
			return err
		}
		failedAt, parseErr := hubapi.ParseTimeValue(failed)
		if runner.State != "active" || runner.Health == "revoked" || runner.Health == "expired" || parseErr == nil && !failedAt.IsZero() && !failedAt.Before(runner.LastHeartbeatAt) {
			continue
		}
		if member.AllocationProjectID != "" {
			if member.WaitingFor = spriteLoginSteps(checks, observed, runner.Problems, member.AllocationProjectID); len(member.WaitingFor) > 0 {
				member.Status = "awaiting_login"
				continue
			}
			if !checks.WorkReady() {
				member.Status = "awaiting_setup"
				continue
			}
		}
		member.Status = "ready"
		if now.Sub(runner.LastHeartbeatAt) >= spriteRunnerIdle {
			member.Status = "sleeping"
		}
	}
	return nil
}

func spriteLoginSteps(checks runnerauth.LocalChecks, observed bool, problems []runnerauth.Problem, project tracker.ProjectID) []spriteLoginStep {
	var steps []spriteLoginStep
	if !observed || !checks.ProviderReady() {
		for _, problem := range problems {
			if problem.Message == "not signed in" && problem.FixCommand != "" && (problem.ProjectID == "" || problem.ProjectID == string(project)) && !slices.ContainsFunc(steps, func(step spriteLoginStep) bool { return step.Command == problem.FixCommand }) {
				steps = append(steps, spriteLoginStep{Check: "provider", Command: problem.FixCommand})
			}
		}
		if len(steps) == 0 && observed {
			for _, kind := range checks.ProviderKinds {
				switch kind {
				case "codex":
					steps = append(steps, spriteLoginStep{Check: "provider", Command: "codex login --device-auth"})
				case "claude_code", "claude-code":
					steps = append(steps, spriteLoginStep{Check: "provider", Command: "claude auth login"})
				}
			}
		}
		if len(steps) == 0 && len(checks.ProviderKinds) == 0 {
			steps = append(steps, spriteLoginStep{Check: "provider", Command: "codex login --device-auth"})
		}
	}
	if !observed || checks.Checkout != "passed" {
		steps = append(steps, spriteLoginStep{Check: "github", Command: "gh auth login"}, spriteLoginStep{Check: "github", Command: "gh auth setup-git"})
	}
	return steps
}

func spritePoolInvalid(message string) error {
	return &hubapi.NativeError{Code: "invalid_request", Message: message, Status: http.StatusUnprocessableEntity, PublicMessage: true}
}

func (s *Service) readSpritePoolRecord(ctx context.Context, scope hubscope.NativeScope, members bool) (spritePoolView, postgres.SpritePool, error) {
	return s.readSpritePoolSnapshot(ctx, scope, members, func(r tenantstore.Reader) error { return s.authorizeAccessScope(ctx, r, scope, false) })
}

func (s *Service) readSpritePoolController(ctx context.Context, scope hubscope.NativeScope) (spritePoolView, error) {
	view, _, err := s.readSpritePoolSnapshot(ctx, scope, true, func(r tenantstore.Reader) error {
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		_, err := owner.SpritePoolAuthority(ctx, s.config.Clock())
		return err
	})
	return view, err
}

func (s *Service) readSpritePoolSnapshot(ctx context.Context, scope hubscope.NativeScope, members bool, authorize func(tenantstore.Reader) error) (spritePoolView, postgres.SpritePool, error) {
	view := spritePoolView{Members: []spritePoolMember{}}
	row := postgres.SpritePool{OrganizationID: scope.Organization, IdleSeconds: 300, IsolationTier: isolation.NativeTrusted, PlacementMode: "blended"}
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return view, row, hubapi.NativeWorkItemStorageUnavailable()
	}
	var bootstrap attachment.ArchiveReference
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := authorize(r); err != nil {
			return err
		}
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		stored, err := owner.SpritePool(ctx)
		if err == nil {
			row = stored
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if row.BootstrapObjectKey != nil {
			archives, ok := r.(hubrecords.AttemptArchiveReader)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			object, err := archives.ArchiveObjectByKey(ctx, *row.BootstrapObjectKey)
			if err != nil {
				return err
			}
			if object.Reference.Key != *row.BootstrapObjectKey || object.Reference.UncompressedSize > 65536 {
				return artifact.ErrIntegrity
			}
			bootstrap = object.Reference
		}
		if members {
			rows, err := owner.SpritePoolMembers(ctx)
			if err != nil {
				return err
			}
			for _, item := range rows {
				view.Members = append(view.Members, spritePoolMember{Name: item.Name, AllocationProjectID: tracker.ProjectID(item.AllocationProjectID), State: item.State, BootstrapLog: item.BootstrapLog, EnrollmentID: item.EnrollmentID, RunnerID: item.RunnerID, IdleSince: item.IdleSince})
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) {
			return view, row, errors.Join(hubapi.ProjectRecordError(err), err)
		}
		return view, row, hubapi.ProjectRecordError(err)
	}
	placement := policy.Placement{Mode: row.PlacementMode, TodoThreshold: int(row.TodoThreshold), OverflowSlots: int(row.OverflowSlots), MaxSpriteSlots: int(row.MaxSpriteSlots)}.Resolved()
	view.spritePoolSettings = spritePoolSettings{MinRunners: int(row.MinRunners), MaxRunners: int(row.MaxRunners), IdleSeconds: int(row.IdleSeconds), Revision: row.Revision, IsolationTier: row.IsolationTier, ShareIdleCapacity: row.ShareIdleCapacity, Placement: &placement}
	if row.IsolationTier == isolation.Sandbox {
		view.IsolationTierUnavailableReason = spriteSandboxUnavailableReason
	}
	if bootstrap.Key != "" {
		raw, err := hubcontent.ReadChangeContent(s.contentContext(ctx), string(scope.Organization), bootstrap)
		if err != nil {
			return view, row, err
		}
		view.Bootstrap = string(raw)
		if err := s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error { return authorize(r) }); err != nil {
			return view, row, err
		}
	}
	return view, row, nil
}

func (s *Service) spritePoolCommandReceipt(ctx context.Context, scope hubscope.NativeScope, key, hash string) (bool, error) {
	var receipt eventstore.Receipt
	found := false
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		var err error
		receipt, err = r.Receipt(ctx, scope.Credential.ID, "set_sprite_pool", key)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil || !found {
		return found, err
	}
	if receipt.RequestHash != hash {
		return false, hubapi.NativeConflict(0)
	}
	_, err = eventstore.NewContent(s.runDetailStore()).Receipt(ctx, receipt)
	return true, err
}

func (s *Service) spritePoolRecordAuthority(ctx context.Context, scope hubscope.NativeScope, revision *int64) (string, error) {
	var actor string
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return actor, hubapi.NativeWorkItemStorageUnavailable()
	}
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if revision != nil {
			row, err := owner.SpritePool(ctx)
			if err != nil {
				return err
			}
			if row.Revision != *revision {
				return hubapi.NativeConflict(tracker.Revision(row.Revision))
			}
		}
		var err error
		actor, err = owner.SpritePoolAuthority(ctx, s.config.Clock())
		return err
	})
	return actor, err
}

func (s *Service) spritePoolRunner(ctx context.Context, scope hubscope.NativeScope, id string) (runnerauth.Runner, error) {
	var result runnerauth.Runner
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return result, hubapi.NativeWorkItemStorageUnavailable()
	}
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		owner, ok := r.(runnerstore.RoutingReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		now := s.config.Clock()
		record, err := owner.RunnerRoutingRecord(ctx, id, now)
		if err != nil {
			return err
		}
		urgent, err := r.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		result = runnerFromRoutingRecord(record, now, urgent)
		return nil
	})
	return result, hubapi.ProjectRecordError(err)
}

func (s *Service) updateSpritePoolMember(ctx context.Context, scope hubscope.NativeScope, name string, update func(*runnerstore.SpriteMembership) error) error {
	return s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		owner, ok := w.(postgres.SpriteReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		row, err := owner.SpriteMember(ctx, name)
		if err != nil {
			return err
		}
		member := runnerstore.SpriteMembership{OrganizationID: row.OrganizationID, Name: row.Name, ProviderOrganization: row.ProviderOrganization, TokenProjectID: tracker.ProjectID(row.TokenProjectID), EnrollmentID: row.EnrollmentID, State: row.State, BootstrapLog: row.BootstrapLog, IdleSince: row.IdleSince, CreatedAt: row.CreatedAt, AllocationProjectID: tracker.ProjectID(row.AllocationProjectID), AllocationRevision: row.AllocationRevision}
		if err := update(&member); err != nil {
			return err
		}
		return w.UpdateRunnerSpriteMembership(ctx, member)
	})
}

func (s *Service) spritePoolControllerAuthority(ctx context.Context, scope hubscope.NativeScope) (string, error) {
	var actor string
	err := s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error {
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		actor, err = owner.SpritePoolAuthority(ctx, s.config.Clock())
		return err
	})
	return actor, err
}

func (s *Service) readSpriteAllocationController(ctx context.Context, scope hubscope.NativeScope) (projectSpriteAllocation, error) {
	var result projectSpriteAllocation
	err := s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error {
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if _, err := owner.SpritePoolAuthority(ctx, s.config.Clock()); err != nil {
			return err
		}
		var err error
		result, _, err = projectSpriteAllocationRecord(ctx, r, scope)
		return err
	})
	return result, err
}

func (s *Service) spritePoolControllerRunner(ctx context.Context, scope hubscope.NativeScope, id string) (runnerauth.Runner, error) {
	var result runnerauth.Runner
	err := s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error {
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if _, err := owner.SpritePoolAuthority(ctx, s.config.Clock()); err != nil {
			return err
		}
		routing, ok := r.(runnerstore.RoutingReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		now := s.config.Clock()
		record, err := routing.RunnerRoutingRecord(ctx, id, now)
		if err != nil {
			return err
		}
		urgent, err := r.RunnerUrgentUpdate(ctx)
		if err == nil {
			result = runnerFromRoutingRecord(record, now, urgent)
		}
		return err
	})
	return result, err
}
