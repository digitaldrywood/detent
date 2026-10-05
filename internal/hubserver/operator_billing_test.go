package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

const billingMCPMeta = `{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"billing-fixture","version":"1"},"yolo":true}`

type billingMCPReply struct {
	Result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Structured json.RawMessage `json:"structuredContent"`
	} `json:"result"`
	Error json.RawMessage `json:"error"`
}

func (f *browserHostedFixture) billingTool(t *testing.T, account, name, args string) billingMCPReply {
	t.Helper()
	response := f.billingAPI(t, account, http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`,"_meta":`+billingMCPMeta+`}}`)
	var reply billingMCPReply
	if response.Code != http.StatusOK && response.Code != http.StatusBadRequest || json.Unmarshal(response.Body.Bytes(), &reply) != nil {
		t.Fatalf("MCP=%d %s", response.Code, response.Body.String())
	}
	return reply
}

func billingPreview(t *testing.T, reply billingMCPReply) chat.Action {
	t.Helper()
	var value struct {
		Preview     chat.Action `json:"preview"`
		ApprovalURL *string     `json:"approval_url"`
	}
	if reply.Result.IsError || len(reply.Error) > 0 || json.Unmarshal(reply.Result.Structured, &value) != nil || value.Preview.ID == "" {
		t.Fatalf("preview: %+v %s", reply, reply.Result.Structured)
	}
	if value.Preview.Status == chat.ActionPending {
		if value.ApprovalURL == nil || !strings.Contains(*value.ApprovalURL, "connection_id="+value.Preview.ConnectionID) {
			t.Fatalf("pending billing action lacks approval destination: %s", reply.Result.Structured)
		}
	} else if value.ApprovalURL != nil {
		t.Fatalf("resolved billing action advertises approval: %s", reply.Result.Structured)
	}
	return value.Preview
}

func (f *browserHostedFixture) billingDecisionForm(t *testing.T, action chat.Action, decision, mode string) url.Values {
	t.Helper()
	page := f.page(t, "owner", "/chat/approval?connection_id="+action.ConnectionID)
	if page.Code != http.StatusOK {
		t.Fatalf("approval page=%d %s", page.Code, page.Body.String())
	}
	html := page.Body.String()
	if action.ID != "" {
		for _, form := range regexp.MustCompile(`<form[^>]*>[\s\S]*?</form>`).FindAllString(html, -1) {
			if strings.Contains(form, `name="action_id" value="`+action.ID+`"`) {
				html = form
				break
			}
		}
	}
	match := regexp.MustCompile(`name="form_token" value="([^"]+)"`).FindStringSubmatch(html)
	if len(match) != 2 {
		t.Fatalf("missing form token: %s", html)
	}
	return url.Values{"connection_id": {action.ConnectionID}, "action_id": {action.ID}, "form_token": {match[1]}, "decision": {decision}, "mode": {mode}}
}
func (f *browserHostedFixture) billingDecision(t *testing.T, action chat.Action, decision, mode string, forge bool) *httptest.ResponseRecorder {
	form := f.billingDecisionForm(t, action, decision, mode)
	if forge {
		form.Set("form_token", "forged")
	}
	return f.form(t, "owner", "/chat/approval", form)
}

// Catches provider effects before approval, direct-call owner bypass, stale
// configuration/authority, unsafe diagnostics, and duplicate effects on replay.
func TestHostedBillingMCP(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"reads", "approve", "reject", "forged form", "stale configuration", "downgraded owner", "unavailable", "checkout disabled", "YOLO", "YOLO downgraded", "concurrent replay", "changed replay", "portal", "portal response loss", "provider failure", "response loss"} {
		t.Run(scenario, func(t *testing.T) {
			f, provider := newHostedCustomerFixture(t)
			var loss *billingResponseLossProvider
			if scenario == "response loss" {
				loss = &billingResponseLossProvider{hostedCustomerProvider: provider, sessions: make(map[string]billing.Session)}
				f.service.config.Hosted.Billing.Provider = loss
			}
			var logs bytes.Buffer
			f.service.config.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			args := `{"price":"price_fixture","request_id":"purchase"}`
			if scenario == "reads" {
				for _, name := range []string{operatortool.BillingStatus, operatortool.BillingUsage, operatortool.BillingExport, operatortool.HostedPlan, operatortool.HostedUsage} {
					reply := f.billingTool(t, "owner", name, `{}`)
					if reply.Result.IsError || len(reply.Error) > 0 || !strings.Contains(string(reply.Result.Structured), `"generated_at"`) {
						t.Fatalf("%s: %+v", name, reply)
					}
					if name == operatortool.BillingExport && (!strings.Contains(string(reply.Result.Structured), `"export_id"`) || !strings.Contains(string(reply.Result.Structured), `"url"`)) {
						t.Fatal("missing export identity")
					}
				}
				for _, step := range []struct {
					action string
					on     bool
				}{{"", false}, {"grant", true}, {"revoke", false}} {
					before, err := f.service.database.hostedPlanUsage(t.Context(), time.Now())
					if err != nil {
						t.Fatal(err)
					}
					if step.action != "" {
						command := hostedPlanCommand{ID: "model_" + step.action, Action: step.action, ExpectedRevision: before.Revision, GrantID: "model_choice", Plan: before.Base, Scope: []string{"model_choice"}, Reason: "approved model access"}
						if err := f.service.database.applyHostedPlanCommand(t.Context(), "operator", command); err != nil {
							t.Fatal(err)
						}
					}
					api := f.billingAPI(t, "owner", http.MethodGet, "/plan", "")
					requireNativeStatus(t, api, http.StatusOK)
					var plan HostedEntitlement
					if err := json.Unmarshal(api.Body.Bytes(), &plan); err != nil || slices.Contains(plan.Features, "model_choice") != step.on {
						t.Fatalf("%s API plan = %+v (%v)", step.action, plan, err)
					}
					reply := f.billingTool(t, "owner", operatortool.HostedPlan, `{}`)
					var report struct {
						Plan HostedEntitlement `json:"plan"`
					}
					if reply.Result.IsError || len(reply.Error) > 0 || json.Unmarshal(reply.Result.Structured, &report) != nil || slices.Contains(report.Plan.Features, "model_choice") != step.on {
						t.Fatalf("%s MCP plan = %s", step.action, reply.Result.Structured)
					}
				}
				for _, account := range []string{"viewer", "support-viewer", "wrong-organization", "staff"} {
					response := f.billingAPI(t, account, http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+operatortool.BillingStatus+`","arguments":{},"_meta":`+billingMCPMeta+`}}`)
					if response.Code == 200 {
						var reply billingMCPReply
						_ = json.Unmarshal(response.Body.Bytes(), &reply)
						if !reply.Result.IsError && len(reply.Error) == 0 {
							t.Fatalf("%s read owner billing", account)
						}
					}
				}
				if denied := f.billingTool(t, "viewer", operatortool.HostedUsage, `{"project_id":"`+f.privateProject+`"}`); !denied.Result.IsError {
					t.Fatal("usage bypassed current project grants")
				}
				if allowed := f.billingTool(t, "viewer", operatortool.HostedUsage, `{}`); allowed.Result.IsError {
					t.Fatal("viewer could not read granted usage")
				}
				if len(provider.keys) != 0 || len(provider.checkouts) != 0 || len(provider.portals) != 0 {
					t.Fatal("reads created a billing effect")
				}
				return
			}
			if scenario == "unavailable" {
				f.service.config.Hosted.Billing = nil
			}
			if scenario == "checkout disabled" {
				f.service.config.Hosted.Billing.CheckoutDisabled = true
			}
			if scenario == "unavailable" || scenario == "checkout disabled" {
				if reply := f.billingTool(t, "owner", operatortool.BillingCheckout, args); !reply.Result.IsError {
					t.Fatal("unavailable checkout succeeded")
				}
				if len(provider.keys) != 0 {
					t.Fatal("unavailable service created customer")
				}
				return
			}
			if scenario == "YOLO" || scenario == "YOLO downgraded" {
				info := f.billingTool(t, "owner", operatortool.ConnectionInfo, `{}`)
				var connection struct {
					ID string `json:"connection_id"`
				}
				if json.Unmarshal(info.Result.Structured, &connection) != nil {
					t.Fatal("connection info")
				}
				requireNativeStatus(t, f.billingDecision(t, chat.Action{ConnectionID: connection.ID}, "mode", "yolo", false), http.StatusSeeOther)
			}
			if scenario == "YOLO downgraded" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE hosted_members SET role='admin' WHERE user_id='user_browser_owner'"); err != nil {
					t.Fatal(err)
				}
				if reply := f.billingTool(t, "owner", operatortool.BillingCheckout, args); !reply.Result.IsError || len(provider.keys) != 0 {
					t.Fatal("YOLO bypassed current owner authority")
				}
				return
			}
			reply := f.billingTool(t, "owner", operatortool.BillingCheckout, args)
			action := billingPreview(t, reply)
			if scenario == "YOLO" {
				if action.Status != chat.ActionSucceeded || action.IssueID != "cs_test_fixture" || len(provider.checkouts) != 1 {
					t.Fatalf("YOLO=%+v", action)
				}
				return
			}
			if action.Status != chat.ActionPending || len(provider.keys) != 0 || len(provider.checkouts) != 0 || len(provider.portals) != 0 {
				t.Fatal("provider effect before human approval")
			}
			for _, name := range []string{"update_hosted_plan", "hosted_artifact_allowances", "hosted_plan_report", "change_platform_entitlement", "platform_entitlements_json"} {
				if denied := f.billingTool(t, "owner", "billing_usage."+name, `{}`); !denied.Result.IsError && len(denied.Error) == 0 {
					t.Fatal("operator gained platform administration")
				}
			}
			if scenario == "reject" {
				requireNativeStatus(t, f.billingDecision(t, action, "reject", "", false), http.StatusSeeOther)
				if rejected := billingPreview(t, f.billingTool(t, "owner", operatortool.ActionResult, `{"action_id":"`+action.ID+`"}`)); rejected.Status != chat.ActionRejected {
					t.Fatal("rejected action lost its disposition")
				}
				if len(provider.keys) != 0 {
					t.Fatal("rejected checkout created customer")
				}
				return
			}
			if scenario == "forged form" {
				requireNativeStatus(t, f.billingDecision(t, action, "confirm", "", true), http.StatusForbidden)
				if len(provider.keys) != 0 {
					t.Fatal("forged approval created customer")
				}
				return
			}
			if scenario == "stale configuration" {
				f.service.config.Hosted.Billing.Mode = "live"
				requireNativeStatus(t, f.billingDecision(t, action, "confirm", "", false), http.StatusConflict)
				if len(provider.keys) != 0 {
					t.Fatal("stale configuration created customer")
				}
				return
			}
			if scenario == "downgraded owner" {
				form := f.billingDecisionForm(t, action, "confirm", "")
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE hosted_members SET role='admin' WHERE user_id='user_browser_owner'"); err != nil {
					t.Fatal(err)
				}
				requireNativeStatus(t, f.form(t, "owner", "/chat/approval", form), http.StatusForbidden)
				if len(provider.keys) != 0 {
					t.Fatal("downgraded owner created customer")
				}
				return
			}
			if scenario == "provider failure" {
				provider.failures = []error{errors.New("credential-sensitive-value-sentinel whsec_fixture_2196_secret")}
			}
			decision := f.billingDecision(t, action, "confirm", "", false)
			if scenario == "response loss" {
				requireNativeStatus(t, decision, http.StatusConflict)
				var wg sync.WaitGroup
				for range 8 {
					wg.Go(func() {
						replayed := billingPreview(t, f.billingTool(t, "owner", operatortool.BillingCheckout, args))
						if replayed.Status != chat.ActionSucceeded || replayed.IssueID != "cs_test_fixture" {
							t.Errorf("response-loss replay=%+v", replayed)
						}
					})
				}
				wg.Wait()
				if len(provider.checkouts) != 1 || len(loss.keys) != 2 || loss.keys[0] != loss.keys[1] {
					t.Fatal("MCP retry replaced the provider purchase key")
				}
				return
			}
			if scenario == "provider failure" {
				requireNativeStatus(t, decision, http.StatusConflict)
				if failed := billingPreview(t, f.billingTool(t, "owner", operatortool.ActionResult, `{"action_id":"`+action.ID+`"}`)); failed.Status != chat.ActionFailed {
					t.Fatal("failed action lost its disposition")
				}
				if strings.Contains(decision.Body.String(), "credential-sensitive") || strings.Contains(logs.String(), "whsec_") {
					t.Fatal("provider secret leaked")
				}
				return
			}
			requireNativeStatus(t, decision, http.StatusSeeOther)
			completed := billingPreview(t, f.billingTool(t, "owner", operatortool.ActionResult, `{"action_id":"`+action.ID+`"}`))
			if completed.Status != chat.ActionSucceeded || completed.IssueID != "cs_test_fixture" || !strings.HasPrefix(completed.ResourceURL, "https://checkout.stripe.com/") {
				t.Fatalf("completed=%+v", completed)
			}
			if scenario == "portal" || scenario == "portal response loss" {
				portal := billingPreview(t, f.billingTool(t, "owner", operatortool.BillingPortal, `{"request_id":"portal"}`))
				if len(provider.portals) != 0 {
					t.Fatal("portal before approval")
				}
				if scenario == "portal response loss" {
					provider.fail = true
					requireNativeStatus(t, f.billingDecision(t, portal, "confirm", "", false), http.StatusConflict)
					replayed := billingPreview(t, f.billingTool(t, "owner", operatortool.BillingPortal, `{"request_id":"portal"}`))
					if replayed.Status != chat.ActionFailed || len(provider.portals) != 1 {
						t.Fatal("uncertain portal effect was repeated")
					}
					return
				}
				requireNativeStatus(t, f.billingDecision(t, portal, "confirm", "", false), http.StatusSeeOther)
				portal = billingPreview(t, f.billingTool(t, "owner", operatortool.ActionResult, `{"action_id":"`+portal.ID+`"}`))
				if portal.IssueID != "bps_fixture" || !strings.Contains(portal.ResourceURL, "billing.stripe.com") {
					t.Fatalf("portal=%+v", portal)
				}
				replayed := billingPreview(t, f.billingTool(t, "owner", operatortool.BillingPortal, `{"request_id":"portal"}`))
				if replayed.IssueID != portal.IssueID || len(provider.portals) != 1 {
					t.Fatal("portal replay repeated the external effect")
				}
			}
			if scenario == "changed replay" {
				args = `{"price":"changed","request_id":"purchase"}`
				if reply := f.billingTool(t, "owner", operatortool.BillingCheckout, args); !reply.Result.IsError {
					t.Fatal("changed replay accepted")
				}
				return
			}
			if scenario == "concurrent replay" {
				var wg sync.WaitGroup
				for range 8 {
					wg.Go(func() {
						replayed := billingPreview(t, f.billingTool(t, "owner", operatortool.BillingCheckout, args))
						if replayed.IssueID != completed.IssueID {
							t.Error("lost response identity changed")
						}
					})
				}
				wg.Wait()
			}
			if len(provider.checkouts) != 1 || len(provider.keys) != 1 || strings.Contains(logs.String(), "whsec_") {
				t.Fatalf("effects: customer=%d checkout=%d", len(provider.keys), len(provider.checkouts))
			}
		})
	}
}

// Provider response loss is resumable only through the application's existing
// checkout intent/key; concurrent callers must not invent a new purchase.
func TestBillingCommandResponseLoss(t *testing.T) {
	t.Parallel()
	f, provider := newHostedCustomerFixture(t)
	loss := &billingResponseLossProvider{hostedCustomerProvider: provider, sessions: make(map[string]billing.Session)}
	f.service.config.Hosted.Billing.Provider = loss
	var credential apiCredential
	f.service.echo.GET("/billing-command-fixture", func(c echo.Context) error {
		var err error
		credential, err = f.service.hostedBillingOwner(c.Request().Context(), c)
		return err
	})
	requireNativeStatus(t, f.page(t, "owner", "/billing-command-fixture"), http.StatusOK)
	authorize := func(context.Context) (apiCredential, error) { return credential, nil }
	if _, err := f.service.checkoutBilling(t.Context(), authorize, "price_fixture", "lost-response"); err == nil {
		t.Fatal("fixture did not lose response")
	}
	first := loss.keys[0]
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			result, err := f.service.checkoutBilling(t.Context(), authorize, "price_fixture", "lost-response")
			if err != nil || result.ID != "cs_test_fixture" {
				t.Errorf("retry=%+v %v", result, err)
			}
		})
	}
	wg.Wait()
	if len(provider.checkouts) != 1 || len(loss.keys) != 2 || loss.keys[1] != first {
		t.Fatal("response loss replaced provider key")
	}
}

type billingResponseLossProvider struct {
	*hostedCustomerProvider
	sessions map[string]billing.Session
	keys     []string
}

func (p *billingResponseLossProvider) Checkout(ctx context.Context, request billing.CheckoutRequest) (billing.Session, error) {
	p.keys = append(p.keys, request.IdempotencyKey)
	if session, ok := p.sessions[request.IdempotencyKey]; ok {
		return session, nil
	}
	session, err := p.hostedCustomerProvider.Checkout(ctx, request)
	if err != nil {
		return session, err
	}
	p.sessions[request.IdempotencyKey] = session
	return billing.Session{}, errors.New("credential-sensitive-value-sentinel: response lost after provider effect")
}
