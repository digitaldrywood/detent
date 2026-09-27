package templates

import (
	"net/url"

	"github.com/a-h/templ"
)

type HostedPageData struct {
	CreationKey          string
	Provisioning         HostedProvisioning
	PendingOrganizations []HostedOrganizationChoice
	Base                 string
	SharedOrigin         bool
	BillingEnabled       bool
	BillingCanPurchase   bool
	BillingStatus        string
	BillingMessage       string
	BillingCheckedAt     string
	BillingPrices        []HostedBillingPrice
	BillingAudit         []HostedBillingAudit
	PlanName             string
	PlanSource           string
	UsageWindow          string
	PlanGrants           []string
	Allowances           []HostedAllowanceRow
	Assets               AssetPaths
	Title                string
	Email                string
	OrganizationName     string
	OrganizationID       string
	CSRF                 string
	Error                string
	Notice               string
	Mode                 string
	CanManage            bool
	CanCreate            bool
	CanManageRunners     bool
	CanManageOwnership   bool
	CanSupport           bool
	SupportActor         string
	SupportReason        string
	SupportExpiry        string
	Organizations        []HostedOrganizationChoice
	Projects             []HostedProjectChoice
	Members              []HostedMember
}

type HostedBillingPrice struct {
	ID    string
	Label string
}

type HostedBillingAudit struct {
	Actor   string `json:"actor"`
	Action  string `json:"action"`
	Summary string `json:"summary"`
	At      string `json:"at"`
}

type HostedOrganizationChoice struct {
	ID     string
	Name   string
	Status string
}

type HostedProvisioning struct {
	ID        string
	Name      string
	State     string
	Step      string
	Error     string
	CanResume bool
}

func hostedProvisioningLabel(state string) string {
	switch state {
	case "requested":
		return "Waiting to start"
	case "allocating":
		return "Setting up"
	case "failed":
		return "Needs attention"
	default:
		return state
	}
}

type HostedProjectChoice struct {
	ID   string
	Name string
}

type HostedMember struct {
	ID     string
	UserID string
	Role   string
}

func hostedPageTitle(data HostedPageData) string {
	if data.Title != "" {
		return data.Title
	}
	switch data.Mode {
	case "login":
		return "Sign in"
	case "onboarding":
		return "Your organization"
	case "denied":
		return "Access denied"
	case "chooser":
		return "Organizations"
	case "create":
		return "Create organization"
	case "provisioning", "delete":
		return data.Title
	case "join":
		return "Join organization"
	case "support":
		return "Support access"
	default:
		return "Organization"
	}
}

func hostedURL(data HostedPageData, path string) templ.SafeURL {
	if data.SharedOrigin && data.Base == "" && path == "/organization" {
		return templ.SafeURL("/organizations")
	}
	return templ.SafeURL(data.Base + path)
}

func hostedSignInURL(data HostedPageData) templ.SafeURL {
	if data.SharedOrigin && data.Mode != "login" {
		return templ.SafeURL("/organizations")
	}
	return templ.SafeURL("/auth/oidc/start")
}

func hostedJoinURL(data HostedPageData) templ.SafeURL {
	if data.SharedOrigin {
		return templ.SafeURL("/invitations/join")
	}
	return templ.SafeURL("/auth/oidc/start?unscoped=1")
}

func hostedProjectPath(project string) string {
	return "/projects/" + url.PathEscape(project)
}

func hostedMemberPath(member string, action string) string {
	return "/organization/members/" + url.PathEscape(member) + "/" + action
}

func hostedProjectMode(data HostedPageData) bool {
	return data.OrganizationID != "" && data.Mode == "organization"
}

type HostedAllowanceRow struct {
	LimitOnly   bool
	Label       string
	Consumption string
	Allowance   string
	OverLimit   bool
}
