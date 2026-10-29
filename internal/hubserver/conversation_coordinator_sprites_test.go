package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
)

func TestCoordinatorSpritePoolReplay(t *testing.T) {
	for _, test := range []struct {
		name string
		idle *int
	}{
		{"omitted idle threshold", nil},
		{"explicit idle threshold", new(600)},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			scope := nativeScope{organization: f.project.OrganizationID, credential: apiCredential{Scope: apiScopeAdmin, Hash: apikey.HashToken(testHubAdminToken)}}
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT id FROM api_tokens WHERE token_hash=?", scope.credential.Hash).Scan(&scope.credential.ID); err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(t.Context(), nativeOperatorScopeKey{}, func(context.Context) (nativeScope, error) { return scope, nil })
			change := coordinatorSpriteChange{Input: coordinatorSpriteArguments{MinRunners: new(0), MaxRunners: new(0), IdleSeconds: test.idle, ShareIdleCapacity: new(true)}}
			raw, err := json.Marshal(change)
			if err != nil {
				t.Fatal(err)
			}
			action := chat.Action{Kind: "set_sprite_pool", RequestID: "coordinator-sharing", Arguments: raw}
			if _, err := f.service.executeCoordinatorSpriteAction(ctx, action); err != nil {
				t.Fatal(err)
			}
			var initial spritePoolView
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/sprite-pool", testHubAdminToken, nil), &initial)
			wantIdle := 300
			if test.idle != nil {
				wantIdle = *test.idle
			}
			if initial.Revision != 1 || !initial.ShareIdleCapacity || initial.IdleSeconds != wantIdle {
				t.Fatalf("initial settings: %+v", initial)
			}
			input := map[string]any{"idempotency_key": "later-settings", "revision": 1, "min_runners": 0, "max_runners": 0, "idle_seconds": 900, "share_idle_capacity": false, "placement": map[string]any{"mode": "sprites_only"}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/sprite-pool", testHubAdminToken, input), http.StatusOK)
			if _, err := f.service.executeCoordinatorSpriteAction(ctx, action); err != nil {
				t.Fatalf("replay after settings changed: %v", err)
			}
			var saved spritePoolView
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/sprite-pool", testHubAdminToken, nil), &saved)
			if saved.Revision != 2 || saved.ShareIdleCapacity || saved.IdleSeconds != 900 || saved.Placement.Mode != "sprites_only" {
				t.Fatalf("replay overwrote later settings: %+v", saved)
			}
			change.Input.ShareIdleCapacity = new(false)
			action.Arguments, err = json.Marshal(change)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.executeCoordinatorSpriteAction(ctx, action); err == nil {
				t.Fatal("reused key accepted changed action input")
			}
			action.RequestID = "stale-coordinator-sharing"
			if _, err := f.service.executeCoordinatorSpriteAction(ctx, action); err == nil {
				t.Fatal("new command accepted stale revision")
			}
		})
	}
}

func TestCoordinatorSpriteIntakeInstructions(t *testing.T) {
	t.Parallel()
	start := strings.Index(coordinatorInstructions, "Sprites onboarding:")
	end := strings.Index(coordinatorInstructions, "What you cannot do:")
	if start < 0 || end <= start {
		t.Fatal("missing Sprite onboarding guidance")
	}
	intake := coordinatorInstructions[start:end]
	for _, phrase := range []string{"I want to add sprites", "set up Fly", "AI guided conversation pre-seeded from the Runners page"} {
		t.Run(phrase, func(t *testing.T) {
			if !strings.Contains(intake, phrase) || !strings.Contains(intake, "as Sprites intake intent") || !strings.Contains(intake, "Start the same guided intake") {
				t.Fatalf("ordinary wording %q must start Sprite intake", phrase)
			}
		})
	}
	previous := -1
	for _, step := range []string{"Start with get_sprite_pool", "1. Scope:", "2. Token setup:", "3. How many runners?", "4. Access:", "5. Once scope, token, bounds and access are resolved"} {
		index := strings.Index(intake, step)
		if index <= previous {
			t.Fatalf("missing or out-of-order intake step %q", step)
		}
		previous = index
	}
	for _, test := range []struct {
		name string
		want []string
	}{
		{"fleet before Sprites", []string{"First inspect get_project_fleet", "never default capacity questions to Sprites", "Enter this Sprites intake only when the user selects Sprites or the project uses only Sprites"}},
		{"read instead of asking", []string{"token presence/validation", "current floor/ceiling", "bootstrap_configured", "connected_runners", "provider_readiness", "never ask the user for information this read provides", "If already valid, skip token setup"}},
		{"one unanswered question", []string{"never repeat an answered question", "one at a time", "accepted with one word", "default to All projects", "default 1/1"}},
		{"missing token waits", []string{"use set_sprites_token", "wait for the user to save it before continuing", "Read get_sprite_pool again"}},
		{"bounds explain cost", []string{"minimum retained runner capacity", "even without queued work", "ceiling caps growth", "Do not invent prices"}},
		{"combined answer preserves setup", []string{"Omit bootstrap to preserve the saved bootstrap", "call set_sprite_pool exactly once", "wait for the client's approval/submission result"}},
		{"safe observed next action", []string{"No tokens, provider API keys or private credentials in chat or bootstrap", "After the successful approval result, read get_sprite_pool and get_sprite_bootstrap_log", "observed state in plain language", "one next action", "Do not infer Git access from connection alone"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, want := range test.want {
				if !strings.Contains(intake, want) {
					t.Fatalf("missing intake guidance %q", want)
				}
			}
		})
	}
}

func TestCoordinatorSpriteArguments(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, tool, arguments string
		valid                 bool
	}{
		{"sandbox", "set_sprite_pool", `{"min_runners":0,"max_runners":1,"isolation_tier":"sandbox"}`, false},
		{"idle sharing", "set_sprite_pool", `{"min_runners":0,"max_runners":1,"share_idle_capacity":true}`, true},
		{"private capacity", "set_sprite_pool", `{"min_runners":0,"max_runners":1,"share_idle_capacity":false}`, true},
		{"invalid sharing", "set_sprite_pool", `{"min_runners":0,"max_runners":1,"share_idle_capacity":"true"}`, false},
		{"null sharing", "set_sprite_pool", `{"min_runners":0,"max_runners":1,"share_idle_capacity":null}`, false},
		{"full access", "set_sprite_pool", `{"min_runners":0,"max_runners":1,"isolation_tier":"native-trusted"}`, true},
		{"invalid access", "set_sprite_pool", `{"min_runners":0,"max_runners":1,"isolation_tier":"user"}`, false},
		{"null access", "set_sprite_pool", `{"min_runners":0,"max_runners":1,"isolation_tier":null}`, false},
		{"status", "get_sprite_pool", `{}`, true},
		{"connector", "set_sprites_token", `{}`, true},
		{"log", "get_sprite_bootstrap_log", `{}`, true},
		{"retry", "scale_up_sprite_pool", `{}`, true},
		{"first runner", "set_sprite_pool", `{"min_runners":1,"max_runners":1,"bootstrap":"true"}`, true},
		{"preserve bootstrap", "set_sprite_pool", `{"min_runners":0,"max_runners":2}`, true},
		{"disable", "set_sprite_pool", `{"min_runners":0,"max_runners":0,"bootstrap":""}`, true},
		{"former project limit", "set_sprite_pool", `{"min_runners":100,"max_runners":100,"idle_seconds":86400}`, true},
		{"smallest idle", "set_sprite_pool", `{"min_runners":0,"max_runners":1,"idle_seconds":30}`, true},
		{"token forbidden", "set_sprites_token", `{"token":"SECRET"}`, false},
		{"foreign project", "get_sprite_pool", `{"project_id":"foreign"}`, false},
		{"log name forbidden", "get_sprite_bootstrap_log", `{"name":"foreign"}`, false},
		{"scale bounds forbidden", "scale_up_sprite_pool", `{"max_runners":1}`, false},
		{"missing floor", "set_sprite_pool", `{"max_runners":1}`, false},
		{"missing ceiling", "set_sprite_pool", `{"min_runners":0}`, false},
		{"negative floor", "set_sprite_pool", `{"min_runners":-1,"max_runners":1}`, false},
		{"inverted bounds", "set_sprite_pool", `{"min_runners":2,"max_runners":1}`, false},
		{"organization ceiling beyond project limit", "set_sprite_pool", `{"min_runners":0,"max_runners":101}`, true},
		{"fraction", "set_sprite_pool", `{"min_runners":0.5,"max_runners":1}`, false},
		{"string bounds", "set_sprite_pool", `{"min_runners":"0","max_runners":1}`, false},
		{"zero idle explicit", "set_sprite_pool", `{"min_runners":0,"max_runners":1,"idle_seconds":0}`, false},
		{"idle too small", "set_sprite_pool", `{"min_runners":0,"max_runners":1,"idle_seconds":29}`, false},
		{"idle too large", "set_sprite_pool", `{"min_runners":0,"max_runners":1,"idle_seconds":86401}`, false},
		{"empty bootstrap", "set_sprite_pool", `{"min_runners":0,"max_runners":1,"bootstrap":""}`, true},
		{"whitespace bootstrap", "set_sprite_pool", `{"min_runners":0,"max_runners":1,"bootstrap":"  "}`, true},
		{"bootstrap too large", "set_sprite_pool", `{"min_runners":0,"max_runners":1,"bootstrap":"` + strings.Repeat("x", 12001) + `"}`, false},
		{"null field", "set_sprite_pool", `{"min_runners":0,"max_runners":1,"bootstrap":null}`, false},
		{"unknown field", "set_sprite_pool", `{"min_runners":0,"max_runners":1,"approve":true}`, false},
		{"unknown tool", "foreign", `{}`, false},
		{"null object", "get_sprite_pool", `null`, false},
		{"empty payload", "get_sprite_pool", ``, false},
		{"array", "get_sprite_pool", `[]`, false},
		{"trailing payload", "get_sprite_pool", `{} {}`, false},
		{"oversized payload", "get_sprite_pool", `{"x":"` + strings.Repeat("x", coordinatorToolArgumentBytes) + `"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeCoordinatorSpriteArguments(test.tool, json.RawMessage(test.arguments))
			if (err == nil) != test.valid {
				t.Fatalf("validation error=%v, valid=%t", err, test.valid)
			}
			if test.name == "sandbox" && (err == nil || !strings.Contains(coordinatorToolError(err).Content, "Sandbox on a Fly Sprite needs a passing isolation probe on that runner: update the runner, then switch it to Sandbox once it reports sandbox support.")) {
				t.Fatalf("sandbox rejection = %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "SECRET") {
				t.Fatal("argument error exposed a credential")
			}
		})
	}
}
