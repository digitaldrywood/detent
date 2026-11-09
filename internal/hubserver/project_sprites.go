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
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var errSpritesValidation = errors.New("sprites rejected the token or could not validate its organization")

var errSpritesTokenRejected = errors.New("sprites rejected the organization token; replace it through the Sprites connector")
var errSpritesBilling = errors.New("the Fly organization needs Sprites billing enabled; open the Sprites account page, enable billing, then retry")

func spritesResponseError(status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return errSpritesTokenRejected
	case http.StatusPaymentRequired:
		return errSpritesBilling
	default:
		return errSpritesValidation
	}
}

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
		return "", spritesResponseError(response.StatusCode)
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
	scope, _, envelope, err := s.resolveSpritesSecret(ctx, s.database.reader, scope)
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
}

type spriteLifecycleTrigger struct {
	scope nativeScope
	state string
}

type spriteLifecyclePass struct {
	cancel          context.CancelFunc
	done            chan struct{}
	pendingState    string
	pending         map[tracker.ProjectID]spriteLifecycleTrigger
	pendingProjects []tracker.ProjectID
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
	s.scheduleSpriteLifecycle(ctx, scope, issue.State)
}

func (s *Service) scheduleSpriteLifecycle(ctx context.Context, scope nativeScope, state string) {
	if s.config.SecretKeys == nil || s.config.TenantRepository == nil {
		return
	}

	key := spriteWakeKey{organization: scope.organization}
	s.spriteWakeMu.Lock()
	defer s.spriteWakeMu.Unlock()
	if ctx.Err() != nil || s.spriteWakes == nil {
		return
	}
	if active := s.spriteWakes[key]; active != nil {
		if state != "" {
			active.pendingState = state
			if active.pending == nil {
				active.pending = map[tracker.ProjectID]spriteLifecycleTrigger{}
			}
			if _, exists := active.pending[scope.project]; !exists {
				active.pendingProjects = append(active.pendingProjects, scope.project)
			}
			active.pending[scope.project] = spriteLifecycleTrigger{scope: scope, state: state}
		}
		return
	}
	wakeContext, cancel := context.WithTimeout(s.freshRunDetailContext(ctx), runnerauth.MaxEnrollmentTTL)
	pass := &spriteLifecyclePass{done: make(chan struct{}), cancel: cancel}
	s.spriteWakes[key] = pass
	s.spriteWakeWork.Add(1)
	go func() {
		defer s.spriteWakeWork.Done()
		defer cancel()
		for {
			if state != "" {
				if _, err := s.wakeOrganizationSpriteRunners(wakeContext, scope); err != nil && wakeContext.Err() == nil {
					s.config.Logger.Warn("sprite runners could not be woken", "error", err)
				}
			}
			if err := s.scaleSpritePool(wakeContext, scope, state != ""); err != nil && wakeContext.Err() == nil {
				s.config.Logger.Warn("Sprite pool lifecycle failed", "organization", scope.organization, "project", scope.project, "error", err)
			}
			s.spriteWakeMu.Lock()
			if wakeContext.Err() != nil || len(pass.pendingProjects) == 0 {
				delete(s.spriteWakes, key)
				close(pass.done)
				s.spriteWakeMu.Unlock()
				return
			}
			project := pass.pendingProjects[0]
			pass.pendingProjects = pass.pendingProjects[1:]
			next := pass.pending[project]
			delete(pass.pending, project)
			scope, state = next.scope, next.state
			pass.pendingState = ""
			s.spriteWakeMu.Unlock()
		}
	}()
}

func (s *Service) stopSpriteRunners() {
	s.spriteWakeMu.Lock()
	s.workerCancel()
	for _, pass := range s.spriteWakes {
		pass.pendingState = ""
		if pass.cancel != nil {
			pass.cancel()
		}
	}
	s.spriteWakes = nil
	s.spriteWakeMu.Unlock()
	s.spriteWakeWork.Wait()
}

func (s *Service) spriteWakeDispatchable(ctx context.Context, scope nativeScope, state string) (bool, error) {
	record, err := s.readProjectRecord(ctx, scope)
	if err != nil {
		return false, err
	}
	for _, candidate := range record.States {
		if candidate.Name == state && candidate.Dispatchable && !candidate.Terminal {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) wakeSpriteRunners(ctx context.Context, scope nativeScope, state string) (int, error) {
	return s.wakeSpriteRunnersRecord(ctx, scope, state)
}

func (s *Service) spriteWakeSecret(ctx context.Context, db nativeQueryer, scope nativeScope, name string) (nativeScope, hubsecrets.Envelope, error) {
	resolved, _, envelope, err := s.spriteWakeSecretRecord(ctx, scope, name)
	return resolved, envelope, err
}

func (s *Service) wakeOrganizationSpriteRunners(ctx context.Context, scope nativeScope) (int, error) {
	return s.wakeOrganizationSpriteRunnersRecord(ctx, scope)
}
