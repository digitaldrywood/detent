package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func TestHostedCreditMCP(t *testing.T) {
	t.Parallel()
	for _, name := range []string{operatortool.CreditCheckout, operatortool.CreditAutoFund} {
		for _, scenario := range []string{"approved replay", "durable replay", "denied", "reject", "forged", "stale pack", "stale settings", "stale payment method", "downgraded owner", "unavailable", "no credit provider", "no credit mode", "billing disabled", "checkout paused", "paused disable", "changed replay", "provider failure", "response loss", "YOLO", "disable", "invalid input"} {
			if name == operatortool.CreditCheckout && (scenario == "disable" || scenario == "paused disable" || scenario == "stale settings" || scenario == "stale payment method" || scenario == "durable replay") || name == operatortool.CreditAutoFund && scenario == "response loss" {
				continue
			}
			t.Run(name+"/"+scenario, func(t *testing.T) {
				f, base := newHostedBillingFixture(t)
				provider := configureTestCredits(t, f, base)
				provider.savedMethod = "pm_sensitive_secret"
				var logs bytes.Buffer
				f.service.config.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
				args := `{"price":"price_credit","request_id":"credits"}`
				if name == operatortool.CreditAutoFund {
					args = `{"enabled":true,"threshold_cents":100,"price":"price_credit","request_id":"credits"}`
				}
				if scenario == "disable" || scenario == "paused disable" {
					args = `{"enabled":false,"threshold_cents":0,"price":"","request_id":"credits"}`
				}
				if scenario == "denied" {
					for _, account := range []string{"viewer", "support-viewer", "wrong-organization", "staff"} {
						response := f.billingAPI(t, account, http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`,"_meta":`+billingMCPMeta+`}}`)
						var reply billingMCPReply
						if response.Code == http.StatusOK && json.Unmarshal(response.Body.Bytes(), &reply) == nil && !reply.Result.IsError && len(reply.Error) == 0 {
							t.Fatalf("%s executed owner command", account)
						}
					}
					if len(provider.creditCheckouts) != 0 || len(provider.chargeKeys) != 0 {
						t.Fatal("denied authority reached provider")
					}
					return
				}
				if scenario == "unavailable" {
					f.service.billing = nil
				}
				if scenario == "no credit provider" {
					f.service.config.Hosted.Billing.Provider = base
				}
				if scenario == "no credit mode" {
					f.service.database.aiCreditMode = ""
				}
				if scenario == "billing disabled" {
					f.service.config.Hosted.Billing = nil
				}
				if scenario == "checkout paused" || scenario == "paused disable" {
					f.service.config.Hosted.Billing.CheckoutDisabled = true
				}
				if scenario == "checkout paused" && name == operatortool.CreditAutoFund {
					if reply := f.billingTool(t, "owner", name, args); !reply.Result.IsError {
						t.Fatal("paused packs accepted for enable")
					}
					return
				}
				if scenario == "unavailable" || scenario == "no credit provider" || scenario == "no credit mode" || scenario == "billing disabled" || scenario == "checkout paused" {
					response := f.billingAPI(t, "owner", http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":`+billingMCPMeta+`}}`)
					if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"tools":`) {
						t.Fatalf("catalog=%d %s", response.Code, response.Body.String())
					}
					if strings.Contains(response.Body.String(), name) {
						t.Fatal("unavailable capability advertised")
					}
					if reply := f.billingTool(t, "owner", name, args); !reply.Result.IsError {
						t.Fatal("unavailable capability executed")
					}
					return
				}
				if scenario == "approved replay" {
					response := f.billingAPI(t, "owner", http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":`+billingMCPMeta+`}}`)
					if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"`+name+`"`) {
						t.Fatalf("installed catalog=%d %s", response.Code, response.Body.String())
					}
				}
				if scenario == "invalid input" {
					invalid := []string{strings.Replace(args, "price_credit", "price_unapproved", 1), strings.Replace(args, `"request_id":"credits"`, `"request_id":"credits","organization_id":"other"`, 1), strings.Replace(args, `"request_id":"credits"`, `"request_id":"credits","url":"https://attacker.test"`, 1)}
					if name == operatortool.CreditAutoFund {
						invalid = append(invalid, strings.Replace(args, "100", "500", 1), strings.Replace(args, "100", "0", 1), strings.Replace(args, "100", "1.5", 1), strings.Replace(args, `"enabled":true,`, "", 1))
					}
					for _, input := range invalid {
						if reply := f.billingTool(t, "owner", name, input); !reply.Result.IsError {
							t.Fatalf("accepted %s", input)
						}
					}
					return
				}
				if scenario == "YOLO" {
					info := f.billingTool(t, "owner", operatortool.ConnectionInfo, `{}`)
					var connection struct {
						ID string `json:"connection_id"`
					}
					if err := json.Unmarshal(info.Result.Structured, &connection); err != nil {
						t.Fatal(err)
					}
					requireNativeStatus(t, f.billingDecision(t, chat.Action{ConnectionID: connection.ID}, "mode", "yolo", false), http.StatusSeeOther)
				}
				action := billingPreview(t, f.billingTool(t, "owner", name, args))
				if scenario != "disable" && scenario != "paused disable" && !strings.Contains(action.Description, `"usd_cents":500`) {
					t.Fatalf("preview lacks configured amount: %s", action.Description)
				}
				if scenario != "YOLO" && scenario != "disable" && scenario != "paused disable" {
					if action.Status != chat.ActionPending || len(provider.creditCheckouts) != 0 {
						t.Fatal("financial effect before approval")
					}
					view, err := f.service.readAICredits(t.Context())
					if err != nil || view.AutoEnabled {
						t.Fatalf("funding before approval: %+v %v", view, err)
					}
					if scenario == "reject" {
						requireNativeStatus(t, f.billingDecision(t, action, "reject", "", false), http.StatusSeeOther)
						if receipt := billingPreview(t, f.billingTool(t, "owner", name, args)); receipt.Status != chat.ActionRejected {
							t.Fatal("lost rejection receipt")
						}
						return
					}
					if scenario == "forged" {
						requireNativeStatus(t, f.billingDecision(t, action, "confirm", "", true), http.StatusForbidden)
						if len(provider.creditCheckouts) != 0 {
							t.Fatal("forged approval created purchase")
						}
						return
					}
					if scenario == "downgraded owner" {
						form := f.billingDecisionForm(t, action, "confirm", "")
						if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE hosted_members SET role='admin' WHERE user_id='user_browser_owner'"); err != nil {
							t.Fatal(err)
						}
						requireNativeStatus(t, f.form(t, "owner", "/chat/approval", form), http.StatusForbidden)
						if reply := f.billingTool(t, "owner", name, args); !reply.Result.IsError {
							t.Fatal("downgraded owner replayed")
						}
						return
					}
					if scenario == "stale pack" {
						f.service.config.Hosted.Billing.CreditPacks[0].USDCents++
					}
					if scenario == "stale payment method" {
						provider.savedMethod = "pm_changed"
					}
					if scenario == "stale settings" {
						if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE ai_credit_accounts SET threshold_cents=50"); err != nil {
							t.Fatal(err)
						}
					}
					if strings.HasPrefix(scenario, "stale ") {
						requireNativeStatus(t, f.billingDecision(t, action, "confirm", "", false), http.StatusConflict)
						if len(provider.creditCheckouts) != 0 {
							t.Fatal("stale approval created purchase")
						}
						view, err := f.service.readAICredits(t.Context())
						if err != nil || view.AutoEnabled {
							t.Fatal("stale approval enabled funding")
						}
						return
					}
					if scenario == "provider failure" {
						if name == operatortool.CreditCheckout {
							provider.checkoutError = errors.New("pm_sensitive_secret whsec_fixture_2196_secret")
						} else {
							provider.savedError = errors.New("pm_sensitive_secret whsec_fixture_2196_secret")
						}
					}
					if scenario == "response loss" {
						provider.checkoutLoss = true
					}
					decision := f.billingDecision(t, action, "confirm", "", false)
					if scenario == "provider failure" || scenario == "response loss" {
						requireNativeStatus(t, decision, http.StatusConflict)
						failed := billingPreview(t, f.billingTool(t, "owner", operatortool.ActionResult, `{"action_id":"`+action.ID+`"}`))
						if failed.Status != chat.ActionFailed || strings.Contains(failed.Result+logs.String(), "pm_sensitive_secret") {
							t.Fatal("failure receipt leaked or lost status")
						}
						if scenario == "provider failure" {
							return
						}
					} else {
						requireNativeStatus(t, decision, http.StatusSeeOther)
					}
				}
				var wg sync.WaitGroup
				for range 4 {
					wg.Go(func() {
						replay := billingPreview(t, f.billingTool(t, "owner", name, args))
						if replay.ID != action.ID || replay.Status != chat.ActionSucceeded {
							t.Errorf("replay=%+v", replay)
						}
					})
				}
				wg.Wait()
				receiptReply := f.billingTool(t, "owner", operatortool.ActionResult, `{"action_id":"`+action.ID+`"}`)
				receipt := billingPreview(t, receiptReply)
				if receipt.ResolvedAt == nil {
					t.Fatal("terminal receipt lacks resolution time")
				}
				if strings.Contains(receipt.Description+receipt.Result+logs.String(), "pm_sensitive_secret") {
					t.Fatal("receipt leaked payment credentials")
				}
				if name == operatortool.CreditCheckout {
					wantCalls := 1
					if scenario == "response loss" {
						wantCalls = 2
					}
					if len(provider.creditCheckouts) != wantCalls || len(provider.creditSessions) != 1 || receipt.IssueID != "cs_test_credit" || !strings.HasPrefix(receipt.ResourceURL, "https://checkout.stripe.com/") {
						t.Fatalf("purchase receipt=%+v calls=%d", receipt, len(provider.creditCheckouts))
					}
					if wantCalls == 2 && provider.creditCheckouts[0].IdempotencyKey != provider.creditCheckouts[1].IdempotencyKey {
						t.Fatal("lost response changed purchase key")
					}
				} else {
					var result struct {
						Settings *creditFundingInput `json:"settings"`
					}
					if err := json.Unmarshal(receiptReply.Result.Structured, &result); err != nil || result.Settings == nil || result.Settings.Enabled != (scenario != "disable" && scenario != "paused disable") {
						t.Fatalf("typed settings receipt=%s err=%v", receiptReply.Result.Structured, err)
					}
					view, err := f.service.readAICredits(t.Context())
					if err != nil || view.AutoEnabled != (scenario != "disable" && scenario != "paused disable") || receipt.IssueID != f.service.config.Hosted.OrganizationID {
						t.Fatalf("funding receipt=%+v settings=%+v err=%v", receipt, view, err)
					}
					if len(provider.chargeKeys) != 0 {
						t.Fatal("settings command charged customer")
					}
					var durable string
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT response_json FROM native_commands WHERE operation='billing.credit_auto_fund'").Scan(&durable); err != nil {
						t.Fatal(err)
					}
					if strings.Contains(durable, "pm_sensitive_secret") || !strings.Contains(durable, `"threshold_cents":`) {
						t.Fatal("unsafe durable settings receipt")
					}

					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE ai_credit_accounts SET auto_enabled=0,threshold_cents=20"); err != nil {
						t.Fatal(err)
					}
					billingPreview(t, f.billingTool(t, "owner", name, args))
					if scenario == "durable replay" {
						stored, ok := f.service.operatorChat.Action(action.ConnectionID, action.ID)
						if !ok {
							t.Fatal("missing action authority")
						}
						f.service.echo.GET("/credit-replay-fixture", func(c echo.Context) error {
							authorize := func(ctx context.Context) (apiCredential, error) { return f.service.hostedBillingOwner(ctx, c) }
							ctx := mutation.WithContext(c.Request().Context(), stored.Mutation)
							settings, err := f.service.configureCreditFunding(ctx, authorize, creditFundingInput{Enabled: true, Threshold: 100, Price: "price_credit"}, "credits")
							if err != nil {
								return err
							}
							if !settings.Enabled || settings.Threshold != 100 {
								t.Errorf("durable receipt=%+v", settings)
							}
							return c.NoContent(http.StatusOK)
						})
						requireNativeStatus(t, f.page(t, "owner", "/credit-replay-fixture"), http.StatusOK)
					}
					view, err = f.service.readAICredits(t.Context())
					if err != nil || view.AutoEnabled || view.ThresholdCents != 20 {
						t.Fatal("replay overwrote newer settings")
					}
				}
				if scenario == "changed replay" {
					changed := strings.Replace(args, "price_credit", "price_other", 1)
					if reply := f.billingTool(t, "owner", name, changed); !reply.Result.IsError {
						t.Fatal("changed replay accepted")
					}
				}
			})
		}
	}
}
