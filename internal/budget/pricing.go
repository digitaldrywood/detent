package budget

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const DefaultPricingPath = "priv/pricing/models.yaml"

type ModelPricing struct {
	USDPerInputToken  float64
	USDPerOutputToken float64
}

type PricingTable map[string]ModelPricing

func DefaultPricingTable() PricingTable {
	return PricingTable{
		"gpt-5.5": {
			USDPerInputToken:  0.000005,
			USDPerOutputToken: 0.000030,
		},
		"gpt-5.4": {
			USDPerInputToken:  0.0000025,
			USDPerOutputToken: 0.000015,
		},
		"gpt-5.4-mini": {
			USDPerInputToken:  0.00000075,
			USDPerOutputToken: 0.0000045,
		},
		"gpt-5.3-codex": {
			USDPerInputToken:  0.00000175,
			USDPerOutputToken: 0.000014,
		},
	}
}

func LoadPricing(path string) (PricingTable, error) {
	if strings.TrimSpace(path) == "" {
		return DefaultPricingTable(), nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read pricing file: %w", err)
	}

	pricing, err := DecodePricing(raw)
	if err != nil {
		return nil, fmt.Errorf("decode pricing file: %w", err)
	}
	return pricing, nil
}

func PricingForConfig(cfg Config) (PricingTable, error) {
	path := strings.TrimSpace(cfg.PricingPath)
	if path == "" || path == DefaultPricingPath {
		return DefaultPricingTable(), nil
	}
	return LoadPricing(path)
}

func DecodePricing(raw []byte) (PricingTable, error) {
	var decoded map[string]any
	if err := yaml.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}

	models := decoded
	if value, ok := decoded["models"]; ok {
		models = mapValue(value)
	}

	pricing := PricingTable{}
	for model, row := range models {
		model = normalizeModel(model)
		if model == "" {
			continue
		}
		modelPricing, ok := normalizePricingRow(row)
		if !ok {
			continue
		}
		pricing[model] = modelPricing
	}
	return pricing, nil
}

func (p PricingTable) Lookup(model string) (ModelPricing, bool) {
	if p == nil {
		p = DefaultPricingTable()
	}

	row, ok := p[normalizeModel(model)]
	return row, ok
}

func UsageCostUSD(pricing PricingTable, model string, inputTokens int64, outputTokens int64) (float64, bool) {
	modelPricing, ok := pricing.Lookup(model)
	if !ok {
		return 0, false
	}
	return float64(nonNegative(inputTokens))*modelPricing.USDPerInputToken +
		float64(nonNegative(outputTokens))*modelPricing.USDPerOutputToken, true
}

func normalizePricingRow(value any) (ModelPricing, bool) {
	row := mapValue(value)
	if row == nil {
		return ModelPricing{}, false
	}

	input, inputOK := numericValue(row["usd_per_input_token"])
	if !inputOK {
		input, inputOK = perTokenFromMillion(row["input_usd_per_1m_tokens"])
	}

	output, outputOK := numericValue(row["usd_per_output_token"])
	if !outputOK {
		output, outputOK = perTokenFromMillion(row["output_usd_per_1m_tokens"])
	}

	if !inputOK || !outputOK || input < 0 || output < 0 {
		return ModelPricing{}, false
	}

	return ModelPricing{
		USDPerInputToken:  input,
		USDPerOutputToken: output,
	}, true
}

func perTokenFromMillion(value any) (float64, bool) {
	number, ok := numericValue(value)
	if !ok {
		return 0, false
	}
	return number / 1_000_000, true
}

func mapValue(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		return typed
	default:
		return nil
	}
}

func numericValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case float64:
		return typed, true
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0, false
		}
		return number, true
	default:
		return 0, false
	}
}

func normalizeModel(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}
