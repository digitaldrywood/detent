package hubserver

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestWakeSpriteRunners(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name          string
		heartbeatAgo  time.Duration
		dispatchable  bool
		storeToken    bool
		spriteStatus  int
		wantWoken     int
		wantWakeCalls int
	}{
		{name: "paused runner with dispatchable work", heartbeatAgo: time.Minute, dispatchable: true, storeToken: true, spriteStatus: http.StatusOK, wantWoken: 1, wantWakeCalls: 1},
		{name: "awake runner is left alone", heartbeatAgo: time.Second, dispatchable: true, storeToken: true, spriteStatus: http.StatusOK},
		{name: "terminal or non-dispatchable state", heartbeatAgo: time.Minute, storeToken: true, spriteStatus: http.StatusOK},
		{name: "project without a Sprites token", heartbeatAgo: time.Minute, dispatchable: true, spriteStatus: http.StatusOK},
		{name: "host is not a Sprite in this organization", heartbeatAgo: time.Minute, dispatchable: true, storeToken: true, spriteStatus: http.StatusNotFound, wantWakeCalls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var calls []*http.Request
			client := &http.Client{Transport: spritesTestTransport(func(request *http.Request) (*http.Response, error) {
				mu.Lock()
				calls = append(calls, request.Clone(request.Context()))
				mu.Unlock()
				return spritesTestResponse(`{"type":"complete"}`, tt.spriteStatus), nil
			})}
			f := newDefaultNativeFixture(t, Config{SecretKeys: secretTestKeys(t, "1", "1"), SpritesHTTPClient: client})
			runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			runner.enroll(t)
			heartbeat := formatHubTime(f.service.config.now().Add(-tt.heartbeatAgo))
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at=?", heartbeat); err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{ID: "test"}}
			if tt.storeToken {
				envelope, err := f.service.config.SecretKeys.Seal([]byte(spritesSecretSentinel), secretAAD(string(scope.organization), string(scope.project), flySpritesToken))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO project_secrets(organization_id, project_id, kind, organization_slug, ciphertext, nonce, wrapped_data_key, master_key_version, updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, scope.organization, scope.project, flySpritesToken, "detent-test", envelope.Ciphertext, envelope.Nonce, envelope.WrappedKey, envelope.Version, heartbeat); err != nil {
					t.Fatal(err)
				}
			}
			state := ""
			for _, candidate := range f.project.States {
				if (candidate.Dispatchable && !candidate.Terminal) == tt.dispatchable {
					state = candidate.Name
					break
				}
			}
			if state == "" {
				t.Fatalf("fixture project has no state with dispatchable=%t", tt.dispatchable)
			}
			woken, err := f.service.wakeSpriteRunners(t.Context(), scope, state)
			if err != nil {
				t.Fatal(err)
			}
			if woken != tt.wantWoken {
				t.Fatalf("woken = %d, want %d", woken, tt.wantWoken)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(calls) != tt.wantWakeCalls {
				t.Fatalf("wake calls = %d, want %d", len(calls), tt.wantWakeCalls)
			}
			for _, call := range calls {
				if call.Method != http.MethodPost || call.URL.String() != "https://api.sprites.dev/v1/sprites/customer-host/services/detent-runner/start?duration=30s" {
					t.Fatalf("wake request = %s %s", call.Method, call.URL)
				}
				if call.Header.Get("Authorization") != "Bearer "+spritesSecretSentinel {
					t.Fatal("wake request did not carry the project's Sprites token")
				}
			}
		})
	}
}
