package hubserver

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedFleetSprites(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		status       string
		known        bool
		token        bool
		hidden       bool
		wakeFailure  bool
		oldFailure   bool
		organization string
		wantHealth   string
		wantSprite   bool
		wantCalls    int
	}{
		{name: "warm Sprite", status: "warm", known: true, token: true, wantHealth: "asleep", wantSprite: true, wantCalls: 1},
		{name: "cold Sprite", status: "cold", known: true, token: true, wantHealth: "asleep", wantSprite: true, wantCalls: 1},
		{name: "ordinary host sharing a Sprite name", status: "warm", token: true, wantHealth: "offline"},
		{name: "running Sprite with missing heartbeat", status: "running", known: true, token: true, wantHealth: "needs_attention", wantSprite: true, wantCalls: 1},
		{name: "missing token", known: true, wantHealth: "needs_attention", wantSprite: true},
		{name: "hidden project token", known: true, token: true, hidden: true, wantHealth: "needs_attention", wantSprite: true},
		{name: "ordinary offline host", wantHealth: "offline"},
		{name: "known Sprite cannot be found", known: true, token: true, status: "missing", wantHealth: "needs_attention", wantSprite: true, wantCalls: 1},
		{name: "provider redirect", known: true, token: true, status: "redirect", wantHealth: "needs_attention", wantSprite: true, wantCalls: 1},
		{name: "provider reports another organization", known: true, token: true, status: "warm", organization: "other", wantHealth: "needs_attention", wantSprite: true, wantCalls: 1},
		{name: "failed wake", known: true, token: true, status: "warm", wakeFailure: true, wantHealth: "needs_attention", wantSprite: true, wantCalls: 1},
		{name: "heartbeat supersedes wake failure", known: true, token: true, status: "warm", oldFailure: true, wantHealth: "asleep", wantSprite: true, wantCalls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			organization := tt.organization
			if organization == "" {
				organization = "detent-test"
			}
			client := &http.Client{Transport: spritesTestTransport(func(request *http.Request) (*http.Response, error) {
				if request.Header.Get("Authorization") != "Bearer "+spritesSecretSentinel {
					t.Fatal("provider read did not use the selected project token")
				}
				if request.Method == http.MethodPost {
					return spritesTestResponse("", http.StatusServiceUnavailable), nil
				}
				calls++
				if request.Method != http.MethodGet || request.URL.String() != "https://api.sprites.dev/v1/sprites/customer-host" {
					t.Fatalf("unexpected provider request: %s %s", request.Method, request.URL)
				}
				if tt.status == "missing" {
					return spritesTestResponse("", http.StatusNotFound), nil
				}
				if tt.status == "redirect" {
					response := spritesTestResponse("", http.StatusFound)
					response.Header.Set("Location", "https://other.example.test/secret")
					return response, nil
				}
				return spritesTestResponse(`{"name":"customer-host","organization":"`+organization+`","status":"`+tt.status+`"}`, http.StatusOK), nil
			})}
			f, scope, runners := newSpriteWakeFixture(t, client, tt.token, 3*time.Minute, "customer-host")
			name := ""
			if tt.known {
				name = "customer-host"
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE machines SET capabilities_json=json_set(capabilities_json, '$.sprite_name', ?) WHERE id=?", name, runners[0].binding.MachineID); err != nil {
				t.Fatal(err)
			}
			if tt.wakeFailure {
				state := ""
				for _, candidate := range f.project.States {
					if candidate.Dispatchable && !candidate.Terminal {
						state = candidate.Name
						break
					}
				}
				if _, err := f.service.wakeSpriteRunners(t.Context(), scope, state); err != nil {
					t.Fatal(err)
				}
			}
			if tt.oldFailure {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE machines SET capabilities_json=json_set(capabilities_json, '$.sprite_wake_failed_at', ?) WHERE id=?", formatHubTime(f.service.config.now().Add(-5*time.Minute)), runners[0].binding.MachineID); err != nil {
					t.Fatal(err)
				}
			}
			runner, err := readRunner(t.Context(), f.service.database.db, scope.organization, runners[0].binding.RunnerID, f.service.config.now())
			if err != nil {
				t.Fatal(err)
			}
			visible := map[tracker.ProjectID]bool{scope.project: !tt.hidden}
			view := hostedFleetRunnerView(runner, "dev", visible, f.service.config.now())
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if err := f.service.hostedFleetSprite(t.Context(), ctx, scope.credential, &view, runner, visible); err != nil {
				t.Fatal(err)
			}
			if view.Health != tt.wantHealth || (view.Sprite != nil) != tt.wantSprite || calls != tt.wantCalls {
				t.Fatalf("health=%q sprite=%+v provider calls=%d; want %q sprite=%t calls=%d", view.Health, view.Sprite, calls, tt.wantHealth, tt.wantSprite, tt.wantCalls)
			}
			if tt.wakeFailure && !view.Sprite.WakeFailed {
				t.Fatal("failed provider wake was not preserved in the fleet read")
			}
			if calls > 0 {
				var auditUses int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM project_secret_audit WHERE project_id=? AND actor=? AND event='use'", scope.project, scope.credential.ID).Scan(&auditUses); err != nil || auditUses < calls {
					t.Fatalf("provider reads were not attributed to the fleet reader: %d, %v", auditUses, err)
				}
			}
		})
	}
}
