package config

import "strings"

func NoProgressSpendLimitMultiplier(effort string) float64 {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "medium":
		return 1.5
	case "high":
		return 3
	case "xhigh":
		return 6
	case "max", "ultracode":
		return 8
	default:
		return 1
	}
}

func EffectiveNoProgressSpendLimitUSD(limitUSD float64, effort string) float64 {
	if limitUSD <= 0 {
		return limitUSD
	}
	return limitUSD * NoProgressSpendLimitMultiplier(effort)
}
