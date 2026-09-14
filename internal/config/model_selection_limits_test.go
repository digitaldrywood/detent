package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestModelSelectionSessionLimits(t *testing.T) {
	for _, tt := range []struct {
		name         string
		duration     *int
		tokens       *int64
		disabled     bool
		level        string
		wantDuration int
		wantTokens   int64
	}{
		{name: "inherits", level: "normal", wantDuration: 100, wantTokens: 200},
		{name: "duration override", level: "normal", duration: new(500), wantDuration: 500, wantTokens: 200},
		{name: "token override", level: "normal", tokens: new(int64(600)), wantDuration: 100, wantTokens: 600},
		{name: "explicit zero", level: "normal", duration: new(0), tokens: new(int64(0))},
		{name: "disabled", level: "normal", disabled: true, duration: new(500), wantDuration: 100, wantTokens: 200},
		{name: "ineligible backend", duration: new(500), wantDuration: 100, wantTokens: 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			agent := Agent{MaxSessionDurationMS: 100, MaxSessionTokens: 200, MaxTurnDurationMS: 30, NoProgressTimeoutMS: 40, MaxTurns: 7}
			policy := ModelSelection{Enabled: new(!tt.disabled), Levels: map[string]ModelSelectionDefaults{"normal": {MaxSessionDurationMS: tt.duration, MaxSessionTokens: tt.tokens}}}
			got := policy.SessionAgent(agent, tt.level)
			want := agent
			want.MaxSessionDurationMS, want.MaxSessionTokens = tt.wantDuration, tt.wantTokens
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("SessionAgent = %+v, want %+v", got, want)
			}
			if tt.duration != nil {
				*tt.duration = 999
			}
			if tt.tokens != nil {
				*tt.tokens = 999
			}
			if got.MaxSessionDurationMS != tt.wantDuration || got.MaxSessionTokens != tt.wantTokens {
				t.Fatal("snapshot changed with policy")
			}
		})
	}
}

func TestModelSelectionLimitOverlay(t *testing.T) {
	for _, tt := range []struct {
		name, project string
		wantDuration  int
		wantTokens    int64
	}{
		{"inherit", "effort: low", 500, 600},
		{"override", "max_session_duration_ms: 700", 700, 600},
		{"disable", "max_session_duration_ms: 0\nmax_session_tokens: 0", 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			instance := ModelSelection{Preset: new("sol_first"), Levels: map[string]ModelSelectionDefaults{"normal": {MaxSessionDurationMS: new(500), MaxSessionTokens: new(int64(600))}}}
			var level ModelSelectionDefaults
			if err := yaml.Unmarshal([]byte(tt.project), &level); err != nil {
				t.Fatal(err)
			}
			resolved := ResolveModelSelection(instance, ModelSelection{Levels: map[string]ModelSelectionDefaults{"normal": level}})
			got := resolved.SessionAgent(Agent{}, "normal")
			if got.MaxSessionDurationMS != tt.wantDuration || got.MaxSessionTokens != tt.wantTokens {
				t.Fatalf("limits = %d/%d", got.MaxSessionDurationMS, got.MaxSessionTokens)
			}
			source := "instance"
			if level.MaxSessionDurationMS != nil {
				source = "project"
			}
			if resolved.Sources["levels.normal.max_session_duration_ms"] != source {
				t.Fatal("wrong provenance")
			}
			encoded, err := yaml.Marshal(resolved)
			if err != nil {
				t.Fatal(err)
			}
			var decoded ModelSelection
			if err := yaml.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded.SessionAgent(Agent{}, "normal"), got) {
				t.Fatal("limits lost in YAML round trip")
			}
		})
	}
}

func TestModelSelectionLimitValidation(t *testing.T) {
	for _, field := range []string{"max_session_duration_ms", "max_session_tokens"} {
		for _, value := range []string{"-1", "0", "100"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				var level ModelSelectionDefaults
				if err := yaml.Unmarshal([]byte(field+": "+value), &level); err != nil {
					t.Fatal(err)
				}
				policy := ResolveModelSelection(ModelSelection{Preset: new("sol_first")}, ModelSelection{Levels: map[string]ModelSelectionDefaults{"normal": level}})
				problems := strings.Join(policy.Validate(), "; ")
				if value == "-1" {
					if !strings.Contains(problems, "levels.normal."+field+" must be non-negative") {
						t.Fatalf("problems = %s", problems)
					}
				} else if problems != "" {
					t.Fatal(problems)
				}
			})
		}
	}
}
