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

// wakeSpriteRunnersAfter wakes the project's paused Sprite runners when a
// mutation leaves a work item in a dispatchable state. It never delays or fails
// the mutation that triggered it.
func (s *Service) wakeSpriteRunnersAfter(scope nativeScope, result json.RawMessage) {
	if s.config.SecretKeys == nil {
		return
	}
	var issue struct {
		State string `json:"state"`
	}
	if json.Unmarshal(result, &issue) != nil || issue.State == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = s.wakeSpriteRunners(ctx, scope, issue.State)
	}()
}

// wakeSpriteRunners starts the detent-runner service on each paused Sprite
// that runs a runner granted this project. A Sprite's hostname is its name, so
// the runner's enrolled hostname identifies the Sprite. Starting the service
// wakes a cold Sprite; the 30-second log stream keeps it active until the
// runner claims the work and holds its own Sprite task.
func (s *Service) wakeSpriteRunners(ctx context.Context, scope nativeScope, state string) (int, error) {
	project, err := readNativeProject(ctx, s.database.db, scope)
	if err != nil {
		return 0, err
	}
	dispatchable := false
	for _, candidate := range project.States {
		if candidate.Name == state && candidate.Dispatchable && !candidate.Terminal {
			dispatchable = true
		}
	}
	if !dispatchable {
		return 0, nil
	}
	cutoff := formatHubTime(s.config.now().Add(-spriteRunnerIdle))
	rows, err := s.database.db.QueryContext(ctx, `SELECT DISTINCT m.hostname FROM runner_identities r
JOIN machines m ON m.id = r.machine_id
JOIN api_tokens t ON t.id = r.token_id AND t.revoked_at IS NULL
JOIN token_grants g ON g.token_id = r.token_id AND g.project_id = ?
WHERE r.organization_id = ? AND r.state = 'active' AND r.last_heartbeat_at < ?
ORDER BY m.hostname`, scope.project, scope.organization, cutoff)
	if err != nil {
		return 0, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if validSpritesSlug(name) {
			names = append(names, name)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	if len(names) == 0 {
		return 0, nil
	}
	var envelope hubsecrets.Envelope
	err = s.database.db.QueryRowContext(ctx, `SELECT ciphertext, nonce, wrapped_data_key, master_key_version FROM project_secrets WHERE organization_id=? AND project_id=? AND kind=?`, scope.organization, scope.project, flySpritesToken).Scan(&envelope.Ciphertext, &envelope.Nonce, &envelope.WrappedKey, &envelope.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if err := s.auditSecretUse(ctx, scope, envelope.Version); err != nil {
		return 0, err
	}
	token, err := s.config.SecretKeys.Open(envelope, secretAAD(string(scope.organization), string(scope.project), flySpritesToken))
	if err != nil {
		return 0, err
	}
	defer clear(token)
	client := http.Client{Timeout: 45 * time.Second}
	if s.config.SpritesHTTPClient != nil {
		client = *s.config.SpritesHTTPClient
		client.Timeout = 45 * time.Second
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	woken := 0
	for _, name := range names {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.sprites.dev/v1/sprites/"+name+"/services/detent-runner/start?duration=30s", nil)
		if err != nil {
			continue
		}
		request.Header.Set("Authorization", "Bearer "+string(token))
		response, err := client.Do(request)
		request.Header.Del("Authorization")
		if err != nil {
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		if response.StatusCode >= 200 && response.StatusCode <= 299 {
			woken++
		}
	}
	return woken, nil
}
