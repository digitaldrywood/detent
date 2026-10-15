package conversation

import (
	"encoding/json"
	"testing"
)

func TestPreferencesNormalized(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value Preferences
		want  Preferences
	}{
		{
			name:  "empty becomes auto",
			value: Preferences{},
			want:  Preferences{Model: PreferenceAuto, ReasoningEffort: PreferenceAuto, Access: PreferenceAuto},
		},
		{
			name:  "case and space are folded",
			value: Preferences{Model: " GPT-6-Astra ", ReasoningEffort: " HIGH ", Access: " Read_Only "},
			want:  Preferences{Model: "GPT-6-Astra", ReasoningEffort: EffortHigh, Access: AccessReadOnly},
		},
		{
			name:  "auto is preserved",
			value: Preferences{Model: "AUTO", ReasoningEffort: "auto", Access: "auto"},
			want:  Preferences{Model: PreferenceAuto, ReasoningEffort: PreferenceAuto, Access: PreferenceAuto},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := test.value.Normalized(); got != test.want {
				t.Fatalf("Normalized() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestValidatePreferences(t *testing.T) {
	t.Parallel()
	models := ModelChoices([]string{"gpt-6-astra", "claude-opus-5"})
	// A model whose runner published its own ladder: "max" is a rung it has
	// and the fixed vocabulary does not.
	laddered := []ModelChoice{
		{ID: "gpt-6-astra", Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
		{ID: "claude-opus-5"},
	}
	tests := []struct {
		name    string
		value   Preferences
		models  []ModelChoice
		wantErr bool
	}{
		{name: "all auto", value: DefaultPreferences(), models: models},
		{name: "known model", value: Preferences{Model: "gpt-6-astra"}, models: models},
		{name: "unknown model", value: Preferences{Model: "gpt-4"}, models: models, wantErr: true},
		{name: "no choices refuses a model", value: Preferences{Model: "gpt-6-astra"}, wantErr: true},
		{name: "effort low", value: Preferences{ReasoningEffort: EffortLow}, models: models},
		{name: "effort medium", value: Preferences{ReasoningEffort: EffortMedium}, models: models},
		{name: "effort high", value: Preferences{ReasoningEffort: EffortHigh}, models: models},
		{name: "effort max is refused without a published ladder", value: Preferences{ReasoningEffort: "max"}, models: models, wantErr: true},
		{
			name:   "effort max is accepted from the model's own ladder",
			value:  Preferences{Model: "gpt-6-astra", ReasoningEffort: "max"},
			models: laddered,
		},
		{
			name:    "another model's ladder does not widen this one",
			value:   Preferences{Model: "claude-opus-5", ReasoningEffort: "max"},
			models:  laddered,
			wantErr: true,
		},
		{
			name:    "a ladder rung is refused while the model is auto",
			value:   Preferences{ReasoningEffort: "max"},
			models:  laddered,
			wantErr: true,
		},
		{name: "access read only", value: Preferences{Access: AccessReadOnly}, models: models},
		{name: "access full", value: Preferences{Access: AccessFull}, models: models},
		{name: "access other is refused", value: Preferences{Access: "write"}, models: models, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ValidatePreferences(test.value, test.models)
			if test.wantErr != (err != nil) {
				t.Fatalf("ValidatePreferences() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func TestPreferencesResolvedValues(t *testing.T) {
	t.Parallel()
	auto := DefaultPreferences()
	if got := auto.ModelValue(); got != "" {
		t.Fatalf("auto ModelValue() = %q, want empty", got)
	}
	if got := auto.EffortValue(); got != "" {
		t.Fatalf("auto EffortValue() = %q, want empty", got)
	}
	if got := auto.AccessValue(); got != "" {
		t.Fatalf("auto AccessValue() = %q, want empty", got)
	}
	explicit := Preferences{Model: "gpt-6-astra", ReasoningEffort: EffortHigh, Access: AccessReadOnly}
	if got := explicit.ModelValue(); got != "gpt-6-astra" {
		t.Fatalf("ModelValue() = %q", got)
	}
	if got := explicit.EffortValue(); got != EffortHigh {
		t.Fatalf("EffortValue() = %q", got)
	}
	if got := explicit.AccessValue(); got != AccessReadOnly {
		t.Fatalf("AccessValue() = %q", got)
	}
	if !explicit.ReadOnly() {
		t.Fatal("ReadOnly() = false, want true for access read_only")
	}
	full := Preferences{Access: AccessFull}
	if full.ReadOnly() {
		t.Fatal("ReadOnly() = true for access full")
	}
}

// TestPreferencesJSON pins the wire shape the client reads: three fields,
// each "auto" or a value, never omitted.
func TestPreferencesJSON(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(DefaultPreferences())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if string(encoded) != `{"model":"auto","reasoning_effort":"auto","access":"auto"}` {
		t.Fatalf("Marshal() = %s", encoded)
	}
	var decoded Preferences
	if err := json.Unmarshal([]byte(`{"model":"gpt-6-astra","reasoning_effort":"low","access":"full"}`), &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	want := Preferences{Model: "gpt-6-astra", ReasoningEffort: EffortLow, Access: AccessFull}
	if decoded != want {
		t.Fatalf("Unmarshal() = %#v, want %#v", decoded, want)
	}
}

func TestStatusValues(t *testing.T) {
	t.Parallel()
	if !StatusActive.Valid() || !StatusSettled.Valid() {
		t.Fatal("active and settled must be valid statuses")
	}
	if Status("archived").Valid() {
		t.Fatal("archived is no longer a status (decisions section 14)")
	}
}
