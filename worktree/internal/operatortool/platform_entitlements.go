package operatortool

import (
	"encoding/json"
	"time"
)

const (
	PlatformEntitlements      = "platform_entitlements"
	PlatformChangeEntitlement = "platform_change_entitlement"
)

type PlatformPlanReference struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

type PlatformEntitlementPlan struct {
	PlatformPlanReference
	Name            string           `json:"name,omitempty"`
	MonthlyUSDCents *int64           `json:"monthly_usd_cents,omitempty"`
	Features        []string         `json:"features"`
	Allowances      map[string]int64 `json:"allowances"`
}

type PlatformEntitlementChange struct {
	Action           string                   `json:"action"`
	Feature          string                   `json:"feature,omitempty"`
	IdempotencyKey   string                   `json:"idempotency_key"`
	ExpectedRevision int64                    `json:"expected_revision"`
	Plan             PlatformPlanReference    `json:"plan"`
	Definition       *PlatformEntitlementPlan `json:"definition,omitempty"`
	GrantID          string                   `json:"grant_id"`
	ExpiresAt        *time.Time               `json:"expires_at"`
	Reason           string                   `json:"reason"`
}

type PlatformEntitlementArguments struct {
	OrganizationID string `json:"organization_id"`
	PlatformEntitlementChange
}

func PlatformEntitlementCatalog() []Definition {
	return []Definition{
		{Name: PlatformEntitlements, Description: "Read an organization's immutable plan catalog and entitlement revision. Requires entitlement administrator authority.", InputSchema: json.RawMessage(`{"type":"object","required":["organization_id"],"properties":{"organization_id":{"type":"string"}},"additionalProperties":false}`), Annotations: Annotations{ReadOnly: true}, Meta: ToolMetadata{Toolset: "platform"}},
		{Name: PlatformChangeEntitlement, Description: "Create an immutable plan version or set base, subscription, grant or revoke access immediately. Stripe owns subscriptions when billing is enabled. Read platform_entitlements first for the revision; replay the same key and inputs for retries.", InputSchema: json.RawMessage(`{"type":"object","required":["organization_id","action","idempotency_key","expected_revision","reason"],"properties":{"organization_id":{"type":"string"},"action":{"type":"string","enum":["create_plan","base","subscription","end_subscription","grant","revoke"]},"idempotency_key":{"type":"string"},"expected_revision":{"type":"integer","minimum":1},"reason":{"type":"string","minLength":1,"maxLength":500},"plan":{"type":"object","required":["id","version"],"properties":{"id":{"type":"string"},"version":{"type":"integer","minimum":1}},"additionalProperties":false},"definition":{"type":"object","required":["id","version","features","allowances"],"properties":{"id":{"type":"string"},"version":{"type":"integer","minimum":1},"name":{"type":"string"},"monthly_usd_cents":{"type":"integer","minimum":0},"features":{"type":"array","items":{"type":"string"}},"allowances":{"type":"object","additionalProperties":{"type":"integer","minimum":0}}},"additionalProperties":false},"grant_id":{"type":"string"},"expires_at":{"type":["string","null"],"format":"date-time"},"feature":{"type":"string","enum":["model_choice"]}},"additionalProperties":false}`), Annotations: Annotations{Destructive: true, Idempotent: true}, Meta: ToolMetadata{Toolset: "platform"}},
	}
}
