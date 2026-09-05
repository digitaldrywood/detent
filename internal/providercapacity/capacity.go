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

type Report struct {
	Provider           string    `json:"provider"`
	Backend            string    `json:"backend"`
	AccountAlias       string    `json:"account_alias"`
	SharedAccountAlias string    `json:"shared_account_alias,omitempty"`
	Models             []string  `json:"models"`
	MaxConcurrent      int       `json:"max_concurrent"`
	Availability       string    `json:"availability"`
	ObservedAt         time.Time `json:"observed_at"`
	ResetAt            time.Time `json:"reset_at,omitzero"`
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
		if !slices.Contains([]string{"unknown", "available", "exhausted"}, r.Availability) || r.ObservedAt.IsZero() || !r.ResetAt.IsZero() && !r.ResetAt.After(r.ObservedAt) {
			return errors.New("provider reports need an observation time, valid availability and a later reset hint")
		}
	}
	return nil
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
