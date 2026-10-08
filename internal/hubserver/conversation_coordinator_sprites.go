package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func coordinatorSpriteTools() []runner.AgentTool {
	empty := `{"type":"object","properties":{},"additionalProperties":false}`
	return []runner.AgentTool{
		coordinatorTool("get_sprite_pool", "Read this project's Sprites token metadata, pool bounds, bootstrap state, runner_setup_declared (null means unknown) and runner connection/provider readiness. Enrollment alone is not readiness.", empty),
		coordinatorTool("set_sprites_token", "Get the secure Sprites connector link for setting or replacing this project's write-only organization token. The user enters the token there, never in chat or tool arguments.", empty),
		coordinatorTool("set_sprite_pool", "Preview pool floor/ceiling and optional customer bootstrap steps. Preserve existing bootstrap when omitted. Never include credentials in bootstrap; use customer-owned login/setup. Requires current owner/admin authority; the client controls confirmation; saving starts the existing pool lifecycle.", `{"type":"object","required":["min_runners","max_runners"],"properties":{"min_runners":{"type":"integer","minimum":0,"maximum":100},"max_runners":{"type":"integer","minimum":0,"maximum":100},"idle_seconds":{"type":"integer","minimum":30,"maximum":86400},"bootstrap":{"type":"string","maxLength":12000}},"additionalProperties":false}`),
		coordinatorTool("scale_up_sprite_pool", "Preview a scale-up or retry through the existing pool lifecycle, within the saved floor and ceiling. Set a floor of one for the first runner on an empty project. Requires current owner/admin authority; the client controls confirmation.", empty),
		coordinatorTool("get_sprite_bootstrap_log", "Read the last Sprite bootstrap progress log tail, including failed/deleted members, and retry guidance. Logs contain known progress only.", empty),
	}
}

func coordinatorSpriteTool(name string) bool {
	return coordinatorSpriteMutation(name) || name == "get_sprite_pool" || name == "set_sprites_token" || name == "get_sprite_bootstrap_log"
}

func coordinatorSpriteMutation(name string) bool {
	return name == "set_sprite_pool" || name == "scale_up_sprite_pool"
}

type coordinatorSpriteArguments struct {
	MinRunners  *int    `json:"min_runners,omitempty"`
	MaxRunners  *int    `json:"max_runners,omitempty"`
	IdleSeconds *int    `json:"idle_seconds,omitempty"`
	Bootstrap   *string `json:"bootstrap,omitempty"`
}

type coordinatorSpriteChange struct {
	Input    coordinatorSpriteArguments `json:"input"`
	Revision int64                      `json:"revision"`
}

func (args coordinatorSpriteArguments) apply(settings *spritePoolSettings) {
	if args.MinRunners != nil {
		settings.MinRunners = *args.MinRunners
	}
	if args.MaxRunners != nil {
		settings.MaxRunners = *args.MaxRunners
	}
	if args.IdleSeconds != nil {
		settings.IdleSeconds = *args.IdleSeconds
	}
	if args.Bootstrap != nil {
		settings.Bootstrap = *args.Bootstrap
	}
}

func decodeCoordinatorSpriteArguments(name string, raw json.RawMessage) (coordinatorSpriteArguments, error) {
	var args coordinatorSpriteArguments
	var fields map[string]json.RawMessage
	if len(raw) > coordinatorToolArgumentBytes || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return args, operatortool.ErrInvalidArguments
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return args, operatortool.ErrInvalidArguments
		}
	}
	if decodeCoordinatorArguments(raw, &args) != nil {
		return args, operatortool.ErrInvalidArguments
	}
	if name != "set_sprite_pool" {
		if !coordinatorSpriteTool(name) || len(fields) != 0 {
			return args, operatortool.ErrInvalidArguments
		}
		return args, nil
	}
	if args.MinRunners == nil || args.MaxRunners == nil || *args.MinRunners < 0 || *args.MaxRunners < *args.MinRunners || *args.MaxRunners > 100 || args.IdleSeconds != nil && (*args.IdleSeconds < 30 || *args.IdleSeconds > 86400) || args.Bootstrap != nil && len(*args.Bootstrap) > 12000 {
		return args, operatortool.ErrInvalidArguments
	}
	return args, nil
}

func (t *coordinatorToolset) spriteTool(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	args, err := decodeCoordinatorSpriteArguments(call.Name, call.Arguments)
	if err != nil {
		return nil, err
	}
	ctx, err = t.actionContext(ctx, record)
	if err != nil {
		return nil, err
	}
	permission := apikey.ScopeRead
	if coordinatorSpriteMutation(call.Name) || call.Name == "set_sprites_token" {
		permission = apikey.ScopeAdmin
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: permission, ProjectID: string(record.ProjectID)})
	if err != nil {
		return nil, err
	}
	s := t.coordinator.service.server
	scope, err := s.coordinatorSpriteScope(ctx, string(record.ProjectID), permission == apikey.ScopeAdmin)
	if err != nil {
		return nil, err
	}
	connector := s.hostedPath("/settings/integrations") + "?project=" + url.QueryEscape(string(scope.project)) + "#sprites"
	if call.Name == "set_sprites_token" {
		return map[string]string{"connector_url": connector, "token_url": "https://sprites.dev/account", "instructions": "Create a Sprites organization token for a dedicated Fly organization with billing enabled and a spend alert. Set or replace the complete token in the Sprites connector. Its value is write-only; never paste it or provider API keys into chat."}, nil
	}
	view, err := s.readSpritePool(ctx, scope)
	if err != nil {
		return nil, err
	}
	if call.Name == "get_sprite_pool" {
		return s.coordinatorSpritePoolStatus(ctx, scope, view, connector)
	}
	if call.Name == "get_sprite_bootstrap_log" {
		result := map[string]string{"name": "", "state": "", "log_tail": "", "retry": "After correcting token, billing or customer setup, approve scale_up_sprite_pool. The existing lifecycle cleans up the failed member and retries within the configured bounds."}
		if len(view.Members) > 0 {
			last := view.Members[len(view.Members)-1]
			lines := strings.Split(strings.TrimSpace(last.BootstrapLog), "\n")
			result["name"], result["state"], result["log_tail"] = last.Name, last.State, boundRunes(strings.Join(lines[max(0, len(lines)-20):], "\n"), 4000)
		}
		return result, nil
	}
	settings := view.spritePoolSettings
	if call.Name == "set_sprite_pool" {
		args.apply(&settings)
	} else if settings.Revision == 0 || settings.MaxRunners == 0 {
		return nil, nativeInvalid("Configure and enable the Sprite pool with set_sprite_pool before scaling up")
	}
	if call.Name == "scale_up_sprite_pool" {
		if _, err := s.spritePoolAuthority(ctx, s.database.db, scope); err != nil {
			return nil, operatortool.ErrAccessDenied
		}
	}
	if err := s.validateCoordinatorSpriteChange(ctx, scope, settings); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(coordinatorSpriteChange{Input: args, Revision: settings.Revision})
	if err != nil {
		return nil, err
	}
	action := chat.Action{ConversationID: record.ID, Kind: chat.ActionKind(call.Name), ProjectID: string(record.ProjectID), Title: strings.ReplaceAll(call.Name, "_", " "), Description: fmt.Sprintf("Sprite pool floor %d, ceiling %d, idle threshold %d seconds (revision %d). Customer setup runs inside new Sprites; Fly usage is billed to your organization.", settings.MinRunners, settings.MaxRunners, settings.IdleSeconds, settings.Revision), Arguments: raw, Material: true}
	return t.submitCoordinatorAction(ctx, record, call, action)
}

func (s *Service) coordinatorSpriteScope(ctx context.Context, project string, manage bool) (nativeScope, error) {
	resolve, ok := ctx.Value(nativeOperatorScopeKey{}).(func(context.Context) (nativeScope, error))
	if !ok {
		return nativeScope{}, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil || manage && !canManageProjectSecrets(scope.credential) {
		return nativeScope{}, operatortool.ErrAccessDenied
	}
	scope.project = tracker.ProjectID(project)
	scope.requireHostedAdmin = manage
	return scope, nil
}

func (s *Service) validateCoordinatorSpriteChange(ctx context.Context, scope nativeScope, settings spritePoolSettings) error {
	if err := validateSpritePoolSettings(&settings); err != nil {
		return err
	}
	if settings.MaxRunners > 0 {
		if s.config.SecretKeys == nil || s.config.Hosted == nil {
			return nativeInvalid("Sprite pools require hosted configuration and the project secret store")
		}
		status, err := readSecretStatus(ctx, s.database.db, scope)
		if err != nil {
			return err
		}
		if !status.Present {
			return nativeInvalid("Set the Sprites organization token through set_sprites_token before enabling the pool")
		}
	}
	return nil
}

func (s *Service) executeCoordinatorSpriteAction(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	scope, err := s.coordinatorSpriteScope(ctx, action.ProjectID, true)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	var change coordinatorSpriteChange
	if decodeCoordinatorArguments(action.Arguments, &change) != nil {
		return chat.ActionExecution{}, operatortool.ErrInvalidArguments
	}
	view, err := s.readSpritePool(ctx, scope)
	if err != nil {
		return chat.ActionExecution{}, coordinatorActionError(err)
	}
	if view.Revision != change.Revision {
		return chat.ActionExecution{}, coordinatorActionError(nativeConflict(tracker.Revision(view.Revision)))
	}
	settings := view.spritePoolSettings
	change.Input.apply(&settings)
	if string(action.Kind) == "scale_up_sprite_pool" && (settings.Revision == 0 || settings.MaxRunners == 0) {
		return chat.ActionExecution{}, operatortool.ErrInvalidArguments
	}
	if err := s.validateCoordinatorSpriteChange(ctx, scope, settings); err != nil {
		return chat.ActionExecution{}, coordinatorActionError(err)
	}
	if string(action.Kind) == "set_sprite_pool" {
		err = s.updateSpritePool(ctx, scope, settings)
	} else {
		err = s.secretMutation(ctx, scope, func(tx *sql.Tx) error {
			if err := requireCredentialAuthority(ctx, tx, scope.credential, s.config.now()); err != nil {
				return err
			}
			if _, err := s.spritePoolAuthority(ctx, tx, scope); err != nil {
				return operatortool.ErrAccessDenied
			}
			var revision int64
			if err := tx.QueryRowContext(ctx, `SELECT revision FROM project_sprite_pools WHERE organization_id=? AND project_id=?`, scope.organization, scope.project).Scan(&revision); err != nil {
				return err
			}
			if revision != settings.Revision {
				return nativeConflict(tracker.Revision(revision))
			}
			return nil
		})
	}
	if err != nil {
		return chat.ActionExecution{}, coordinatorActionError(err)
	}
	s.startSpritePoolForQueue(scope)
	return chat.ActionExecution{Message: "Sprite pool lifecycle requested. Read get_sprite_pool and get_sprite_bootstrap_log to watch progress. A connected runner still needs customer provider sign-in, Git access, checkout and approved project policy before it can push work."}, nil
}

func (s *Service) coordinatorSpritePoolStatus(ctx context.Context, scope nativeScope, view spritePoolView, connector string) (any, error) {
	secret, err := readSecretStatus(ctx, s.database.db, scope)
	if err != nil {
		return nil, err
	}
	validation := "No Sprites organization token is set"
	if secret.Present {
		if s.config.SecretKeys == nil {
			validation = "The Hub secret store is unavailable"
		} else if _, err := s.checkProjectSprites(ctx, scope); err != nil {
			validation = "The Sprites organization could not be validated; check the connector, billing and provider availability"
			if errors.Is(err, errSpritesTokenRejected) || errors.Is(err, errSpritesBilling) {
				validation = err.Error()
			}
		} else {
			validation = "Sprites organization token validated"
		}
	}
	members := make([]map[string]any, 0, len(view.Members))
	connected := 0
	for _, member := range view.Members {
		item := map[string]any{"name": member.Name, "state": member.State, "runner_id": member.RunnerID, "connected": false, "provider_readiness": "unavailable"}
		if member.RunnerID != "" && member.State == "enrolled" {
			r, err := readRunnerWithClock(ctx, s.database.db, scope.organization, member.RunnerID, s.config.now)
			if err != nil {
				return nil, err
			}
			if slices.Contains(r.ProjectIDs, scope.project) {
				item["connection_health"] = r.ConnectionHealth
				item["health"] = r.Health
				item["reported_capacity"] = r.ReportedCapacity
				item["connected"] = r.State == "active" && r.ConnectionHealth == "online"
				if item["connected"] == true {
					connected++
				}
				providers := make([]map[string]string, 0, len(r.ProviderCapacity))
				for _, provider := range r.ProviderCapacity {
					providers = append(providers, map[string]string{"provider": provider.Provider, "backend": provider.Backend, "availability": provider.Availability, "state": provider.State})
				}
				if len(providers) > 0 {
					item["provider_readiness"] = providers
				}
			}
		}
		members = append(members, item)
	}
	declared, err := s.projectRunnerSetupDeclaration(ctx, scope)
	if err != nil {
		return nil, err
	}
	return map[string]any{"runner_setup_declared": declared, "token": secret, "token_validation": validation, "connector_url": connector, "min_runners": view.MinRunners, "max_runners": view.MaxRunners, "idle_seconds": view.IdleSeconds, "revision": view.Revision, "bootstrap_configured": strings.TrimSpace(view.Bootstrap) != "", "connected_runners": connected, "members": members, "login_guide": "https://github.com/digitaldrywood/detent/blob/develop/docs/sprite-runners.md#6-sign-in-to-your-agents"}, nil
}

func (s *Service) projectRunnerSetupDeclaration(ctx context.Context, scope nativeScope) (*bool, error) {
	onboarding, err := s.projectOnboarding(ctx, scope)
	if err != nil {
		return nil, err
	}
	var declared *bool
	for _, entry := range onboarding.Runners {
		if entry.Runner.State != "active" || entry.Runner.ConnectionHealth != "online" || entry.LocalChecks == nil || entry.LocalChecks.Checkout != "passed" || entry.LocalChecks.RunnerSetupDeclared == nil {
			continue
		}
		declared = entry.LocalChecks.RunnerSetupDeclared
		if !*declared {
			break
		}
	}
	return declared, nil
}
