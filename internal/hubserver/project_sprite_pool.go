package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type spritePoolSettings struct {
	MinRunners  int    `json:"min_runners"`
	MaxRunners  int    `json:"max_runners"`
	IdleSeconds int    `json:"idle_seconds"`
	Bootstrap   string `json:"bootstrap"`
	Revision    int64  `json:"revision"`
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
	Members []spritePoolMember `json:"members"`
}

type spriteScaleInput struct {
	Depth, Free, Pending, Count, Floor, Ceiling, Idle int
}

type spriteScaleDecision struct {
	Create, Delete int
}

func decideSpriteScale(input spriteScaleInput) spriteScaleDecision {
	create := min(max(input.Floor-input.Count, input.Depth-input.Free-input.Pending, 0), max(input.Ceiling-input.Count, 0))
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
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, view)
}

func (s *Service) readSpritePool(ctx context.Context, scope nativeScope) (spritePoolView, error) {
	view := spritePoolView{spritePoolSettings: spritePoolSettings{IdleSeconds: 300}, Members: []spritePoolMember{}}
	err := s.database.db.QueryRowContext(ctx, `SELECT min_runners,max_runners,idle_seconds,bootstrap,revision FROM project_sprite_pools WHERE organization_id=? AND project_id=?`, scope.organization, scope.project).Scan(&view.MinRunners, &view.MaxRunners, &view.IdleSeconds, &view.Bootstrap, &view.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return view, nil
	}
	if err != nil {
		return view, err
	}
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
	var settings spritePoolSettings
	if err := decodeAPIJSON(c, &settings); err != nil {
		return invalidAPIRequest(c, err)
	}
	if err := s.updateSpritePool(c.Request().Context(), scope, settings); err != nil {
		return s.nativeAPIError(c, err)
	}
	s.startSpritePoolForQueue(scope)
	return s.getSpritePool(c)
}

func validateSpritePoolSettings(settings *spritePoolSettings) error {
	if settings.IdleSeconds == 0 {
		settings.IdleSeconds = 300
	}
	if settings.MinRunners < 0 || settings.MaxRunners < settings.MinRunners || settings.MaxRunners > 100 || settings.IdleSeconds < 30 || settings.IdleSeconds > 86400 || len(settings.Bootstrap) > 65536 || settings.MaxRunners > 0 && strings.TrimSpace(settings.Bootstrap) == "" || settings.Revision < 0 {
		return nativeInvalid("Sprite pools require 0 <= min_runners <= max_runners <= 100, an idle threshold of 30 to 86400 seconds, and customer provider/project bootstrap steps when enabled")
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
		err := tx.QueryRowContext(ctx, `SELECT revision FROM project_sprite_pools WHERE organization_id=? AND project_id=?`, scope.organization, scope.project).Scan(&revision)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if revision != settings.Revision {
			return nativeConflict(tracker.Revision(revision))
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO project_sprite_pools(organization_id,project_id,min_runners,max_runners,idle_seconds,bootstrap,configured_by,revision) VALUES(?,?,?,?,?,?,?,1) ON CONFLICT(organization_id,project_id) DO UPDATE SET min_runners=excluded.min_runners,max_runners=excluded.max_runners,idle_seconds=excluded.idle_seconds,bootstrap=excluded.bootstrap,configured_by=excluded.configured_by,revision=project_sprite_pools.revision+1`, scope.organization, scope.project, settings.MinRunners, settings.MaxRunners, settings.IdleSeconds, settings.Bootstrap, actor)
		return err
	})
}

func (s *Service) spritePoolAuthority(ctx context.Context, query nativeQueryer, scope nativeScope) (string, error) {
	var actor string
	err := query.QueryRowContext(ctx, `SELECT p.configured_by FROM project_sprite_pools p JOIN api_tokens t ON t.id=p.configured_by AND t.revoked_at IS NULL
WHERE p.organization_id=? AND p.project_id=? AND (t.expires_at IS NULL OR julianday(t.expires_at)>julianday(?)) AND
((t.scope='admin' AND t.native_only=0) OR EXISTS (SELECT 1 FROM hosted_members m JOIN hosted_project_grants g ON g.user_id=m.user_id WHERE m.principal_id=t.id AND m.active=1 AND m.role IN ('owner','admin') AND g.organization_id=p.organization_id AND g.project_id=p.project_id AND g.can_write=1 AND g.manage_runner=1))`, scope.organization, scope.project, formatHubTime(s.config.now())).Scan(&actor)
	return actor, err
}

func (s *Service) spritePoolSnapshot(ctx context.Context, scope nativeScope, view spritePoolView) (spriteScaleInput, []spritePoolMember, error) {
	input := spriteScaleInput{Floor: view.MinRunners, Ceiling: view.MaxRunners}
	now := s.config.now()
	err := s.database.db.QueryRowContext(ctx, `SELECT count(*) FROM issues i JOIN projects p ON p.id=i.project_id AND p.organization_id=i.organization_id JOIN workflow_states w ON w.id=i.workflow_state_id WHERE i.organization_id=? AND i.project_id=? AND i.archived=0 AND w.dispatchable=1 AND w.terminal=0 AND NOT EXISTS (SELECT 1 FROM leases l WHERE l.issue_id=i.id AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?)) AND (p.require_dependencies=0 OR NOT EXISTS (SELECT 1 FROM issue_dependencies d JOIN issues b ON b.id=d.blocker_issue_id LEFT JOIN workflow_states bs ON bs.id=b.workflow_state_id WHERE d.dependent_issue_id=i.id AND COALESCE(bs.terminal,0)=0))`, scope.organization, scope.project, formatHubTime(now)).Scan(&input.Depth)
	if err != nil {
		return input, nil, err
	}
	ids := []string{}
	rows, err := s.database.db.QueryContext(ctx, `SELECT r.id FROM runner_identities r JOIN token_grants g ON g.token_id=r.token_id WHERE r.organization_id=? AND r.removed_at IS NULL AND g.organization_id=? AND g.project_id=? ORDER BY r.id`, scope.organization, scope.organization, scope.project)
	if err != nil {
		return input, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return input, nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return input, nil, err
	}
	hostFree := make(map[tracker.MachineID]int)
	var allocated []struct {
		report providercapacity.Report
		slots  int
	}
	for _, id := range ids {
		runner, err := readRunnerWithClock(ctx, s.database.db, scope.organization, id, s.config.now)
		if err != nil {
			return input, nil, err
		}
		pendingRunner := false
		var name, woke string
		if err := s.database.db.QueryRowContext(ctx, `SELECT COALESCE(json_extract(capabilities_json,'$.sprite_name'),''),COALESCE(json_extract(capabilities_json,'$.sprite_woken_at'),'') FROM machines WHERE id=? AND organization_id=?`, runner.MachineID, scope.organization).Scan(&name, &woke); err != nil {
			return input, nil, err
		}
		paused := name == runner.Hostname && validSpritesSlug(name) && now.Sub(runner.LastHeartbeatAt) >= spriteRunnerIdle
		if runner.ConnectionHealth == "offline" || paused {
			if runner.Health == "revoked" || runner.Health == "expired" {
				continue
			}
			at, err := parseTimeValue(woke)
			if err != nil {
				continue
			}
			if name == runner.Hostname && !at.IsZero() && !now.Before(at) && now.Sub(at) < time.Minute {
				runner.Health, runner.ConnectionHealth = "online", "online"
				pendingRunner = true
			} else {
				continue
			}
		}
		if len(runner.Exclusions(scope.project, policy.Requirements{}, false)) != 0 || len(runner.Problems) != 0 || len(runner.ProviderCapacity) == 0 {
			continue
		}
		free := max(min(runner.CapacityLimit, runner.ReportedCapacity)-runner.Used, 0)
		available := 0
		var account providercapacity.Report
		for _, provider := range runner.ProviderCapacity {
			if provider.State == "exhausted" {
				continue
			}
			remaining := max(provider.MaxConcurrent-provider.Used, 0)
			for _, prior := range allocated {
				if sharedProviderAccount(prior.report, provider.Report) {
					remaining = max(remaining-prior.slots, 0)
				}
			}
			if remaining > available {
				available, account = remaining, provider.Report
			}
		}
		free = min(free, available)
		if _, ok := hostFree[runner.MachineID]; !ok {
			hostFree[runner.MachineID] = max(runner.HostCapacity-runner.HostUsed, 0)
		}
		free = min(free, hostFree[runner.MachineID])
		hostFree[runner.MachineID] -= free
		allocated = append(allocated, struct {
			report providercapacity.Report
			slots  int
		}{account, free})
		if pendingRunner {
			input.Pending += free
		} else {
			input.Free += free
		}
	}
	idle := []spritePoolMember{}
	for _, member := range view.Members {
		if member.State == "deleted" {
			continue
		}
		input.Count++
		if member.State == "bootstrapping" {
			input.Pending++
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
		if runner.HostUsed > 0 {
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
