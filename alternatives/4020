package sprites

import (
	"strings"
	"testing"
)

func TestSpriteScaleDecision(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		input ScaleInput
		want  ScaleDecision
	}{
		{"two queued issues on an empty project", ScaleInput{Depth: 2, Ceiling: 2}, ScaleDecision{Create: 2}},
		{"free capacity absorbs queue", ScaleInput{Depth: 2, Free: 2, Count: 1, Ceiling: 2}, ScaleDecision{}},
		{"only unmet depth scales", ScaleInput{Depth: 3, Free: 1, Count: 1, Ceiling: 4}, ScaleDecision{Create: 2}},
		{"ceiling caps a burst", ScaleInput{Depth: 10, Count: 1, Ceiling: 2}, ScaleDecision{Create: 1}},
		{"ceiling already reached", ScaleInput{Depth: 2, Count: 2, Ceiling: 2}, ScaleDecision{}},
		{"floor keeps warm members without work", ScaleInput{Floor: 1, Ceiling: 2}, ScaleDecision{Create: 1}},
		{"pending bootstrap or wake absorbs demand", ScaleInput{Depth: 2, Pending: 2, Count: 2, Ceiling: 3}, ScaleDecision{}},
		{"idle pool scales to zero", ScaleInput{Count: 2, Idle: 2, Ceiling: 2}, ScaleDecision{Delete: 2}},
		{"floor survives idle deletion", ScaleInput{Count: 2, Idle: 2, Floor: 1, Ceiling: 2}, ScaleDecision{Delete: 1}},
		{"busy or recently idle members stay", ScaleInput{Count: 2, Ceiling: 2}, ScaleDecision{}},
		{"queued demand keeps needed idle capacity", ScaleInput{Depth: 1, Free: 1, Count: 1, Idle: 1, Ceiling: 1}, ScaleDecision{}},
		{"surplus idle capacity is removed", ScaleInput{Depth: 1, Free: 2, Count: 2, Idle: 2, Ceiling: 2}, ScaleDecision{Delete: 1}},
		{"disabled pool never provisions", ScaleInput{Depth: 2}, ScaleDecision{}},
		{"shared ceiling reduction overrides queued demand", ScaleInput{Depth: 3, Count: 2, Idle: 2, Floor: 1, Ceiling: 1}, ScaleDecision{Delete: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := DecideScale(test.input); got != test.want {
				t.Fatalf("decision = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestSpritePoolSettingsValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		settings PoolSettings
		valid    bool
	}{
		{"enabled without bootstrap", PoolSettings{MinRunners: 2, MaxRunners: 2}, true},
		{"bootstrap byte limit", PoolSettings{MaxRunners: 1, Bootstrap: strings.Repeat("x", 65536)}, true},
		{"bootstrap too large", PoolSettings{MaxRunners: 1, Bootstrap: strings.Repeat("x", 65537)}, false},
		{"negative floor", PoolSettings{MinRunners: -1, MaxRunners: 1}, false},
		{"inverted bounds", PoolSettings{MinRunners: 2, MaxRunners: 1}, false},
		{"organization ceiling beyond project limit", PoolSettings{MaxRunners: 101}, true},
		{"idle too small", PoolSettings{MaxRunners: 1, IdleSeconds: 29}, false},
		{"idle too large", PoolSettings{MaxRunners: 1, IdleSeconds: 86401}, false},
		{"negative revision", PoolSettings{MaxRunners: 1, Revision: -1}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidatePoolSettings(&test.settings); (err == nil) != test.valid {
				t.Fatalf("validation error = %v, valid = %t", err, test.valid)
			}
		})
	}
}
