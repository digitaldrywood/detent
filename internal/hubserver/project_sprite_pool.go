package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const spriteSandboxUnavailableReason = "Sandbox on a Fly Sprite needs a passing isolation probe on that runner: update the runner, then switch it to Sandbox once it reports sandbox support."

type spritePoolSettings struct {
	ShareIdleCapacity bool              `json:"share_idle_capacity"`
	IsolationTier     string            `json:"isolation_tier"`
	Placement         *policy.Placement `json:"placement,omitempty"`
	MinRunners        int               `json:"min_runners"`
	MaxRunners        int               `json:"max_runners"`
	IdleSeconds       int               `json:"idle_seconds"`
	Bootstrap         string            `json:"bootstrap"`
	Revision          int64             `json:"revision"`
}

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
	IsolationTierUnavailableReason string             `json:"isolation_tier_unavailable_reason,omitempty"`
	Members                        []spritePoolMember `json:"members"`
	Decision                       *placementDecision `json:"placement_decision,omitempty"`
	RunnerSetupDeclared            *bool              `json:"runner_setup_declared"`
}

type spriteScaleInput struct {
	Depth, Free, Pending, Count, Floor, Ceiling, Idle int
	CreateLimit                                       *int
}

type spriteScaleDecision struct {
	Create, Delete int
}

func decideSpriteScale(input spriteScaleInput) spriteScaleDecision {
	create := min(max(input.Floor-input.Count, input.Depth-input.Free-input.Pending, 0), max(input.Ceiling-input.Count, 0))
	if input.CreateLimit != nil {
		create = min(create, *input.CreateLimit)
	}
	if create > 0 {
		return spriteScaleDecision{Create: create}
	}
	surplus := max(input.Free+input.Pending-input.Depth, 0)
	if input.Depth == 0 {
		surplus = input.Idle
	}
	return spriteScaleDecision{Delete: min(input.Idle, max(input.Count-input.Floor, 0), max(surplus, input.Count-input.Ceiling))}
}

func (s *Service) registerSpritePoolRoutes(e *echo.Echo) {
	read := s.requireNativeScope(apiScopeOperator, apiScopeAdmin)
	e.GET(nativeBase+"/sprite-pool", s.getSpritePool, read)
	e.PUT(nativeBase+"/sprite-pool", s.setSpritePool, read)
	e.GET(nativeBase+"/sprite-pool/allocation", s.getProjectSpriteAllocation, read)
	e.PUT(nativeBase+"/sprite-pool/allocation", s.setProjectSpriteAllocation, read)
}

func (s *Service) getSpritePool(c echo.Context) error {
	view, err := s.spritePoolResponse(c.Request().Context(), nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, view)
}

func (s *Service) spritePoolResponse(ctx context.Context, scope nativeScope) (spritePoolView, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	view, err := s.readSpritePool(ctx, scope)
	if err != nil {
		return view, err
	}
	decision, err := readOrganizationPlacementSnapshot(ctx, s.database.reader, scope, s.config.now())
	if err != nil {
		return view, err
	}
	view.Decision = &decision.placementDecision
	if err := s.describeSpritePoolMembers(ctx, scope, view.Members); err != nil {
		return view, err
	}
	view.RunnerSetupDeclared, err = s.projectRunnerSetupDeclaration(ctx, scope)
	return view, err
}

func (s *Service) readSpritePool(ctx context.Context, scope nativeScope) (spritePoolView, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	settings, err := readSpritePoolSettings(ctx, s.database.reader, scope)
	view := spritePoolView{spritePoolSettings: settings, Members: []spritePoolMember{}}
	if err != nil {
		return view, err
	}
	if view.IsolationTier == isolation.Sandbox {
		view.IsolationTierUnavailableReason = spriteSandboxUnavailableReason
	}
	rows, err := s.database.reader.QueryContext(ctx, `SELECT m.allocation_project_id,m.name,m.state,m.bootstrap_log,m.enrollment_id,COALESCE(r.id,''),m.idle_since FROM organization_sprite_members m LEFT JOIN runner_identities r ON r.enrollment_id=m.enrollment_id WHERE m.organization_id=? AND (m.state<>'deleted' OR m.name IN (SELECT name FROM organization_sprite_members WHERE organization_id=m.organization_id AND state='deleted' ORDER BY created_at DESC,name DESC LIMIT 20)) ORDER BY m.created_at,m.name`, scope.organization)
	if err != nil {
		return view, err
	}
	defer rows.Close()
	for rows.Next() {
		var member spritePoolMember
		var idle string
		if err := rows.Scan(&member.AllocationProjectID, &member.Name, &member.State, &member.BootstrapLog, &member.EnrollmentID, &member.RunnerID, &idle); err != nil {
			return view, err
		}
		member.IdleSince, err = parseTimeValue(idle)
		if err != nil {
			return view, err
		}
		view.Members = append(view.Members, member)
	}
	return view, rows.Err()
}

func readSpritePoolSettings(ctx context.Context, q nativeQueryer, scope nativeScope) (spritePoolSettings, error) {
	settings := spritePoolSettings{IsolationTier: isolation.NativeTrusted, IdleSeconds: 300, Placement: &policy.Placement{Mode: "blended"}}
	var placement string
	err := q.QueryRowContext(ctx, `SELECT min_runners,max_runners,idle_seconds,bootstrap,revision,placement_json,isolation_tier,share_idle_capacity FROM organization_sprite_pools WHERE organization_id=?`, scope.organization).Scan(&settings.MinRunners, &settings.MaxRunners, &settings.IdleSeconds, &settings.Bootstrap, &settings.Revision, &placement, &settings.IsolationTier, &settings.ShareIdleCapacity)
	if errors.Is(err, sql.ErrNoRows) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	if err := json.Unmarshal([]byte(placement), settings.Placement); err != nil {
		return settings, err
	}
	resolved := settings.Placement.Resolved()
	settings.Placement = &resolved
	return settings, nil
}

func (s *Service) setSpritePool(c echo.Context) error {
	scope := nativeRequestScope(c)
	if !canManageProjectSecrets(scope.credential) {
		return s.refusedAPIError(c, http.StatusForbidden, "forbidden", "Sprite pool settings require owner or admin access", apiErrorResponse{Code: "forbidden", Message: "Sprite pool settings require owner or admin access"})
	}
	var input spritePoolCommand
	if err := decodeAPIJSON(c, &input); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	view, err := s.setSpritePoolCommand(c.Request().Context(), scope, input)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, view)
}

func (s *Service) setSpritePoolCommand(ctx context.Context, scope nativeScope, input spritePoolCommand) (spritePoolView, error) {
	if input.ShareIdleCapacity != nil || input.IdempotencyKey != "" {
		if err := s.executeSpritePoolCommand(ctx, scope, input); err != nil {
			return spritePoolView{}, err
		}
		s.startSpritePoolForQueue(scope)
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
	s.startSpritePoolForQueue(scope)
	return s.spritePoolResponse(ctx, scope)
}

func (s *Service) executeSpritePoolCommand(ctx context.Context, scope nativeScope, command spritePoolCommand) error {
	return s.executeSpritePoolSettingsCommand(ctx, scope, command.Mutation, command, func(ctx context.Context, tx *sql.Tx, scope nativeScope) (spritePoolSettings, *string, error) {
		settings := command.spritePoolSettings
		settings.IsolationTier = isolation.NativeTrusted
		err := tx.QueryRowContext(ctx, `SELECT bootstrap,isolation_tier,share_idle_capacity FROM organization_sprite_pools WHERE organization_id=?`, scope.organization).Scan(&settings.Bootstrap, &settings.IsolationTier, &settings.ShareIdleCapacity)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return settings, nil, err
		}
		if command.Bootstrap != nil {
			settings.Bootstrap = *command.Bootstrap
		}
		if command.ShareIdleCapacity != nil {
			settings.ShareIdleCapacity = *command.ShareIdleCapacity
		}
		return settings, command.IsolationTier, nil
	})
}

func (s *Service) executeSpritePoolSettingsCommand(ctx context.Context, scope nativeScope, command tracker.Mutation, input any, resolve func(context.Context, *sql.Tx, nativeScope) (spritePoolSettings, *string, error)) error {
	scope.project = ""
	scope.requireHostedAdmin = true
	_, err := s.executeNativeMutation(ctx, scope, nativeCommandOptions{OperationID: "set_sprite_pool", Feature: "collaboration"}, command, input, func(ctx context.Context, tx *sql.Tx, scope nativeScope, _ time.Time) (any, error) {
		settings, tier, err := resolve(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		return true, s.updateSpritePoolInTx(ctx, tx, scope, settings, tier)
	})
	return err
}

func validateSpritePoolAccess(tier *string) error {
	if tier != nil && *tier == isolation.Sandbox {
		return spritePoolInvalid(spriteSandboxUnavailableReason)
	}
	return nil
}

func validateSpritePoolSettings(settings *spritePoolSettings) error {
	if settings.IsolationTier == "" {
		settings.IsolationTier = isolation.NativeTrusted
	}
	if settings.IsolationTier != isolation.Sandbox && settings.IsolationTier != isolation.NativeTrusted {
		return spritePoolInvalid("Sprite pool access must be sandbox or native-trusted")
	}
	if settings.Placement != nil {
		resolved := settings.Placement.Resolved()
		if err := resolved.Validate(); err != nil {
			return spritePoolInvalid(err.Error())
		}
		settings.Placement = &resolved
	}
	if settings.IdleSeconds == 0 {
		settings.IdleSeconds = 300
	}
	if settings.MinRunners < 0 || settings.MaxRunners < settings.MinRunners || settings.IdleSeconds < 30 || settings.IdleSeconds > 86400 || len(settings.Bootstrap) > 65536 || settings.Revision < 0 {
		return spritePoolInvalid("Sprite pools require 0 <= min_runners <= max_runners, an idle threshold of 30 to 86400 seconds, optional bootstrap of at most 65536 bytes, and a nonnegative revision")
	}
	return nil
}

func (s *Service) updateSpritePool(ctx context.Context, scope nativeScope, settings spritePoolSettings, tier *string) error {
	scope.project = ""
	scope.requireHostedAdmin = true
	return s.secretMutation(ctx, scope, func(tx *sql.Tx) error {
		return s.updateSpritePoolInTx(ctx, tx, scope, settings, tier)
	})
}

func (s *Service) updateSpritePoolInTx(ctx context.Context, tx *sql.Tx, scope nativeScope, settings spritePoolSettings, tier *string) error {
	if err := validateSpritePoolAccess(tier); err != nil {
		return err
	}
	if tier != nil {
		settings.IsolationTier = *tier
	}
	if !canManageProjectSecrets(scope.credential) {
		return spritePoolInvalid("Sprite pool settings require owner or admin access")
	}
	if err := validateSpritePoolSettings(&settings); err != nil {
		return err
	}
	if settings.MaxRunners > 0 && (s.config.SecretKeys == nil || s.config.Hosted == nil) {
		return spritePoolInvalid("Sprite pools require hosted configuration and the project secret store")
	}
	if err := requireCredentialAuthority(ctx, tx, scope.credential, s.config.now()); err != nil {
		return err
	}
	actor := scope.credential.ID
	if scope.credential.HostedPrincipal != "" {
		actor = scope.credential.HostedPrincipal
	}
	var revision int64
	currentPlacement := `{"mode":"blended"}`
	err := tx.QueryRowContext(ctx, `SELECT revision,placement_json FROM organization_sprite_pools WHERE organization_id=?`, scope.organization).Scan(&revision, &currentPlacement)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if revision != settings.Revision {
		return nativeConflict(tracker.Revision(revision))
	}
	if settings.Placement != nil {
		raw, err := json.Marshal(settings.Placement)
		if err != nil {
			return err
		}
		currentPlacement = string(raw)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO organization_sprite_pools(organization_id,min_runners,max_runners,idle_seconds,bootstrap,configured_by,placement_json,isolation_tier,share_idle_capacity,revision) VALUES(?,?,?,?,?,?,?,?,?,1) ON CONFLICT(organization_id) DO UPDATE SET min_runners=excluded.min_runners,max_runners=excluded.max_runners,idle_seconds=excluded.idle_seconds,bootstrap=excluded.bootstrap,configured_by=excluded.configured_by,placement_json=excluded.placement_json,isolation_tier=excluded.isolation_tier,share_idle_capacity=excluded.share_idle_capacity,revision=organization_sprite_pools.revision+1`, scope.organization, settings.MinRunners, settings.MaxRunners, settings.IdleSeconds, settings.Bootstrap, actor, currentPlacement, settings.IsolationTier, settings.ShareIdleCapacity)
	return err
}

func (s *Service) spritePoolAuthority(ctx context.Context, query nativeQueryer, scope nativeScope) (string, error) {
	var actor string
	err := query.QueryRowContext(ctx, `SELECT p.configured_by FROM organization_sprite_pools p JOIN api_tokens t ON t.id=p.configured_by AND t.revoked_at IS NULL
WHERE p.organization_id=? AND (t.expires_at IS NULL OR julianday(t.expires_at)>julianday(?)) AND
((t.scope='admin' AND t.native_only=0) OR EXISTS (SELECT 1 FROM hosted_members m WHERE m.principal_id=t.id AND m.active=1 AND m.role IN ('owner','admin')))`, scope.organization, formatHubTime(s.config.now())).Scan(&actor)
	return actor, err
}

func (s *Service) spritePoolSnapshot(ctx context.Context, scope nativeScope, view spritePoolView) (spriteScaleInput, []spritePoolMember, error) {
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	defer cancelRead()
	allocation, err := readProjectSpriteAllocation(readCtx, s.database.reader, scope)
	if err != nil {
		return spriteScaleInput{}, nil, err
	}
	if !allocation.Managed {
		scope.project = ""
	}
	input := spriteScaleInput{Floor: view.MinRunners, Ceiling: view.MaxRunners}
	now := s.config.now()
	placement, err := readOrganizationPlacementSnapshot(readCtx, s.database.reader, scope, now)
	if err != nil {
		return input, nil, err
	}
	input.Depth, input.Free, input.Pending = placement.SpriteTarget, placement.SpriteFree, placement.Pending
	input.CreateLimit = placement.ProviderFree
	allowed, err := monthlySpriteAllowed(readCtx, s.database.reader, scope, now)
	if err != nil {
		return input, nil, err
	}
	decision, err := checkMonthlyBudget(readCtx, s.database.reader, scope, budget.CostExposure{SpriteInfrastructure: true}, false, now)
	cancelRead()
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
	if scope.project != "" {
		allocation, err := readProjectSpriteAllocation(ctx, s.database.reader, scope)
		if err != nil {
			return input, nil, err
		}
		input.Depth, input.Free, input.Pending = 0, 0, 0
		input.Floor, input.Ceiling = allocation.DesiredCount, allocation.DesiredCount
	}
	otherMembers := 0
	for _, member := range view.Members {
		if member.AllocationProjectID != scope.project && member.State != "deleted" && member.State != "deleting" {
			otherMembers++
		}
	}
	input.Ceiling = min(input.Ceiling, max(view.MaxRunners-otherMembers, 0))
	input.Floor = min(input.Floor, input.Ceiling)
	idle := []spritePoolMember{}
	retirement := map[string]int{}
	for _, member := range view.Members {
		if member.State == "deleted" || member.State == "deleting" || member.AllocationProjectID != scope.project {
			continue
		}
		if member.State == "bootstrapping" {
			if scope.project == "" {
				input.Count++
			}
			continue
		}
		input.Count++
		if member.RunnerID == "" {
			if scope.project != "" {
				idle = append(idle, member)
			}
			continue
		}
		runner, retained, err := s.spritePoolMemberRetention(ctx, scope, member.RunnerID)
		if err != nil {
			return input, nil, err
		}
		if runner.HostUsed > 0 || retained {
			_, err := s.database.db.ExecContext(ctx, `UPDATE organization_sprite_members SET idle_since=? WHERE organization_id=? AND name=?`, formatHubTime(now), scope.organization, member.Name)
			if err != nil {
				return input, nil, err
			}
			if scope.project != "" {
				retirement[member.Name] = 2
				idle = append(idle, member)
			}
			continue
		}
		if scope.project != "" {
			ready, err := spriteAllocationRunnerAllowed(ctx, s.database.reader, scope, runner.MachineID)
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
	if scope.project != "" {
		slices.Reverse(idle)
		slices.SortStableFunc(idle, func(a, b spritePoolMember) int { return retirement[a.Name] - retirement[b.Name] })
	}
	input.Idle = len(idle)
	return input, idle, nil
}

func (s *Service) spritePoolMemberRetention(ctx context.Context, scope nativeScope, runnerID string) (runnerauth.Runner, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	runner, err := readRunnerWithClock(ctx, s.database.reader, scope.organization, runnerID, s.config.now)
	if err != nil {
		return runner, false, err
	}
	retained, err := monthlyBudgetRetainsRunner(ctx, s.database.reader, scope, runner.MachineID)
	return runner, retained, err
}

func (s *Service) scaleSpritePool(ctx context.Context, scope nativeScope, allowCreate bool) error {
	scope.project = ""
	view, err := s.readSpritePool(ctx, scope)
	if err != nil || view.Revision == 0 {
		return err
	}
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	_, err = s.spritePoolAuthority(readCtx, s.database.reader, scope)
	cancelRead()
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	for _, member := range view.Members {
		if member.State == "bootstrapping" {
			expires, err := s.spriteEnrollmentExpiry(ctx, member.EnrollmentID)
			if err != nil {
				return err
			}
			end, err := parseTimeValue(expires)
			if err != nil {
				return err
			}
			if !end.After(s.config.now()) {
				if err := s.retirePoolSpriteReservation(ctx, scope, member.Name, "Sprite provisioning did not finish before its enrollment expired\n", true); err != nil {
					return err
				}
				member.State = "deleting"
			}
		}
		if member.State == "deleting" {
			if err := s.deletePoolSprite(ctx, nativeScope{organization: scope.organization, project: member.AllocationProjectID}, member); err != nil {
				return err
			}
		}
	}
	view, err = s.readSpritePool(ctx, scope)
	if err != nil {
		return err
	}
	projects, err := readSpriteAllocationProjects(ctx, s.database.reader, scope)
	if err != nil {
		return err
	}
	retirement := append([]tracker.ProjectID{""}, projects...)
	slices.Reverse(retirement[1:])
	for _, project := range retirement {
		owner := scope
		owner.project = project
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
			if view, err = s.readSpritePool(ctx, scope); err != nil {
				return err
			}
		}
	}
	if !allowCreate && len(projects) == 0 {
		return nil
	}
	for range view.MaxRunners {
		view, err = s.readSpritePool(ctx, scope)
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

func (s *Service) spriteEnrollmentExpiry(ctx context.Context, enrollmentID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	var expires string
	err := s.database.reader.QueryRowContext(ctx, `SELECT expires_at FROM runner_enrollments WHERE id=?`, enrollmentID).Scan(&expires)
	return expires, err
}

func (s *Service) scheduleSpritePoolForQueue(ctx context.Context, scope nativeScope) {
	s.startSpriteLifecycle(scope, "organization")
}

func (s *Service) maintainSpritePools(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	rows, err := s.database.reader.QueryContext(ctx, `SELECT organization_id FROM organization_sprite_members WHERE state<>'deleted' UNION SELECT organization_id FROM project_sprite_allocations WHERE desired_count>0`)
	if err != nil {
		return
	}
	defer rows.Close()
	var scopes []nativeScope
	for rows.Next() {
		var scope nativeScope
		if err := rows.Scan(&scope.organization); err != nil {
			return
		}
		scopes = append(scopes, scope)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return
	}
	for _, scope := range scopes {
		s.startSpriteLifecycle(scope, "")
	}
}

func (s *Service) describeSpritePoolMembers(ctx context.Context, scope nativeScope, members []spritePoolMember) error {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	now := s.config.now()
	for i := range members {
		member := &members[i]
		member.Status, member.WaitingFor = member.State, nil
		switch member.State {
		case "bootstrapping":
			member.Status = "provisioning"
			continue
		case "deleting":
			held, err := spriteMemberHoldsWork(ctx, s.database.reader, scope.organization, member.EnrollmentID)
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
		runner, err := readRunnerWithClock(ctx, s.database.reader, scope.organization, member.RunnerID, s.config.now)
		if err != nil {
			return err
		}
		var failed string
		var raw sql.NullString
		if err := s.database.reader.QueryRowContext(ctx, `SELECT COALESCE(json_extract(m.capabilities_json,'$.sprite_wake_failed_at'),''),json_extract(r.local_checks_json,?) FROM runner_identities r JOIN machines m ON m.id=r.machine_id WHERE r.id=? AND r.organization_id=?`, "$."+jsonPathKey(string(member.AllocationProjectID)), member.RunnerID, scope.organization).Scan(&failed, &raw); err != nil {
			return err
		}
		failedAt, parseErr := parseTimeValue(failed)
		if runner.State != "active" || runner.Health == "revoked" || runner.Health == "expired" || parseErr == nil && !failedAt.IsZero() && !failedAt.Before(runner.LastHeartbeatAt) {
			continue
		}
		if member.AllocationProjectID != "" {
			checks, observed, err := decodeSpriteMemberChecks(raw)
			if err != nil {
				return err
			}
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
	return &nativeError{Code: "invalid_request", Message: message, status: http.StatusUnprocessableEntity, publicMessage: true}
}
