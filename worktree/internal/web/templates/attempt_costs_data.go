package templates

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

type AttemptCostData struct {
	ID         int64
	Label      string
	Tokens     string
	TokenUSD   string
	ComputeUSD string
	Detail     string
}

func AttemptCost(id int64, number int, workerType, host, raw string) AttemptCostData {
	row := AttemptCostData{ID: id, Label: fmt.Sprintf("Attempt %d · %s", number, workerType), Tokens: "Unavailable", TokenUSD: "Unavailable", ComputeUSD: "Unavailable"}
	var metrics struct {
		TotalTokens    *int64   `json:"total_tokens"`
		TokenUSD       *float64 `json:"token_usd"`
		ComputeUSD     *float64 `json:"compute_usd"`
		CPUSeconds     float64  `json:"cpu_seconds"`
		AvgMemoryBytes float64  `json:"avg_memory_bytes"`
		WallSeconds    float64  `json:"wall_seconds"`
	}
	if json.Unmarshal([]byte(raw), &metrics) != nil {
		return row
	}
	if metrics.TotalTokens != nil {
		row.Tokens = formatInt(*metrics.TotalTokens)
	}
	if metrics.TokenUSD != nil {
		row.TokenUSD = computeCostValue(*metrics.TokenUSD, 1)
	}
	if metrics.ComputeUSD != nil {
		row.ComputeUSD = computeCostValue(*metrics.ComputeUSD, 1)
		row.Detail = fmt.Sprintf("%s · %.2f CPU s · %.2f GB avg · %.2f wall s", host, metrics.CPUSeconds, metrics.AvgMemoryBytes/1e9, metrics.WallSeconds)
	}
	return row
}

// Sub-cent compute remains visible rather than rounding an entire turn to $0.
func computeCostValue(value float64, measured int64) string {
	if measured == 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return "Unavailable"
	}
	if value > 0 && value < 0.000001 {
		return "<$0.000001"
	}
	if value > 0 && value < 0.01 {
		return "$" + strconv.FormatFloat(value, 'f', 6, 64)
	}
	return formatUSD(value)
}

func computeOutcomeValue(value float64, measured, outcomes int64) string {
	if outcomes == 0 {
		return "—"
	}
	return computeCostValue(value, measured)
}
