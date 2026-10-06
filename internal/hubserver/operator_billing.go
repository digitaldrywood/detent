package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

var errBillingUnavailable = errors.New("billing operation is unavailable")

type operatorCredentialKey struct{}
type hostedOperatorExecutor struct{ service *Service }

func (s *Service) operatorDashboardURL() string {
	if s.config.Hosted == nil {
		return ""
	}
	return strings.TrimRight(s.config.Hosted.PublicURL, "/")
}

func (s *Service) operatorBillingCredential(ctx context.Context, kind string, write bool) (apiCredential, error) {
	scope := apikey.ScopeRead
	if write {
		scope = apikey.ScopeWrite
	}
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: scope, ResourceKind: kind}); err != nil {
		return apiCredential{}, err
	}
	resolve, ok := ctx.Value(operatorCredentialKey{}).(billingAuthorization)
	if !ok || s.config.Hosted == nil {
		return apiCredential{}, operatortool.ErrAccessDenied
	}
	credential, err := resolve(ctx)
	if err != nil || operatorIdentity(credential, s.config.Hosted.OrganizationID) != operatortool.ConnectionIdentity(ctx) {
		return apiCredential{}, operatortool.ErrAccessDenied
	}
	// The resolver and the application's role predicates are evaluated together;
	// generic organization write authority never implies owner billing authority.
	if credential.Hosted == nil || kind == "billing" && (credential.HostedRole != "owner" || credential.Hosted.SupportActor != "") || kind == "plan" && credential.HostedRole != "owner" && credential.HostedRole != "admin" {
		return apiCredential{}, operatortool.ErrAccessDenied
	}
	return credential, nil
}

func (e hostedOperatorExecutor) OpenConnection(ctx context.Context) error {
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
		return err
	}
	return e.service.operatorChat.AttachConnection(ctx)
}

func (e hostedOperatorExecutor) ListTools(ctx context.Context) ([]operatortool.Definition, error) {
	definitions, err := nativeOperatorExecutor(e).ListTools(ctx)
	if err != nil {
		return nil, err
	}
	projectDefinitions, err := hubProjectExecutor(e).ListTools(ctx)
	if err != nil {
		return nil, err
	}
	for _, definition := range projectDefinitions {
		if definition.Meta.Toolset == "projects" || definition.Meta.Toolset == "local_projects" {
			definitions = append(definitions, definition)
		}
	}
	workspaceTools, err := (workspaceOperatorExecutor{server: e.service}).ListTools(ctx)
	if err != nil {
		return nil, err
	}
	for _, definition := range workspaceTools {
		if definition.Name != operatortool.ActionResult && definition.Name != operatortool.ConnectionInfo {
			definitions = append(definitions, definition)
		}
	}
	changes, err := hubOperatorExecutor(e).ListTools(ctx)
	if err != nil {
		return nil, err
	}
	definitions = append(definitions, changes...)
	admin, err := e.service.administrationCatalog(ctx)
	if err != nil {
		return nil, err
	}
	for _, definition := range admin {
		if operatortool.IsAdministration(definition.Name) || e.service.config.Hosted == nil {
			definitions = append(definitions, definition)
		}
	}
	fleet, err := hubFleetExecutor(e).ListTools(ctx)
	if err != nil {
		return nil, err
	}
	for _, definition := range fleet {
		if definition.Name != operatortool.ActionResult && definition.Name != operatortool.ConnectionInfo {
			definitions = append(definitions, definition)
		}
	}
	if e.service.config.Hosted == nil {
		return definitions, nil
	}
	definitions = append(definitions, (hostedContextExecutor(e)).listTools(ctx)...)
	for _, definition := range operatortool.BillingCatalog() {
		kind := "billing"
		switch definition.Name {
		case operatortool.UsageReport, operatortool.IssueExplanation, operatortool.BudgetOverrideSet, operatortool.BudgetOverrideClear:
			continue
		case operatortool.HostedPlan:
			kind = "plan"
		case operatortool.HostedUsage:
			kind = ""
		}
		if _, err := e.service.catalogBillingCredential(ctx, kind, !definition.Annotations.ReadOnly); err != nil {
			continue
		}
		if (definition.Name == operatortool.CreditCheckout || definition.Name == operatortool.CreditAutoFund) && !e.service.creditAvailable() {
			continue
		}
		if definition.Name == operatortool.CreditCheckout && e.service.config.Hosted.Billing.CheckoutDisabled {
			continue
		}
		if !definition.Annotations.ReadOnly && e.service.config.Hosted.Billing == nil {
			continue
		}
		definitions = append(definitions, definition)
	}
	for _, name := range []string{operatortool.ActionResult, operatortool.ConnectionInfo} {
		definition, _ := operatortool.Lookup(name)
		definitions = append(definitions, definition)
	}
	return definitions, nil
}

func billingResult(value any) (operatortool.Result, error) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > operatortool.MaxResultBytes {
		return operatortool.Result{}, errBillingUnavailable
	}
	return operatortool.Result{Content: raw}, nil
}

func safeBillingError(err error) error {
	for _, safe := range []error{operatortool.ErrAccessDenied, operatortool.ErrInvalidArguments, mutation.ErrConflict, mutation.ErrUncertain} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return errBillingUnavailable
}

func (e hostedOperatorExecutor) Execute(ctx context.Context, call operatortool.Call) (result operatortool.Result, err error) {
	if call.Name == operatortool.BoardState || call.Name == operatortool.Dashboard {
		return nativeOperatorExecutor(e).Execute(ctx, call)
	}
	if operatortool.IsHostedContext(call.Name) {
		return (hostedContextExecutor(e)).Execute(ctx, call)
	}
	if operatortool.IsLocalProjectTool(call.Name) {
		return hubProjectExecutor(e).Execute(ctx, call)
	}
	if _, err := operatortool.WorkspaceDefinition(call.Name); err == nil {
		return (workspaceOperatorExecutor{server: e.service}).Execute(ctx, call)
	}
	if definition, ok := operatortool.Lookup(call.Name); ok && definition.Meta.Toolset == "projects" {
		return hubProjectExecutor(e).Execute(ctx, call)
	}
	if _, ok := operatortool.ChangeDefinition(call.Name); ok {
		return hubOperatorExecutor(e).Execute(ctx, call)
	}
	if operatortool.IsAdministration(call.Name) {
		return e.service.administration.Execute(ctx, call)
	}
	if call.Name == operatortool.ActionResult {
		var request struct {
			ActionID string `json:"action_id"`
		}
		if operatortool.DecodeArguments(call.Arguments, &request) == nil {
			if action, ok := e.service.operatorChat.Action(operatortool.CurrentConnection(ctx).ID, request.ActionID); ok {
				if _, err := operatortool.WorkspaceDefinition(string(action.Kind)); err == nil {
					return (workspaceOperatorExecutor{server: e.service}).Execute(ctx, call)
				}
				if definition, known := operatortool.Lookup(string(action.Kind)); known && definition.Meta.Toolset == "projects" {
					return hubProjectExecutor(e).connectionRead(ctx, call)
				}
			}
			if action, ok := e.service.operatorChat.Action(operatortool.CurrentConnection(ctx).ID, request.ActionID); ok && operatortool.IsAdministration(string(action.Kind)) {
				return e.service.administration.Execute(ctx, call)
			}
		}
	}
	if hubFleetTool(call.Name) {
		return hubFleetExecutor(e).Execute(ctx, call)
	}
	if call.Name == operatortool.MoveItem || operatortool.IsAttachmentTool(call.Name) || call.Name == operatortool.FileIssue || operatortool.IsWorkTool(call.Name) || operatortool.IsWorkRead(call.Name) || call.Name == operatortool.ExplainItem {
		return nativeOperatorExecutor(e).Execute(ctx, call)
	}
	defer func() {
		if err != nil {
			err = safeBillingError(err)
		}
	}()
	s := e.service
	switch call.Name {
	case operatortool.BillingCheckout, operatortool.BillingPortal, operatortool.CreditCheckout, operatortool.CreditAutoFund:
		return e.submitBilling(ctx, call)
	case operatortool.ActionResult:
		var request struct {
			ActionID string `json:"action_id"`
		}
		if operatortool.DecodeArguments(call.Arguments, &request) != nil || request.ActionID == "" || len(request.ActionID) > 256 {
			return result, operatortool.ErrInvalidArguments
		}
		if err := s.operatorChat.CheckConnection(ctx); err != nil {
			return result, err
		}
		action, ok := s.operatorChat.Action(operatortool.CurrentConnection(ctx).ID, request.ActionID)
		if !ok {
			return result, errBillingUnavailable
		}
		if _, ok := operatortool.ChangeDefinition(string(action.Kind)); ok {
			return (hubOperatorExecutor{service: s}).Execute(ctx, call)
		}
		if hubFleetTool(string(action.Kind)) {
			if _, err := operatortool.AuthorizeCurrent(ctx, hubFleetRequirement(string(action.Kind))); err != nil {
				return result, err
			}
			return (hubFleetExecutor{service: s}).actionResult(ctx, action)
		}
		if action.Kind == chat.ActionMoveItem {
			if _, _, err := (nativeOperatorExecutor{service: s}).workflowAuthority(ctx, action.Arguments); err != nil {
				return result, err
			}
			return s.hubActionResult(action)
		}
		if action.Kind == chat.ActionKind(operatortool.DeleteAttachment) {
			return (nativeOperatorExecutor{service: s}).attachmentActionResult(ctx, action)
		}
		if _, err := s.operatorBillingCredential(ctx, "billing", true); err != nil {
			return result, err
		}
		return s.billingActionResult(action)
	case operatortool.ConnectionInfo:
		if err := operatortool.DecodeArguments(call.Arguments, &struct{}{}); err != nil {
			return result, err
		}
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
			return result, err
		}
		if err := s.operatorChat.CheckConnection(ctx); err != nil {
			return result, err
		}
		conversation := s.operatorChat.Conversation(operatortool.CurrentConnection(ctx).ID)
		return billingResult(struct {
			ConnectionID   string `json:"connection_id"`
			OrganizationID string `json:"organization_id"`
		}{conversation.ConnectionID, conversation.OrganizationID})
	case operatortool.BillingStatus, operatortool.BillingUsage, operatortool.BillingExport, operatortool.HostedPlan, operatortool.HostedUsage:
		kind := "billing"
		var request struct {
			ProjectID string `json:"project_id"`
			Range     string `json:"range"`
		}
		if call.Name == operatortool.HostedUsage {
			kind = ""
			if operatortool.DecodeArguments(call.Arguments, &request) != nil || len(request.ProjectID) > 256 {
				return result, operatortool.ErrInvalidArguments
			}
		} else if operatortool.DecodeArguments(call.Arguments, &struct{}{}) != nil {
			return result, operatortool.ErrInvalidArguments
		}
		if call.Name == operatortool.HostedPlan {
			kind = "plan"
		}
		credential, err := s.operatorBillingCredential(ctx, kind, false)
		if err != nil {
			return result, err
		}
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: request.ProjectID, ResourceKind: kind}); err != nil {
			return result, err
		}
		now := s.config.now().UTC()
		switch call.Name {
		case operatortool.HostedUsage:
			data, err := s.readHostedUsage(ctx, credential, request.Range, request.ProjectID)
			if err != nil {
				return result, err
			}
			if data.MonthlyCosts != nil {
				bounded := boundedMonthlyCostDetails(*data.MonthlyCosts)
				data.MonthlyCosts = &bounded
			}
			return billingResult(struct {
				GeneratedAt time.Time   `json:"generated_at"`
				Report      usageReport `json:"report"`
			}{now, data})
		case operatortool.HostedPlan:
			data, err := s.database.hostedPlanUsage(ctx, now)
			if err != nil {
				return result, err
			}
			return billingResult(struct {
				GeneratedAt time.Time         `json:"generated_at"`
				Plan        HostedEntitlement `json:"plan"`
			}{now, data})
		case operatortool.BillingUsage:
			metadata, err := s.database.hostedMetadata(ctx)
			if err != nil {
				return result, err
			}
			data, err := s.hostedUsage(ctx, metadata)
			if err != nil {
				return result, err
			}
			return billingResult(struct {
				GeneratedAt time.Time         `json:"generated_at"`
				Usage       hostedUsageReport `json:"usage"`
			}{now, data})
		case operatortool.BillingExport:
			data, err := s.hostedBillingReport(ctx)
			if err != nil {
				return result, err
			}
			raw, err := json.Marshal(data)
			if err != nil {
				return result, err
			}
			digest := sha256.Sum256(raw)
			if err := s.hostedAudit(ctx, credential.Hosted, "billing_exported", "mcp.billing_export", "", 200); err != nil {
				return result, err
			}
			return billingResult(struct {
				ID          string              `json:"export_id"`
				URL         string              `json:"url"`
				GeneratedAt time.Time           `json:"generated_at"`
				Report      hostedBillingReport `json:"report"`
			}{hex.EncodeToString(digest[:]), s.operatorDashboardURL() + s.hostedPath("/api/cloud/billing/subscription"), now, data})
		default:
			data, err := s.readBillingView(ctx)
			if err != nil {
				return result, err
			}
			return billingResult(struct {
				GeneratedAt time.Time         `json:"generated_at"`
				Billing     hostedBillingView `json:"billing"`
			}{now, data})
		}
	default:
		return operatortool.NewAuthorizedExecutor(nil).Execute(ctx, call)
	}
}

type billingActionRequest struct {
	RequestID string `json:"request_id"`
	Price     string `json:"price,omitempty"`
	Enabled   *bool  `json:"enabled,omitempty"`
	Threshold *int64 `json:"threshold_cents,omitempty"`
}

func decodeBillingAction(name string, raw json.RawMessage) (billingActionRequest, error) {
	var request billingActionRequest
	switch name {
	case operatortool.BillingCheckout, operatortool.CreditCheckout:
		var input struct {
			RequestID string `json:"request_id"`
			Price     string `json:"price"`
		}
		if operatortool.DecodeArguments(raw, &input) != nil || input.Price == "" {
			return request, operatortool.ErrInvalidArguments
		}
		request.RequestID, request.Price = input.RequestID, input.Price
	case operatortool.BillingPortal:
		var input struct {
			RequestID string `json:"request_id"`
		}
		if operatortool.DecodeArguments(raw, &input) != nil {
			return request, operatortool.ErrInvalidArguments
		}
		request.RequestID = input.RequestID
	case operatortool.CreditAutoFund:
		var input struct {
			RequestID string  `json:"request_id"`
			Price     *string `json:"price"`
			Enabled   *bool   `json:"enabled"`
			Threshold *int64  `json:"threshold_cents"`
		}
		if operatortool.DecodeArguments(raw, &input) != nil || input.Price == nil || input.Enabled == nil || input.Threshold == nil || *input.Threshold < 0 {
			return request, operatortool.ErrInvalidArguments
		}
		request = billingActionRequest{RequestID: input.RequestID, Price: *input.Price, Enabled: input.Enabled, Threshold: input.Threshold}
	default:
		return request, operatortool.ErrInvalidArguments
	}
	if request.RequestID == "" || len(request.RequestID) > 128 || len(request.Price) > 256 {
		return request, operatortool.ErrInvalidArguments
	}
	return request, nil
}

func (e hostedOperatorExecutor) submitBilling(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	s := e.service
	request, err := decodeBillingAction(call.Name, call.Arguments)
	if err != nil {
		return operatortool.Result{}, err
	}
	credential, err := s.operatorBillingCredential(ctx, "billing", true)
	if err != nil {
		return operatortool.Result{}, err
	}
	if s.config.Hosted.Billing == nil || s.billing == nil {
		return operatortool.Result{}, errBillingUnavailable
	}
	arguments, err := json.Marshal(request)
	if call.Name == operatortool.CreditAutoFund {
		arguments, err = json.Marshal(struct {
			RequestID string `json:"request_id"`
			creditFundingInput
		}{request.RequestID, creditFundingInput{Enabled: *request.Enabled, Threshold: *request.Threshold, Price: request.Price}})
	}

	if err != nil {
		return operatortool.Result{}, err
	}
	if previous, found, err := s.operatorChat.RetryResult(ctx, chat.ActionKind(call.Name), request.RequestID, arguments); found || err != nil {
		if err != nil {
			if errors.Is(err, operatortool.ErrInvalidArguments) {
				return operatortool.Result{}, mutation.ErrConflict
			}
			return operatortool.Result{}, err
		}
		return s.billingActionResult(previous)
	}
	if call.Name == operatortool.BillingCheckout {
		approved := false
		for _, price := range s.config.Hosted.Billing.Prices {
			approved = approved || price.PriceID == request.Price
		}
		if !approved {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		if s.config.Hosted.Billing.CheckoutDisabled {
			return operatortool.Result{}, errBillingUnavailable
		}
	}
	if call.Name == operatortool.CreditCheckout || call.Name == operatortool.CreditAutoFund {
		if !s.creditAvailable() || call.Name == operatortool.CreditCheckout && s.config.Hosted.Billing.CheckoutDisabled {
			return operatortool.Result{}, errBillingUnavailable
		}
		if call.Name == operatortool.CreditCheckout || *request.Enabled {
			pack, ok := s.creditPack(request.Price)
			if !ok || call.Name == operatortool.CreditAutoFund && (*request.Threshold <= 0 || *request.Threshold >= pack.USDCents) {
				return operatortool.Result{}, operatortool.ErrInvalidArguments
			}
		}
	}
	binding, description, err := s.billingRequestBinding(ctx, call.Name, request)
	if err != nil {
		return operatortool.Result{}, err
	}
	identity := operatortool.ConnectionIdentity(ctx)
	m := mutation.Metadata{PrincipalID: identity.PrincipalID, OrganizationID: identity.OrganizationID, Action: call.Name, Source: "mcp", CorrelationID: s.config.newLeaseID()}
	m, err = m.Bind(request.RequestID, json.RawMessage(arguments))
	if err != nil {
		return operatortool.Result{}, err
	}
	action := chat.Action{Kind: chat.ActionKind(call.Name), RequestID: request.RequestID, Arguments: arguments, Mutation: m, Title: call.Name, Identifier: identity.OrganizationID, CurrentState: binding, Description: description}
	action, err = s.operatorChat.Submit(ctx, action)
	if err != nil {
		return operatortool.Result{}, err
	}
	m = action.Mutation
	if err := s.hostedAudit(mutation.WithContext(ctx, m), credential.Hosted, string(action.Status), call.Name, "", 200); err != nil {
		return operatortool.Result{}, err
	}
	return s.billingActionResult(action)
}

// Bind the preview to the configured purchase, not just a mutable price label.
func (s *Service) billingActionBinding(price string) string {
	cfg := s.config.Hosted.Billing
	if cfg == nil {
		return ""
	}
	raw, _ := json.Marshal(struct { //nolint:errcheck // The binding contains only strings and concrete plan references.
		Account, Mode, Portal, Return string
		Prices                        []HostedBillingPrice
		Price                         string
	}{cfg.AccountID, cfg.mode(), cfg.PortalConfigurationID, s.hostedBillingReturn(true), cfg.Prices, price})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *Service) billingRequestBinding(ctx context.Context, name string, request billingActionRequest) (string, string, error) {
	if name != operatortool.CreditCheckout && name != operatortool.CreditAutoFund {
		return s.billingActionBinding(request.Price), "", nil
	}
	if !s.creditAvailable() {
		return "", "", errBillingUnavailable
	}
	cfg := s.config.Hosted.Billing
	pack, _ := s.creditPack(request.Price)
	material := struct {
		Organization string           `json:"organization_id"`
		Pack         HostedCreditPack `json:"credit_pack"`
		Enabled      *bool            `json:"enabled,omitempty"`
		Threshold    *int64           `json:"threshold_cents,omitempty"`
	}{s.config.Hosted.OrganizationID, pack, request.Enabled, request.Threshold}
	preview, err := json.Marshal(material)
	if err != nil {
		return "", "", err
	}
	state := struct {
		Account, Mode, Return, Customer, Method, SavedMethod, Price string
		Disabled, Enabled                                           bool
		Threshold                                                   int64
		Pack                                                        HostedCreditPack
	}{Account: cfg.AccountID, Mode: cfg.mode(), Return: s.hostedBillingReturn(true), Customer: cfg.CustomerID, Disabled: cfg.CheckoutDisabled, Pack: pack}
	if name == operatortool.CreditAutoFund {
		if err := s.database.db.QueryRowContext(ctx, "SELECT auto_enabled,threshold_cents,price_id,payment_method FROM ai_credit_accounts WHERE organization_id=? AND mode=?", s.database.hostedOrganization, s.database.aiCreditMode).Scan(&state.Enabled, &state.Threshold, &state.Price, &state.Method); err != nil {
			return "", "", err
		}
		if *request.Enabled {
			binding, err := s.database.hostedBillingBinding(ctx, cfg)
			if err != nil {
				return "", "", errBillingUnavailable
			}
			state.Customer = binding.CustomerID
			provider, available := cfg.Provider.(billing.CreditProvider)
			if !available {
				return "", "", errBillingUnavailable
			}
			state.SavedMethod, err = provider.SavedCreditPaymentMethod(ctx, binding)
			if err != nil && !errors.Is(err, billing.ErrPaymentFailed) {
				return "", "", errBillingUnavailable
			}
			if state.Method == "" && state.SavedMethod == "" {
				return "", "", operatortool.ErrInvalidArguments
			}
		}
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), string(preview), nil
}

func (e hostedOperatorExecutor) ExecuteAction(ctx context.Context, action chat.Action) (execution chat.ActionExecution, err error) {
	if action.Kind == chat.ActionKind(operatortool.DeleteAttachment) {
		return (nativeOperatorExecutor(e)).executeAttachmentDeletion(ctx, action)
	}
	if action.Mutation.Source == "chat" {
		return e.service.executeCoordinatorAction(ctx, action)
	}
	if action.Kind == chat.ActionMoveItem {
		return (nativeOperatorExecutor(e)).executeWorkflowTransition(ctx, action)
	}
	if _, err := operatortool.WorkspaceDefinition(string(action.Kind)); err == nil {
		return (workspaceOperatorExecutor{server: e.service}).ExecuteAction(ctx, action)
	}
	if definition, ok := operatortool.Lookup(string(action.Kind)); ok && definition.Meta.Toolset == "projects" {
		return hubProjectExecutor(e).ExecuteAction(ctx, action)
	}
	if _, ok := operatortool.ChangeDefinition(string(action.Kind)); ok {
		return hubOperatorExecutor(e).ExecuteAction(ctx, action)
	}
	if operatortool.IsAdministration(string(action.Kind)) {
		return e.service.administration.ExecuteAction(ctx, action)
	}
	if hubFleetTool(string(action.Kind)) {
		return hubFleetExecutor(e).ExecuteAction(ctx, action)
	}
	defer func() {
		if err != nil {
			err = safeBillingError(err)
		}
	}()
	s := e.service
	request, err := decodeBillingAction(string(action.Kind), action.Arguments)
	if err != nil {
		return execution, operatortool.ErrInvalidArguments
	}
	identity := operatortool.ConnectionIdentity(ctx)
	m := action.Mutation
	bound, err := m.Bind(action.RequestID, action.Arguments)
	if err != nil || request.RequestID != action.RequestID || m.Source != "mcp" || m.PrincipalID != identity.PrincipalID || m.OrganizationID != identity.OrganizationID || m.Action != string(action.Kind) || m.CorrelationID == "" || bound.RetryIdentity != m.RetryIdentity || bound.InputHash != m.InputHash && (action.Kind != chat.ActionKind(operatortool.CreditAutoFund) || *request.Enabled || m.Confirmation != "none") {
		return execution, operatortool.ErrAccessDenied
	}
	ctx = mutation.WithContext(ctx, m)
	authorize := func(ctx context.Context) (apiCredential, error) {
		credential, err := s.operatorBillingCredential(ctx, "billing", true)
		if err == nil {
			current, _, bindingErr := s.billingRequestBinding(ctx, string(action.Kind), request)
			if bindingErr != nil {
				err = bindingErr
			} else if action.CurrentState != current {
				err = mutation.ErrConflict
			}
		}
		return credential, err
	}
	var result billingDestination
	if action.Kind == chat.ActionKind(operatortool.BillingCheckout) {
		result, err = s.checkoutBilling(ctx, authorize, request.Price, request.RequestID)
	} else if action.Kind == chat.ActionKind(operatortool.CreditCheckout) {
		result, err = s.checkoutCredits(ctx, authorize, request.Price, request.RequestID)
	} else if action.Kind == chat.ActionKind(operatortool.CreditAutoFund) {
		settings, err := s.configureCreditFunding(ctx, authorize, creditFundingInput{Enabled: *request.Enabled, Threshold: *request.Threshold, Price: request.Price}, request.RequestID)
		if err != nil {
			return execution, err
		}
		raw, err := json.Marshal(settings)
		if err != nil {
			return execution, err
		}
		return chat.ActionExecution{Message: string(raw), ResourceID: identity.OrganizationID, Identifier: identity.OrganizationID}, nil
	} else {
		result, err = s.portalBilling(ctx, authorize, request.RequestID)
	}
	if err != nil {
		return execution, err
	}
	return chat.ActionExecution{Message: "Billing session created.", ResourceID: result.ID, Identifier: identity.OrganizationID, URL: result.URL}, nil
}

func (e hostedOperatorExecutor) AuditAction(ctx context.Context, action chat.Action, outcome string) {
	if action.Kind == chat.ActionKind(operatortool.DeleteAttachment) {
		e.service.hubChangeAudit(ctx, action.Mutation, outcome)
		return
	}
	if action.Kind == chat.ActionMoveItem {
		e.service.hubChangeAudit(ctx, action.Mutation, outcome)
		return
	}
	if _, err := operatortool.WorkspaceDefinition(string(action.Kind)); err == nil {
		(workspaceOperatorExecutor{server: e.service}).AuditAction(ctx, action, outcome)
		return
	}
	if definition, ok := operatortool.Lookup(string(action.Kind)); ok && definition.Meta.Toolset == "projects" {
		hubProjectExecutor(e).AuditAction(ctx, action, outcome)
		return
	}
	if _, ok := operatortool.ChangeDefinition(string(action.Kind)); ok {
		hubOperatorExecutor(e).AuditAction(ctx, action, outcome)
		return
	}
	if operatortool.IsAdministration(string(action.Kind)) {
		e.service.administration.AuditAction(ctx, action, outcome)
		return
	}
	if hubFleetTool(string(action.Kind)) {
		hubFleetExecutor(e).AuditAction(ctx, action, outcome)
		return
	}
	credential, err := e.service.operatorBillingCredential(ctx, "billing", true)
	if err != nil {
		return
	}
	m := action.Mutation
	if err := e.service.hostedAudit(mutation.WithContext(ctx, m), credential.Hosted, outcome, string(action.Kind), "", 200); err != nil {
		e.service.config.Logger.Warn("billing mutation audit unavailable")
	}
}

func (s *Service) billingActionResult(action chat.Action) (operatortool.Result, error) {
	var settings *creditFundingInput
	if action.Kind == chat.ActionKind(operatortool.CreditAutoFund) && action.Status == chat.ActionSucceeded {
		if err := json.Unmarshal([]byte(action.Result), &settings); err != nil {
			return operatortool.Result{}, errBillingUnavailable
		}
	}
	return billingResult(struct {
		Action        chat.Action         `json:"preview"`
		ID            string              `json:"action_id"`
		Status        chat.ActionStatus   `json:"status"`
		ResourceID    string              `json:"resource_id,omitempty"`
		URL           string              `json:"url,omitempty"`
		ResultTool    string              `json:"result_tool"`
		CorrelationID string              `json:"correlation_id"`
		Settings      *creditFundingInput `json:"settings,omitempty"`
	}{action, action.ID, action.Status, action.IssueID, action.ResourceURL, operatortool.ActionResult, action.Mutation.CorrelationID, settings})
}
