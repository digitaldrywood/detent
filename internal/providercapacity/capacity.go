package providercapacity

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"time"
)

const MaxAge = 2 * time.Minute

var token = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:/-]{0,127}$`)
var alias = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

type Requirement struct {
	Role    string `json:"role"`
	Backend string `json:"backend"`
	Model   string `json:"model"`
}

// ModelDetail is what the backend's own catalogue says about one of the
// models the report advertises: the label to show a human, which model the
// backend treats as its default, the reasoning efforts that model actually
// supports, and whether the backend has marked it superseded.
//
// It is additive and optional. A report that carries none is exactly the
// report this package accepted before — Models stays the list capacity and
// dispatch match against, and every consumer that only needs identifiers
// keeps reading it. Detail is advisory: it never widens Models, so a model
// named here that Models does not carry is a malformed report.
type ModelDetail struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
	// Provider names the vendor behind the model, e.g. "openai". Empty when
	// the backend does not say and the report's own provider is the answer.
	Provider string `json:"provider,omitempty"`
	// Default marks the model the backend picks when it is given none.
	Default bool `json:"default,omitempty"`
	// ReasoningEfforts is the backend's own ordered list for this model, from
	// least to most. Empty means the backend does not scope efforts per model.
	ReasoningEfforts []string `json:"reasoning_efforts,omitempty"`
	// DefaultReasoningEffort must be one of ReasoningEfforts when both are set.
	DefaultReasoningEffort string `json:"default_reasoning_effort,omitempty"`
	// Legacy marks a model the backend has named a successor for. It stays
	// selectable — an operator may have pinned it deliberately — but a picker
	// shelves it rather than offering it beside the current models.
	Legacy bool `json:"legacy,omitempty"`
}

type Report struct {
	Provider           string        `json:"provider"`
	Backend            string        `json:"backend"`
	AccountAlias       string        `json:"account_alias"`
	SharedAccountAlias string        `json:"shared_account_alias,omitempty"`
	Models             []string      `json:"models"`
	ModelDetails       []ModelDetail `json:"model_details,omitempty"`
	MaxConcurrent      int           `json:"max_concurrent"`
	Availability       string        `json:"availability"`
	ObservedAt         time.Time     `json:"observed_at"`
	ResetAt            time.Time     `json:"reset_at,omitzero"`
}

type Reservation struct {
	Requirement
	Report Report `json:"report"`
	Reason string `json:"reason"`
}

type View struct {
	Report
	Used   int    `json:"used"`
	State  string `json:"state"`
	Reason string `json:"reason"`
}

func (r Requirement) Validate() error {
	if !token.MatchString(r.Role) || !token.MatchString(r.Backend) || !token.MatchString(r.Model) {
		return errors.New("provider requirement needs bounded role, backend and model identifiers")
	}
	return nil
}

func Validate(reports []Report) error {
	if len(reports) > 32 {
		return errors.New("provider reports are limited to 32 backends")
	}
	seen := make(map[string]bool)
	for _, r := range reports {
		if !token.MatchString(r.Provider) || !token.MatchString(r.Backend) || !alias.MatchString(r.AccountAlias) || r.SharedAccountAlias != "" && !alias.MatchString(r.SharedAccountAlias) {
			return errors.New("provider reports need bounded provider/backend identifiers and opaque account aliases")
		}
		if seen[r.Backend] {
			return errors.New("each backend must identify exactly one local account")
		}
		seen[r.Backend] = true
		if r.MaxConcurrent < 1 || r.MaxConcurrent > 10000 || len(r.Models) == 0 || len(r.Models) > 128 {
			return errors.New("provider reports need 1 to 128 models and a concurrency limit between 1 and 10000")
		}
		for _, model := range r.Models {
			if !token.MatchString(model) {
				return errors.New("provider model identifiers must be bounded tokens")
			}
		}
		if err := validateModelDetails(r); err != nil {
			return err
		}
		if !slices.Contains([]string{"unknown", "available", "exhausted"}, r.Availability) || r.ObservedAt.IsZero() || !r.ResetAt.IsZero() && !r.ResetAt.After(r.ObservedAt) {
			return errors.New("provider reports need an observation time, valid availability and a later reset hint")
		}
	}
	return nil
}

// maxReasoningEfforts bounds one model's effort ladder. A backend that offers
// more than this is not describing a ladder a person picks from.
const maxReasoningEfforts = 16

// maxDetailLabel bounds the display label so a report cannot carry prose.
const maxDetailLabel = 128

// validateModelDetails checks the optional per-model detail. Detail describes
// models the report already advertises, so an entry for a model outside Models
// is a malformed report rather than an extra capability.
func validateModelDetails(r Report) error {
	if len(r.ModelDetails) > len(r.Models) {
		return errors.New("provider model details must describe only the reported models")
	}
	seen := make(map[string]bool, len(r.ModelDetails))
	for _, detail := range r.ModelDetails {
		if !token.MatchString(detail.ID) || !slices.Contains(r.Models, detail.ID) {
			return errors.New("provider model details must describe only the reported models")
		}
		if seen[detail.ID] {
			return errors.New("each reported model may carry at most one detail entry")
		}
		seen[detail.ID] = true
		if len([]rune(detail.Label)) > maxDetailLabel || detail.Provider != "" && !token.MatchString(detail.Provider) {
			return errors.New("provider model details need a bounded label and a bounded provider identifier")
		}
		if len(detail.ReasoningEfforts) > maxReasoningEfforts {
			return errors.New("provider model details are limited to 16 reasoning efforts")
		}
		efforts := make(map[string]bool, len(detail.ReasoningEfforts))
		for _, effort := range detail.ReasoningEfforts {
			if !token.MatchString(effort) || efforts[effort] {
				return errors.New("provider model reasoning efforts must be distinct bounded tokens")
			}
			efforts[effort] = true
		}
		if detail.DefaultReasoningEffort != "" && (!token.MatchString(detail.DefaultReasoningEffort) || len(efforts) > 0 && !efforts[detail.DefaultReasoningEffort]) {
			return errors.New("a provider model's default reasoning effort must be one it supports")
		}
	}
	return nil
}

// Detail returns what the backend catalogue said about one model, and whether
// it said anything at all. A report with no detail answers false for every
// model, which is what a hub that has only ever seen identifiers sees.
func (r Report) Detail(model string) (ModelDetail, bool) {
	for _, detail := range r.ModelDetails {
		if detail.ID == model {
			return detail, true
		}
	}
	return ModelDetail{}, false
}

func Load(path string) (reports []Report, resultErr error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("provider capacity report file is unavailable")
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	decoder := json.NewDecoder(io.LimitReader(file, 256*1024+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&reports); err != nil {
		return nil, errors.New("provider capacity report file must contain only the supported report fields")
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("provider capacity report file contains trailing data")
	}
	if len(reports) == 0 {
		return nil, errors.New("provider capacity report file must contain at least one backend")
	}
	return reports, Validate(reports)
}

func (r Report) Pool() string {
	if r.SharedAccountAlias == "" {
		return r.Provider + "/unknown"
	}
	return r.Provider + "/shared/" + r.SharedAccountAlias
}

func (r Report) State(now time.Time) string {
	if now.Before(r.ObservedAt) || !now.Before(r.ObservedAt.Add(MaxAge)) || !r.ResetAt.IsZero() && !now.Before(r.ResetAt) {
		return "unknown"
	}
	return r.Availability
}

func (r Report) Supports(requirement Requirement) bool {
	return r.Backend == requirement.Backend && slices.Contains(r.Models, requirement.Model)
}

func (v View) Summary() string {
	return fmt.Sprintf("%s / %s · account %s · %s · %d / %d reserved · %s", v.Provider, v.Backend, v.AccountAlias, v.State, v.Used, v.MaxConcurrent, v.Reason)
}
