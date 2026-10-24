package budget

import (
	"errors"
	"fmt"
	"math"
)

type CostBucket string

const (
	LunaAPI              CostBucket = "luna_api"
	RunnerAPI            CostBucket = "runner_api"
	SpriteInfrastructure CostBucket = "sprite_infrastructure"
	Aggregate            CostBucket = "total"
)

type ExhaustionMode string

const (
	DrainIssue ExhaustionMode = "drain_issue"
	HardStop   ExhaustionMode = "hard_stop"
)

type MonthlyPolicy struct {
	Enabled      bool           `json:"enabled"`
	Mode         ExhaustionMode `json:"mode,omitempty"`
	LunaMicros   *int64         `json:"luna_api_micros,omitempty"`
	RunnerMicros *int64         `json:"runner_api_micros,omitempty"`
	SpriteMicros *int64         `json:"sprite_infrastructure_micros,omitempty"`
	TotalMicros  *int64         `json:"total_micros,omitempty"`
}

func (p MonthlyPolicy) EffectiveMode() ExhaustionMode {
	if p.Mode == "" {
		return DrainIssue
	}
	return p.Mode
}

func (p MonthlyPolicy) Validate() error {
	if p.EffectiveMode() != DrainIssue && p.EffectiveMode() != HardStop {
		return errors.New("monthly budget mode must be drain_issue or hard_stop")
	}
	for _, bucket := range []CostBucket{LunaAPI, RunnerAPI, SpriteInfrastructure, Aggregate} {
		if cap := p.cap(bucket); cap != nil && *cap < 0 {
			return fmt.Errorf("monthly %s budget must be nonnegative", bucket)
		}
	}
	return nil
}

func (p MonthlyPolicy) cap(bucket CostBucket) *int64 {
	switch bucket {
	case LunaAPI:
		return p.LunaMicros
	case RunnerAPI:
		return p.RunnerMicros
	case SpriteInfrastructure:
		return p.SpriteMicros
	case Aggregate:
		return p.TotalMicros
	default:
		return nil
	}
}

type MonthlySpend struct {
	LunaMicros   int64 `json:"luna_api_micros"`
	RunnerMicros int64 `json:"runner_api_micros"`
	SpriteMicros int64 `json:"sprite_infrastructure_micros"`
}

func (s MonthlySpend) amount(bucket CostBucket) (int64, error) {
	if s.LunaMicros < 0 || s.RunnerMicros < 0 || s.SpriteMicros < 0 {
		return 0, errors.New("monthly spend must be nonnegative")
	}
	switch bucket {
	case LunaAPI:
		return s.LunaMicros, nil
	case RunnerAPI:
		return s.RunnerMicros, nil
	case SpriteInfrastructure:
		return s.SpriteMicros, nil
	case Aggregate:
		if s.LunaMicros > math.MaxInt64-s.RunnerMicros || s.LunaMicros+s.RunnerMicros > math.MaxInt64-s.SpriteMicros {
			return 0, errors.New("aggregate monthly spend exceeds integer precision")
		}
		return s.LunaMicros + s.RunnerMicros + s.SpriteMicros, nil
	default:
		return 0, fmt.Errorf("unknown cost bucket %q", bucket)
	}
}

type CostExposure struct {
	LunaAPI              bool `json:"luna_api"`
	RunnerAPI            bool `json:"runner_api"`
	SpriteInfrastructure bool `json:"sprite_infrastructure"`
}

func (e CostExposure) affects(bucket CostBucket) bool {
	switch bucket {
	case LunaAPI:
		return e.LunaAPI
	case RunnerAPI:
		return e.RunnerAPI
	case SpriteInfrastructure:
		return e.SpriteInfrastructure
	case Aggregate:
		return e.LunaAPI || e.RunnerAPI || e.SpriteInfrastructure
	default:
		return false
	}
}

type MonthlyConstraint struct {
	Scope  string
	Policy MonthlyPolicy
	Spend  MonthlySpend
}

type MonthlyExhaustion struct {
	Scope         string         `json:"scope"`
	Bucket        CostBucket     `json:"bucket"`
	Mode          ExhaustionMode `json:"mode"`
	CurrentMicros int64          `json:"current_micros"`
	CapMicros     int64          `json:"cap_micros"`
}

type MonthlyDecision struct {
	Allowed   bool                `json:"allowed"`
	Exhausted []MonthlyExhaustion `json:"exhausted"`
}

func CheckMonthly(organization, project MonthlyConstraint, exposure CostExposure, admitted bool) (MonthlyDecision, error) {
	decision := MonthlyDecision{Allowed: true, Exhausted: []MonthlyExhaustion{}}
	for _, constraint := range []MonthlyConstraint{organization, project} {
		if err := constraint.Policy.Validate(); err != nil {
			return MonthlyDecision{}, err
		}
		if !constraint.Policy.Enabled {
			continue
		}
		for _, bucket := range []CostBucket{LunaAPI, RunnerAPI, SpriteInfrastructure, Aggregate} {
			cap := constraint.Policy.cap(bucket)
			if cap == nil || !exposure.affects(bucket) {
				continue
			}
			amount, err := constraint.Spend.amount(bucket)
			if err != nil {
				return MonthlyDecision{}, err
			}
			if amount < *cap {
				continue
			}
			mode := constraint.Policy.EffectiveMode()
			decision.Exhausted = append(decision.Exhausted, MonthlyExhaustion{Scope: constraint.Scope, Bucket: bucket, Mode: mode, CurrentMicros: amount, CapMicros: *cap})
			if !admitted || mode == HardStop {
				decision.Allowed = false
			}
		}
	}
	return decision, nil
}
