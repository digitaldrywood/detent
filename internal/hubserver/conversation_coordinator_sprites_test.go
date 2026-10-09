package hubserver

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCoordinatorSpriteIntakeInstructions(t *testing.T) {
	t.Parallel()
	start := strings.Index(coordinatorInstructions, "Sprites onboarding:")
	end := strings.Index(coordinatorInstructions, "What you cannot do:")
	if start < 0 || end <= start {
		t.Fatal("missing Sprite onboarding guidance")
	}
	intake := coordinatorInstructions[start:end]
	for _, phrase := range []string{"I want to add sprites", "add a runner", "I need more capacity", "set up Fly", "hosted runner", "more capacity", "AI guided conversation pre-seeded from the Runners page"} {
		t.Run(phrase, func(t *testing.T) {
			if !strings.Contains(intake, phrase) || !strings.Contains(intake, "as intake intent") || !strings.Contains(intake, "Start the same guided intake") {
				t.Fatalf("ordinary wording %q must start Sprite intake", phrase)
			}
		})
	}
	previous := -1
	for _, step := range []string{"Start with get_sprite_pool", "1. Token setup:", "2. How many runners?", "3. Access:", "4. Once token, bounds and access are resolved"} {
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
		{"read instead of asking", []string{"token presence/validation", "current floor/ceiling", "bootstrap_configured", "connected_runners", "provider_readiness", "never ask the user for information this read provides", "If already valid, skip token setup"}},
		{"one unanswered question", []string{"never repeat an answered question", "one at a time", "accepted with one word", "default 1/1"}},
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
		{"sandbox", "set_sprite_pool", `{"min_runners":0,"max_runners":1,"isolation_tier":"sandbox"}`, true},
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
		{"largest pool", "set_sprite_pool", `{"min_runners":100,"max_runners":100,"idle_seconds":86400}`, true},
		{"smallest idle", "set_sprite_pool", `{"min_runners":0,"max_runners":1,"idle_seconds":30}`, true},
		{"token forbidden", "set_sprites_token", `{"token":"SECRET"}`, false},
		{"foreign project", "get_sprite_pool", `{"project_id":"foreign"}`, false},
		{"log name forbidden", "get_sprite_bootstrap_log", `{"name":"foreign"}`, false},
		{"scale bounds forbidden", "scale_up_sprite_pool", `{"max_runners":1}`, false},
		{"missing floor", "set_sprite_pool", `{"max_runners":1}`, false},
		{"missing ceiling", "set_sprite_pool", `{"min_runners":0}`, false},
		{"negative floor", "set_sprite_pool", `{"min_runners":-1,"max_runners":1}`, false},
		{"inverted bounds", "set_sprite_pool", `{"min_runners":2,"max_runners":1}`, false},
		{"ceiling too high", "set_sprite_pool", `{"min_runners":0,"max_runners":101}`, false},
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
			if err != nil && strings.Contains(err.Error(), "SECRET") {
				t.Fatal("argument error exposed a credential")
			}
		})
	}
}
