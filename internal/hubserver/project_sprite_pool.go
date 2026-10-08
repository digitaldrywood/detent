package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type spritePoolSettings struct {
	Placement   *policy.Placement `json:"placement,omitempty"`
	MinRunners  int               `json:"min_runners"`
	MaxRunners  int               `json:"max_runners"`
	IdleSeconds int               `json:"idle_seconds"`
	Bootstrap   string            `json:"bootstrap"`
	Revision    int64             `json:"revision"`
}

type spritePoolMember struct {
	Name         string    `json:"name"`
	State        string    `json:"state"`
	BootstrapLog string    `json:"bootstrap_log"`
	RunnerID     string    `json:"runner_id"`
	IdleSince    time.Time `json:"idle_since"`
	EnrollmentID string    `json:"-"`
}

type spritePoolView struct {
	spritePoolSettings
	Members             []spritePoolMember `json:"members"`
	Decision            *placementDecision `json:"placement_decision,omitempty"`
	RunnerSetupDeclared *bool              `json:"runner_setup_declared"`
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
	return spriteScaleDecision{Delete: min(input.Idle, max(input.Count-input.Floor, 0), surplus)}
}

func (s *Service) registerSpritePoolRoutes(e *echo.Echo) {
	read := s.requireNativeScope(apiScopeOperator, apiScopeAdmin)
	e.GET(nativeBase+"/sprite-pool", s.getSpritePool, read)
	e.PUT(nativeBase+"/sprite-pool", s.setSpritePool, read)
}

func (s *Service) getSpritePool(c echo.Context) error {
	view, err := s.readSpritePool(c.Request().Context(), nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	decision, err := readPlacementSnapshot(c.Request().Context(), s.database.db, nativeRequestScope(c), s.config.now(), nil)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	view.Decision = &decision.placementDecision
	view.RunnerSetupDeclared, err = s.projectRunnerSetupDeclaration(c.Request().Context(), nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, view)
}

func (s *Service) readSpritePool(ctx context.Context, scope nativeScope) (spritePoolView, error) {
	view := spritePoolView{spritePoolSettings: spritePoolSettings{IdleSeconds: 300, Placement: &policy.Placement{Mode: "blended"}}, Members: []spritePoolMember{}}
	var placement string
	err := s.database.db.QueryRowContext(ctx, `SELECT min_runners,max_runners,idle_seconds,bootstrap,revision,placement_json FROM project_sprite_pools WHERE organization_id=? AND project_id=?`, scope.organization, scope.project).Scan(&view.MinRunners, &view.MaxRunners, &view.IdleSeconds, &view.Bootstrap, &view.Revision, &placement)
	if errors.Is(err, sql.ErrNoRows) {
		return view, nil
	}
	if err != nil {
		return view, err
	}
	if err := json.Unmarshal([]byte(placement), view.Placement); err != nil {
		return view, err
	}
	resolved := view.Placement.Resolved()
	view.Placement = &resolved
	rows, err := s.database.db.QueryContext(ctx, `SELECT m.name,m.state,m.bootstrap_log,m.enrollment_id,COALESCE(r.id,''),m.idle_since FROM project_sprite_members m LEFT JOIN runner_identities r ON r.enrollment_id=m.enrollment_id WHERE m.organization_id=? AND m.project_id=? AND (m.state<>'deleted' OR m.name IN (SELECT name FROM project_sprite_members WHERE organization_id=m.organization_id AND project_id=m.project_id AND state='deleted' ORDER BY created_at DESC,name DESC LIMIT 20)) ORDER BY m.created_at,m.name`, scope.organization, scope.project)
	if err != nil {
		return view, err
	}
	defer rows.Close()
	for rows.Next() {
		var member spritePoolMember
		var idle string
		if err := rows.Scan(&member.Name, &member.State, &member.BootstrapLog, &member.EnrollmentID, &member.RunnerID, &idle); err != nil {
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

func (s *Service) setSpritePool(c echo.Context) error {
	scope := nativeRequestScope(c)
	if !canManageProjectSecrets(scope.credential) {
		return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "forbidden", Message: "Sprite pool settings require owner or admin access"})
	}
	var input struct {
		spritePoolSettings
		Bootstrap *string `json:"bootstrap"`
	}
	if err := decodeAPIJSON(c, &input); err != nil {
		return invalidAPIRequest(c, err)
	}
	settings := input.spritePoolSettings
	if input.Bootstrap != nil {
		settings.Bootstrap = *input.Bootstrap
	} else {
		view, err := s.readSpritePool(c.Request().Context(), scope)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		settings.Bootstrap = view.Bootstrap
	}
	if err := s.updateSpritePool(c.Request().Context(), scope, settings); err != nil {
		return s.nativeAPIError(c, err)
	}
	s.startSpritePoolForQueue(scope)
	return s.getSpritePool(c)
}

func validateSpritePoolSettings(settings *spritePoolSettings) error {
	if settings.Placement != nil {
		resolved := settings.Placement.Resolved()
		if err := resolved.Validate(); err != nil {
			return nativeInvalid(err.Error())
		}
		settings.Placement = &resolved
	}
	if settings.IdleSeconds == 0 {
		settings.IdleSeconds = 300
	}
	if settings.MinRunners < 0 || settings.MaxRunners < settings.MinRunners || settings.MaxRunners > 100 || settings.IdleSeconds < 30 || settings.IdleSeconds > 86400 || len(settings.Bootstrap) > 65536 || settings.Revision < 0 {
		return nativeInvalid("Sprite pools require 0 <= min_runners <= max_runners <= 100, an idle threshold of 30 to 86400 seconds, optional bootstrap of at most 65536 bytes, and a nonnegative revision")
	}
	return nil
}

func (s *Service) updateSpritePool(ctx context.Context, scope nativeScope, settings spritePoolSettings) error {
	if !canManageProjectSecrets(scope.credential) {
		return nativeInvalid("Sprite pool settings require owner or admin access")
	}
	if err := validateSpritePoolSettings(&settings); err != nil {
		return err
	}
	if settings.MaxRunners > 0 && (s.config.SecretKeys == nil || s.config.Hosted == nil) {
		return nativeInvalid("Sprite pools require hosted configuration and the project secret store")
	}
	scope.requireHostedAdmin = true
	return s.secretMutation(ctx, scope, func(tx *sql.Tx) error {
		if err := requireCredentialAuthority(ctx, tx, scope.credential, s.config.now()); err != nil {
			return err
		}
		actor := scope.credential.ID
		if scope.credential.HostedPrincipal != "" {
			actor = scope.credential.HostedPrincipal
		}
		var revision int64
		currentPlacement := `{"mode":"blended"}`
		err := tx.QueryRowContext(ctx, `SELECT revision,placement_json FROM project_sprite_pools WHERE organization_id=? AND project_id=?`, scope.organization, scope.project).Scan(&revision, &currentPlacement)
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
		_, err = tx.ExecContext(ctx, `INSERT INTO project_sprite_pools(organization_id,project_id,min_runners,max_runners,idle_seconds,bootstrap,configured_by,placement_json,revision) VALUES(?,?,?,?,?,?,?,?,1) ON CONFLICT(organization_id,project_id) DO UPDATE SET min_runners=excluded.min_runners,max_runners=excluded.max_runners,idle_seconds=excluded.idle_seconds,bootstrap=excluded.bootstrap,configured_by=excluded.configured_by,placement_json=excluded.placement_json,revision=project_sprite_pools.revision+1`, scope.organization, scope.project, settings.MinRunners, settings.MaxRunners, settings.IdleSeconds, settings.Bootstrap, actor, currentPlacement)
		return err
	})
}

func (s *Service) spritePoolAuthority(ctx context.Context, query nativeQueryer, scope nativeScope) (string, error) {
	var actor string
	err := query.QueryRowContext(ctx, `SELECT p.configured_by FROM project_sprite_pools p JOIN api_tokens t ON t.id=p.configured_by AND t.revoked_at IS NULL
WHERE p.organization_id=? AND p.project_id=? AND (t.expires_at IS NULL OR julianday(t.expires_at)>julianday(?)) AND
((t.scope='admin' AND t.native_only=0) OR EXISTS (SELECT 1 FROM hosted_members m JOIN hosted_project_grants g ON g.user_id=m.user_id WHERE m.principal_id=t.id AND m.active=1 AND m.role IN ('owner','admin') AND g.organization_id=p.organization_id AND g.project_id=p.project_id AND g.can_write=1))`, scope.organization, scope.project, formatHubTime(s.config.now())).Scan(&actor)
	return actor, err
}

func (s *Service) spritePoolSnapshot(ctx context.Context, scope nativeScope, view spritePoolView) (spriteScaleInput, []spritePoolMember, error) {
	input := spriteScaleInput{Floor: view.MinRunners, Ceiling: view.MaxRunners}
	now := s.config.now()
	placement, err := readPlacementSnapshot(ctx, s.database.db, scope, now, nil)
	if err != nil {
		return input, nil, err
	}
	input.Depth, input.Free, input.Pending = placement.SpriteTarget, placement.SpriteFree, placement.Pending
	input.CreateLimit = placement.ProviderFree
	allowed, err := monthlySpriteAllowed(ctx, s.database.db, scope, now)
	if err != nil {
		return input, nil, err
	}
	decision, err := checkMonthlyBudget(ctx, s.database.db, scope, budget.CostExposure{SpriteInfrastructure: true}, false, now)
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
	idle := []spritePoolMember{}
	for _, member := range view.Members {
		if member.State == "deleted" {
			continue
		}
		input.Count++
		if member.State == "bootstrapping" {
			continue
		}
		if member.State == "deleting" {
			continue
		}
		if member.RunnerID == "" {
			continue
		}
		runner, err := readRunnerWithClock(ctx, s.database.db, scope.organization, member.RunnerID, s.config.now)
		if err != nil {
			return input, nil, err
		}
		retained, err := monthlyBudgetRetainsRunner(ctx, s.database.db, scope, runner.MachineID)
		if err != nil {
			return input, nil, err
		}
		if runner.HostUsed > 0 || retained {
			_, err := s.database.db.ExecContext(ctx, `UPDATE project_sprite_members SET idle_since=? WHERE organization_id=? AND project_id=? AND name=?`, formatHubTime(now), scope.organization, scope.project, member.Name)
			if err != nil {
				return input, nil, err
			}
			continue
		}
		if now.Sub(member.IdleSince) >= time.Duration(view.IdleSeconds)*time.Second {
			idle = append(idle, member)
			input.Idle++
		}
	}
	return input, idle, nil
}

func (s *Service) scaleSpritePool(ctx context.Context, scope nativeScope, allowCreate bool) error {
	view, err := s.readSpritePool(ctx, scope)
	if err != nil || view.Revision == 0 {
		return err
	}
	if _, err := s.spritePoolAuthority(ctx, s.database.db, scope); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	for _, member := range view.Members {
		if member.State == "bootstrapping" {
			var expires string
			if err := s.database.db.QueryRowContext(ctx, `SELECT expires_at FROM runner_enrollments WHERE id=?`, member.EnrollmentID).Scan(&expires); err != nil {
				return err
			}
			end, err := parseTimeValue(expires)
			if err != nil {
				return err
			}
			if !end.After(s.config.now()) {
				member.State = "deleting"
			}
		}
		if member.State == "deleting" {
			if err := s.deletePoolSprite(ctx, scope, member); err != nil {
				return err
			}
		}
	}
	view, err = s.readSpritePool(ctx, scope)
	if err != nil {
		return err
	}
	input, idle, err := s.spritePoolSnapshot(ctx, scope, view)
	if err != nil {
		return err
	}
	decision := decideSpriteScale(input)
	for i := range decision.Delete {
		if err := s.deletePoolSprite(ctx, scope, idle[i]); err != nil {
			return err
		}
	}
	if !allowCreate {
		return nil
	}
	for range view.MaxRunners {
		view, err = s.readSpritePool(ctx, scope)
		if err != nil {
			return err
		}
		input, _, err = s.spritePoolSnapshot(ctx, scope, view)
		if err != nil || decideSpriteScale(input).Create == 0 {
			return err
		}
		if err := s.createPoolSprite(ctx, scope, view.spritePoolSettings); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) scheduleSpritePoolForQueue(ctx context.Context, scope nativeScope) {
	project, err := readNativeProject(ctx, s.database.db, scope)
	if err != nil {
		return
	}
	for _, state := range project.States {
		if state.Dispatchable && !state.Terminal {
			s.startSpriteLifecycle(scope, state.Name)
			return
		}
	}
	s.startSpriteLifecycle(scope, "")
}

func (s *Service) maintainSpritePools(ctx context.Context) {
	rows, err := s.database.db.QueryContext(ctx, `SELECT DISTINCT organization_id,project_id FROM project_sprite_members WHERE state<>'deleted'`)
	if err != nil {
		return
	}
	defer rows.Close()
	var scopes []nativeScope
	for rows.Next() {
		var scope nativeScope
		if err := rows.Scan(&scope.organization, &scope.project); err != nil {
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
