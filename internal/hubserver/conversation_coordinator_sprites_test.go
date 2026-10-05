package hubserver

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCoordinatorSpriteArguments(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, tool, arguments string
		valid                 bool
	}{
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
		{"empty bootstrap", "set_sprite_pool", `{"min_runners":0,"max_runners":1,"bootstrap":"  "}`, false},
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
