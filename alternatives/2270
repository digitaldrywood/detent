package operatortool

import "encoding/json"

const (
	BillingStatus       = "billing_usage.hosted_billing_page"
	BillingUsage        = "billing_usage.hosted_billing"
	BillingExport       = "billing_usage.billing_export"
	BillingCheckout     = "billing_usage.hosted_billing_checkout"
	CreditCheckout      = "billing_usage.hosted_credit_checkout"
	CreditAutoFund      = "billing_usage.hosted_credit_auto_fund"
	BillingPortal       = "billing_usage.hosted_billing_portal"
	HostedPlan          = "billing_usage.hosted_plan_page"
	HostedUsage         = "billing_usage.hosted_usage_report"
	BudgetOverrideSet   = "billing_usage.api_budget_override_set"
	BudgetOverrideClear = "billing_usage.api_budget_override_clear"
	UsageReport         = "billing_usage.api_usage"
	IssueExplanation    = "billing_usage.api_issue_explanation"
)

// BillingCatalog describes application capabilities, not platform staff powers.
// Deployment adapters advertise only operations their current authority permits.
func BillingCatalog() []Definition {
	var tools []Definition
	for _, item := range []struct {
		name, description, properties, required string
		write, external                         bool
	}{
		{BillingStatus, "Read owner-only subscription status, approved prices, current allowances and recent safe billing audit.", "", "", false, false},
		{BillingUsage, "Read owner-only billing metadata, entitlement usage and cost drivers.", "", "", false, false},
		{BillingExport, "Export owner-only subscription, allowance, usage and safe audit data with an export identifier and freshness.", "", "", false, false},
		{HostedPlan, "Read the current organization plan and allowances with owner/admin dashboard authority.", "", "", false, false},
		{HostedUsage, "Read usage and monthly infrastructure costs for currently granted projects. Use range month:YYYY-MM for a UTC calendar month.", `"project_id":{"type":"string","maxLength":256},"range":{"type":"string","pattern":"^(24h|7d|30d|90d|month:[0-9]{4}-[0-9]{2})$"}`, "", false, false},
		{BillingCheckout, "Create or resume an approved subscription checkout. Executes directly with current authority.", `"price":{"type":"string","minLength":1,"maxLength":256}`, `,"price"`, true, true},
		{CreditCheckout, "Create or resume checkout for a configured AI credit pack. Executes directly with current authority.", `"price":{"type":"string","minLength":1,"maxLength":256}`, `,"price"`, true, true},
		{CreditAutoFund, "Configure AI credit auto-funding with a configured pack and threshold in USD cents. Changes execute directly with current billing authority.", `"enabled":{"type":"boolean"},"threshold_cents":{"type":"integer","minimum":0},"price":{"type":"string","maxLength":256}`, `,"enabled","threshold_cents","price"`, true, true},
		{BillingPortal, "Create a billing portal session. Executes directly with current authority.", "", "", true, true},
		{BudgetOverrideSet, "Set project daily/per-issue budget overrides through the dashboard command; requires exact operator approval.", `"project_id":{"type":"string","minLength":1,"maxLength":256},"per_day_max_usd":{"type":"number","exclusiveMinimum":0},"per_issue_max_usd":{"type":"number","exclusiveMinimum":0},"duration":{"type":"string","minLength":1,"maxLength":128},"reason":{"type":"string","minLength":1,"maxLength":280}`, `,"project_id","duration","reason"`, true, false},
		{BudgetOverrideClear, "Clear a project daily budget override directly through the dashboard command.", `"project_id":{"type":"string","minLength":1,"maxLength":256}`, `,"project_id"`, true, false},
		{UsageReport, "Read durable daemon usage for currently authorized projects with bounded dates and grouping.", `"project_id":{"type":"string","maxLength":256},"from":{"type":"string","maxLength":10},"to":{"type":"string","maxLength":10},"by":{"type":"string","enum":["day","project","issue","pr","model"]}`, "", false, false},
		{IssueExplanation, "Read the shared dashboard issue explanation within the currently authorized project.", `"project_id":{"type":"string","minLength":1,"maxLength":256},"reference":{"type":"string","minLength":1,"maxLength":256}`, `,"project_id","reference"`, false, false},
	} {
		properties, required := item.properties, ""
		if item.write {
			if properties != "" {
				properties += ","
			}
			properties += `"request_id":{"type":"string","minLength":1,"maxLength":128}`
			required = `,"required":["request_id"` + item.required + `]`
		} else if item.required != "" {
			required = `,"required":[` + item.required[1:] + `]`
		}
		tools = append(tools, Definition{Name: item.name, Description: item.description, InputSchema: json.RawMessage(`{"type":"object","properties":{` + properties + `},"additionalProperties":false` + required + `}`), Annotations: Annotations{ReadOnly: !item.write, Destructive: item.write && item.name != BudgetOverrideClear, Idempotent: true, OpenWorld: item.external}, Meta: ToolMetadata{Toolset: "billing_usage"}})
	}
	return tools
}
