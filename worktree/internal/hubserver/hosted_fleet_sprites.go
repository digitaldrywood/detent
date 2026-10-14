package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) hostedFleetSprite(ctx, statusContext context.Context, credential apiCredential, view *hostedFleetRunner, runner runnerauth.Runner, visible map[tracker.ProjectID]bool) error {
	var name, failed string
	if err := s.database.db.QueryRowContext(ctx, `SELECT COALESCE(json_extract(capabilities_json, '$.sprite_name'), ''), COALESCE(json_extract(capabilities_json, '$.sprite_wake_failed_at'), '') FROM machines WHERE id=? AND organization_id=?`, runner.MachineID, runner.OrganizationID).Scan(&name, &failed); err != nil {
		return err
	}
	if name == "" || name != runner.Hostname || !validSpritesSlug(name) {
		return nil
	}
	view.Sprite = &hostedFleetSprite{Name: name, Status: "unknown"}
	if s.config.SecretKeys != nil && runner.State == "active" && runner.ConnectionHealth != "revoked" && runner.ConnectionHealth != "expired" {
		for project := range visible {
			if !runner.AllowsProject(project) {
				continue
			}
			if !visible[project] {
				continue
			}
			scope := nativeScope{organization: runner.OrganizationID, project: project, credential: credential}
			scope, organization, envelope, err := resolveSpritesSecret(ctx, s.database.db, scope)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			view.Sprite.CanWake = true
			if runner.ConnectionHealth == "online" {
				view.Sprite.Status = "running"
				break
			}
			status, err := s.readSpriteStatus(statusContext, scope, runner.Hostname, organization, envelope)
			if err != nil {
				continue
			}
			view.Sprite = &hostedFleetSprite{Name: runner.Hostname, Status: status, CanWake: true}
			break
		}
	}
	failedAt, parseErr := parseTimeValue(failed)
	view.Sprite.WakeFailed = parseErr == nil && !failedAt.IsZero() && !failedAt.Before(runner.LastHeartbeatAt)
	if runner.ConnectionHealth == "offline" && runner.State == "active" {
		view.Health = "needs_attention"
		if view.Sprite.CanWake && !view.Sprite.WakeFailed && (view.Sprite.Status == "warm" || view.Sprite.Status == "cold") && len(runner.Problems) == 0 && len(runner.Leases) == 0 && view.ClaimRefusalReason == "" {
			view.Health = "asleep"
		}
	}
	return nil
}

func (s *Service) readSpriteStatus(ctx context.Context, scope nativeScope, name, organization string, envelope hubsecrets.Envelope) (string, error) {
	if err := s.auditSecretUse(ctx, scope, envelope.Version); err != nil {
		return "", err
	}
	token, err := s.config.SecretKeys.Open(envelope, secretAAD(string(scope.organization), string(scope.project), flySpritesToken))
	if err != nil {
		return "", err
	}
	defer clear(token)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.sprites.dev/v1/sprites/"+name, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+string(token))
	defer request.Header.Del("Authorization")
	client := http.Client{Timeout: 5 * time.Second}
	if s.config.SpritesHTTPClient != nil {
		client = *s.config.SpritesHTTPClient
		client.Timeout = 5 * time.Second
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return "", errSpritesValidation
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", errSpritesValidation
	}
	var sprite struct {
		Name         string `json:"name"`
		Organization string `json:"organization"`
		Status       string `json:"status"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&sprite); err != nil || sprite.Name != name || sprite.Organization != organization {
		return "", errSpritesValidation
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", errSpritesValidation
	}
	return sprite.Status, nil
}
