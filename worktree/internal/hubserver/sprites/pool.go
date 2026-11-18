package sprites

import (
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
)

const SandboxUnavailableReason = "Sandbox on a Fly Sprite needs a passing isolation probe on that runner: update the runner, then switch it to Sandbox once it reports sandbox support."

type PoolSettings struct {
	ShareIdleCapacity bool              `json:"share_idle_capacity"`
	IsolationTier     string            `json:"isolation_tier"`
	Placement         *policy.Placement `json:"placement,omitempty"`
	MinRunners        int               `json:"min_runners"`
	MaxRunners        int               `json:"max_runners"`
	IdleSeconds       int               `json:"idle_seconds"`
	Bootstrap         string            `json:"bootstrap"`
	Revision          int64             `json:"revision"`
}

type ScaleInput struct {
	Depth, Free, Pending, Count, Floor, Ceiling, Idle int
	CreateLimit                                       *int
}

type ScaleDecision struct {
	Create, Delete int
}

func DecideScale(input ScaleInput) ScaleDecision {
	create := min(max(input.Floor-input.Count, input.Depth-input.Free-input.Pending, 0), max(input.Ceiling-input.Count, 0))
	if input.CreateLimit != nil {
		create = min(create, *input.CreateLimit)
	}
	if create > 0 {
		return ScaleDecision{Create: create}
	}
	surplus := max(input.Free+input.Pending-input.Depth, 0)
	if input.Depth == 0 {
		surplus = input.Idle
	}
	return ScaleDecision{Delete: min(input.Idle, max(input.Count-input.Floor, 0), max(surplus, input.Count-input.Ceiling))}
}

func ValidatePoolAccess(tier *string) error {
	if tier != nil && *tier == isolation.Sandbox {
		return hubapi.PublicInvalid(SandboxUnavailableReason)
	}
	return nil
}

func ValidatePoolSettings(settings *PoolSettings) error {
	if settings.IsolationTier == "" {
		settings.IsolationTier = isolation.NativeTrusted
	}
	if settings.IsolationTier != isolation.Sandbox && settings.IsolationTier != isolation.NativeTrusted {
		return hubapi.PublicInvalid("Sprite pool access must be sandbox or native-trusted")
	}
	if settings.Placement != nil {
		resolved := settings.Placement.Resolved()
		if err := resolved.Validate(); err != nil {
			return hubapi.PublicInvalid(err.Error())
		}
		settings.Placement = &resolved
	}
	if settings.IdleSeconds == 0 {
		settings.IdleSeconds = 300
	}
	if settings.MinRunners < 0 || settings.MaxRunners < settings.MinRunners || settings.IdleSeconds < 30 || settings.IdleSeconds > 86400 || len(settings.Bootstrap) > 65536 || settings.Revision < 0 {
		return hubapi.PublicInvalid("Sprite pools require 0 <= min_runners <= max_runners, an idle threshold of 30 to 86400 seconds, optional bootstrap of at most 65536 bytes, and a nonnegative revision")
	}
	return nil
}
