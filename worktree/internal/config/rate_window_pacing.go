package config

import (
	"strings"
)

const (
	RateWindowPacingProportional = "proportional"
	RateWindowPacingOff          = "off"
	RateWindowPacingFloor        = "floor"

	DefaultRateWindowPacingFloorPercent      = 20
	DefaultRateWindowPacingStaleAfterSeconds = 15 * 60
)

type RateWindowPacing struct {
	Mode              string  `yaml:"mode"`
	FloorPercent      float64 `yaml:"floor_percent"`
	StaleAfterSeconds int     `yaml:"stale_after_seconds"`
}

func DefaultRateWindowPacing() RateWindowPacing {
	return RateWindowPacing{
		Mode:              RateWindowPacingProportional,
		FloorPercent:      DefaultRateWindowPacingFloorPercent,
		StaleAfterSeconds: DefaultRateWindowPacingStaleAfterSeconds,
	}
}

func (p RateWindowPacing) Normalized() RateWindowPacing {
	p.Mode = strings.ToLower(strings.TrimSpace(p.Mode))
	if p.Mode == "" {
		p.Mode = RateWindowPacingProportional
	}
	if p.FloorPercent == 0 {
		p.FloorPercent = DefaultRateWindowPacingFloorPercent
	}
	if p.StaleAfterSeconds == 0 {
		p.StaleAfterSeconds = DefaultRateWindowPacingStaleAfterSeconds
	}
	return p
}

func (p RateWindowPacing) Validate(prefix string) []string {
	p = p.Normalized()
	var problems []string
	switch p.Mode {
	case RateWindowPacingProportional, RateWindowPacingOff, RateWindowPacingFloor:
	default:
		problems = append(problems, prefix+".mode must be one of proportional, off, floor")
	}
	if p.FloorPercent <= 0 || p.FloorPercent > 100 {
		problems = append(problems, prefix+".floor_percent must be greater than 0 and less than or equal to 100")
	}
	if p.StaleAfterSeconds <= 0 {
		problems = append(problems, prefix+".stale_after_seconds must be greater than 0")
	}
	return problems
}

func (c Config) RateWindowPacingConfigured() bool {
	for path := range c.configuredFields {
		if strings.HasPrefix(path, "agent.rate_window_pacing.") {
			return true
		}
	}
	if c.configuredFields != nil {
		return false
	}
	return c.Agent.RateWindowPacing.Normalized() != DefaultRateWindowPacing()
}
