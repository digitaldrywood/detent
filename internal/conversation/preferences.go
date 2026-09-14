package conversation

import (
	"fmt"
	"slices"
	"strings"
)

// Turn preference values (decisions section 14). Every field is either
// PreferenceAuto, which defers to the project's configured defaults on the
// runner, or an explicit value that travels to the coordinator turn.
const (
	// PreferenceAuto defers the field to the project's configured default.
	PreferenceAuto = "auto"

	EffortLow    = "low"
	EffortMedium = "medium"
	EffortHigh   = "high"

	// AccessReadOnly forbids the turn from changing anything; AccessFull
	// gives it the run's ordinary access. Coordinator turns are read-only
	// whatever the preference says.
	AccessReadOnly = "read_only"
	AccessFull     = "full"
)

var (
	reasoningEfforts = []string{PreferenceAuto, EffortLow, EffortMedium, EffortHigh}
	accessLevels     = []string{PreferenceAuto, AccessReadOnly, AccessFull}
)

// ReasoningEfforts returns the reasoning effort choices, "auto" first.
func ReasoningEfforts() []string { return slices.Clone(reasoningEfforts) }

// AccessLevels returns the access choices, "auto" first.
func AccessLevels() []string { return slices.Clone(accessLevels) }

// Preferences are a conversation's turn preferences. They are stored on the
// conversation, published on its resource and applied to every turn the
// conversation produces.
type Preferences struct {
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
	Access          string `json:"access"`
}

// DefaultPreferences is the conversation default: everything auto.
func DefaultPreferences() Preferences {
	return Preferences{Model: PreferenceAuto, ReasoningEffort: PreferenceAuto, Access: PreferenceAuto}
}

// Normalized trims each field and folds an empty value to auto. Model
// identifiers keep their case because a provider catalog is case-sensitive;
// effort and access are lower-cased because their vocabulary is fixed.
func (p Preferences) Normalized() Preferences {
	p.Model = strings.TrimSpace(p.Model)
	if p.Model == "" || strings.EqualFold(p.Model, PreferenceAuto) {
		p.Model = PreferenceAuto
	}
	p.ReasoningEffort = normalizeChoice(p.ReasoningEffort)
	p.Access = normalizeChoice(p.Access)
	return p
}

func normalizeChoice(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return PreferenceAuto
	}
	return value
}

// ModelChoice is one model a conversation may select, with the reasoning
// efforts that model supports.
//
// Efforts are per model because a provider's are: one model offers low through
// max, the next only medium. When a runner has published a model's ladder, that
// ladder is what the conversation may choose from for that model; when it has
// published none, the fixed vocabulary is all there is to go on.
type ModelChoice struct {
	ID      string
	Efforts []string
}

// ModelChoices wraps bare identifiers as choices with no published ladder.
func ModelChoices(models []string) []ModelChoice {
	choices := make([]ModelChoice, 0, len(models))
	for _, model := range models {
		choices = append(choices, ModelChoice{ID: model})
	}
	return choices
}

// ValidatePreferences checks a normalized preference set against the model
// choices the bootstrap published. An empty choice list accepts auto only:
// no runner has reported a catalog, so no explicit model can be honoured.
//
// The effort is accepted when it is in the fixed vocabulary, or when the
// selected model publishes it. A hub that refused a model's own effort would
// be offering a choice it would then reject.
func ValidatePreferences(preferences Preferences, models []ModelChoice) error {
	preferences = preferences.Normalized()
	index := slices.IndexFunc(models, func(choice ModelChoice) bool { return choice.ID == preferences.Model })
	if preferences.Model != PreferenceAuto && index < 0 {
		return fmt.Errorf("preferences: model %q is not one of the available choices", preferences.Model)
	}
	published := []string(nil)
	if index >= 0 {
		published = models[index].Efforts
	}
	if !slices.Contains(reasoningEfforts, preferences.ReasoningEffort) && !slices.Contains(published, preferences.ReasoningEffort) {
		allowed := append(ReasoningEfforts(), published...)
		return fmt.Errorf("preferences: reasoning_effort must be one of %s", strings.Join(slices.Compact(allowed), ", "))
	}
	if !slices.Contains(accessLevels, preferences.Access) {
		return fmt.Errorf("preferences: access must be one of %s", strings.Join(accessLevels, ", "))
	}
	return nil
}

// ModelValue is the explicit model, empty when the preference is auto.
func (p Preferences) ModelValue() string { return explicit(p.Normalized().Model) }

// EffortValue is the explicit reasoning effort, empty when auto.
func (p Preferences) EffortValue() string { return explicit(p.Normalized().ReasoningEffort) }

// AccessValue is the explicit access level, empty when auto.
func (p Preferences) AccessValue() string { return explicit(p.Normalized().Access) }

// ReadOnly reports whether the conversation asked for read-only turns.
func (p Preferences) ReadOnly() bool { return p.AccessValue() == AccessReadOnly }

func explicit(value string) string {
	if value == PreferenceAuto {
		return ""
	}
	return value
}
