package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var errSpritesValidation = errors.New("sprites rejected the token or could not validate its organization")

// validateSpritesToken uses only the fixed Sprites origin. Redirects are refused
// and neither provider bodies nor transport errors are returned or logged.
func validateSpritesToken(ctx context.Context, configured *http.Client, token []byte) (string, error) {
	if len(token) == 0 || len(token) > 8192 {
		return "", errSpritesValidation
	}
	for _, b := range token {
		if b <= ' ' || b >= 127 {
			return "", errSpritesValidation
		}
	}
	// Sprites organization tokens carry org-slug/org-id/token-id/token-value.
	// A successful authenticated list validates that prefix even for an empty org.
	parts := strings.Split(string(token), "/")
	if len(parts) != 4 {
		return "", errSpritesValidation
	}
	for _, part := range parts {
		if part == "" {
			return "", errSpritesValidation
		}
	}
	slug := strings.Clone(parts[0])
	if !validSpritesSlug(slug) {
		return "", errSpritesValidation
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.sprites.dev/v1/sprites", nil)
	if err != nil {
		return "", errSpritesValidation
	}
	request.Header.Set("Authorization", "Bearer "+string(token))
	defer request.Header.Del("Authorization")
	client := http.Client{Timeout: 15 * time.Second}
	if configured != nil {
		client = *configured
	}
	if client.Timeout <= 0 || client.Timeout > 15*time.Second {
		client.Timeout = 15 * time.Second
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
	var result struct {
		Sprites []struct {
			OrganizationSlug string `json:"org_slug"`
			Organization     string `json:"organization"`
		} `json:"sprites"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&result); err != nil || result.Sprites == nil {
		return "", errSpritesValidation
	}
	// Never trust a provider response that returns a different organization.
	for _, sprite := range result.Sprites {
		if sprite.OrganizationSlug == "" && sprite.Organization == "" || sprite.OrganizationSlug != "" && sprite.OrganizationSlug != slug || sprite.Organization != "" && sprite.Organization != slug {
			return "", errSpritesValidation
		}
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", errSpritesValidation
	}
	return slug, nil
}

func validSpritesSlug(slug string) bool {
	if len(slug) == 0 || len(slug) > 63 || slug[0] == '-' || slug[len(slug)-1] == '-' {
		return false
	}
	for _, c := range slug {
		if c != '-' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// checkProjectSprites is the internal use path: unseal for one fixed provider
// call, audit before the call, and clear the byte buffer on every exit. No API
// or coordinator tool exposes a plaintext reader or an arbitrary callback.
func (s *Service) checkProjectSprites(ctx context.Context, scope nativeScope) (string, error) {
	var envelope hubsecrets.Envelope
	err := s.database.db.QueryRowContext(ctx, `SELECT ciphertext, nonce, wrapped_data_key, master_key_version FROM project_secrets WHERE organization_id=? AND project_id=? AND kind=?`, scope.organization, scope.project, flySpritesToken).Scan(&envelope.Ciphertext, &envelope.Nonce, &envelope.WrappedKey, &envelope.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if err != nil {
		return "", err
	}
	if err := s.auditSecretUse(ctx, scope, envelope.Version); err != nil {
		return "", err
	}
	token, err := s.config.SecretKeys.Open(envelope, secretAAD(string(scope.organization), string(scope.project), flySpritesToken))
	if err != nil {
		return "", err
	}
	defer clear(token)
	return validateSpritesToken(ctx, s.config.SpritesHTTPClient, token)
}

// spriteRunnerIdle is how long a runner may go without a heartbeat before the
// Hub treats its Sprite as paused. Runners heartbeat every second while awake.
const spriteRunnerIdle = 15 * time.Second

type spriteWakeKey struct {
	organization tracker.OrganizationID
	project      tracker.ProjectID
}

func (s *Service) scheduleSpriteWake(ctx context.Context, scope nativeScope, result json.RawMessage) {
	if s.config.SecretKeys == nil {
		return
	}
	var issue struct {
		State string `json:"state"`
	}
	if json.Unmarshal(result, &issue) != nil || issue.State == "" {
		return
	}
	dispatchable, err := s.spriteWakeDispatchable(ctx, scope, issue.State)
	if err != nil || !dispatchable {
		return
	}
	key := spriteWakeKey{organization: scope.organization, project: scope.project}
	s.spriteWakeMu.Lock()
	defer s.spriteWakeMu.Unlock()
	if ctx.Err() != nil || s.spriteWakes[key] != nil {
		return
	}
	done := make(chan struct{})
	s.spriteWakes[key] = done
	s.spriteWakeWork.Add(1)
	go func() {
		defer s.spriteWakeWork.Done()
		defer func() {
			s.spriteWakeMu.Lock()
			delete(s.spriteWakes, key)
			close(done)
			s.spriteWakeMu.Unlock()
		}()
		wakeContext, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if _, err := s.wakeSpriteRunners(wakeContext, scope, issue.State); err != nil && wakeContext.Err() == nil {
			s.config.Logger.Warn("sprite runners could not be woken", "error", err)
		}
	}()
}

func (s *Service) stopSpriteRunners() {
	s.spriteWakeMu.Lock()
	s.workerCancel()
	s.spriteWakeMu.Unlock()
	s.spriteWakeWork.Wait()
}

func (s *Service) spriteWakeDispatchable(ctx context.Context, scope nativeScope, state string) (bool, error) {
	project, err := readNativeProject(ctx, s.database.db, scope)
	if err != nil {
		return false, err
	}
	for _, candidate := range project.States {
		if candidate.Name == state && candidate.Dispatchable && !candidate.Terminal {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) wakeSpriteRunners(ctx context.Context, scope nativeScope, state string) (int, error) {
	if s.config.SecretKeys == nil {
		return 0, nil
	}
	client := http.Client{Timeout: 45 * time.Second}
	if s.config.SpritesHTTPClient != nil {
		client = *s.config.SpritesHTTPClient
		client.Timeout = 45 * time.Second
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	woken := 0
	last := ""
	for {
		dispatchable, err := s.spriteWakeDispatchable(ctx, scope, state)
		if err != nil || !dispatchable {
			return woken, err
		}
		var name string
		var envelope hubsecrets.Envelope
		cutoff := formatHubTime(s.config.now().Add(-spriteRunnerIdle))
		err = s.database.db.QueryRowContext(ctx, `SELECT m.hostname, ps.ciphertext, ps.nonce, ps.wrapped_data_key, ps.master_key_version FROM runner_identities r
JOIN machines m ON m.id = r.machine_id
JOIN api_tokens t ON t.id = r.token_id AND t.revoked_at IS NULL
JOIN token_grants g ON g.token_id = r.token_id AND g.organization_id = r.organization_id AND g.project_id = ?
JOIN project_secrets ps ON ps.organization_id = r.organization_id AND ps.project_id = g.project_id AND ps.kind = ?
WHERE r.organization_id = ? AND r.state = 'active' AND r.last_heartbeat_at < ? AND m.hostname > ?
ORDER BY m.hostname LIMIT 1`, scope.project, flySpritesToken, scope.organization, cutoff, last).Scan(&name, &envelope.Ciphertext, &envelope.Nonce, &envelope.WrappedKey, &envelope.Version)
		if errors.Is(err, sql.ErrNoRows) {
			return woken, nil
		}
		if err != nil {
			return woken, err
		}
		last = name
		if !validSpritesSlug(name) {
			continue
		}
		started, err := s.wakeSpriteRunner(ctx, scope, &client, name, envelope)
		if err != nil {
			return woken, err
		}
		failedAt := ""
		if !started {
			failedAt = formatHubTime(s.config.now())
		}
		if _, err := s.database.db.ExecContext(ctx, `UPDATE machines SET capabilities_json=json_set(capabilities_json, '$.sprite_wake_failed_at', ?) WHERE organization_id=? AND hostname=? AND json_extract(capabilities_json, '$.sprite_name')=hostname AND id IN (SELECT r.machine_id FROM runner_identities r JOIN token_grants g ON g.token_id=r.token_id AND g.organization_id=r.organization_id JOIN api_tokens t ON t.id=r.token_id AND t.revoked_at IS NULL WHERE r.organization_id=? AND g.project_id=? AND r.state='active')`, failedAt, scope.organization, name, scope.organization, scope.project); err != nil {
			return woken, err
		}
		if started {
			woken++
		}
	}
}

func (s *Service) wakeSpriteRunner(ctx context.Context, scope nativeScope, client *http.Client, name string, envelope hubsecrets.Envelope) (bool, error) {
	if err := s.auditSecretUse(ctx, scope, envelope.Version); err != nil {
		return false, err
	}
	token, err := s.config.SecretKeys.Open(envelope, secretAAD(string(scope.organization), string(scope.project), flySpritesToken))
	if err != nil {
		return false, err
	}
	defer clear(token)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.sprites.dev/v1/sprites/"+name+"/services/detent-runner/start?duration=30s", nil)
	if err != nil {
		return false, err
	}
	request.Header.Set("Authorization", "Bearer "+string(token))
	response, err := client.Do(request)
	request.Header.Del("Authorization")
	if err != nil {
		return false, nil
	}
	_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	_ = response.Body.Close()
	return readErr == nil && response.StatusCode >= 200 && response.StatusCode <= 299, nil
}
