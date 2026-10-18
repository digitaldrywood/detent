package operatortool

import (
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/billing"
)

const PlatformAdjustAICredits = "platform_adjust_ai_credits"

type PlatformCreditArguments struct {
	OrganizationID string `json:"organization_id"`
	billing.CreditAdjustment
}

func PlatformCreditCatalog() []Definition {
	return []Definition{{
		Name:        PlatformAdjustAICredits,
		Description: "Adjust an organization's AI credit balance in USD. Positive amounts grant complimentary credits; negative amounts correct the balance. Requires platform entitlement administrator authority. Replay the same idempotency key, amount and reason to avoid applying a command twice.",
		InputSchema: json.RawMessage(`{"type":"object","required":["organization_id","amount_usd","reason","idempotency_key"],"properties":{"organization_id":{"type":"string","minLength":1},"amount_usd":{"type":"string","description":"Signed nonzero USD decimal with at most six decimal places, for example 10.00 or -2.50"},"reason":{"type":"string","minLength":1,"maxLength":500},"idempotency_key":{"type":"string","minLength":1,"maxLength":128}},"additionalProperties":false}`),
		Annotations: Annotations{Destructive: true, Idempotent: true},
		Meta:        ToolMetadata{Toolset: "platform"},
	}}
}
