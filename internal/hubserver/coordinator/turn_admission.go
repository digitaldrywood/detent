package coordinator

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type TurnAdmissionDeps struct {
	RequireExecution func(context.Context, tenantstore.Reader, time.Time) error
	HasLuna          func() bool
	CreditMode       func() string
	IncludedCredits  func(context.Context, tenantstore.Reader, time.Time) (int64, error)
	ModelChoice      func(context.Context, tenantstore.Reader, tracker.OrganizationID, time.Time) (bool, error)
	Models           func(context.Context, tenantstore.Reader, tracker.ProjectID, time.Time) ([]conversations.ConversationModel, error)
}

// ErrNothingPending aborts the start transaction when the
// re-check inside the transaction finds no work.
var ErrNothingPending = errors.New("coordinator: nothing pending")

func (d TurnAdmissionDeps) Check(ctx context.Context, r tenantstore.Reader, record conversations.ConversationRecord, principal string, now time.Time, modelChoice bool) (bool, error, error) {
	var refusal error
	decision, err := MonthlyBudget(ctx, r, record.ProjectID, now)
	if err != nil {
		return modelChoice, refusal, err
	}
	if !decision.Allowed {
		return modelChoice, refusal, ErrNothingPending
	}
	refusal = d.RequireExecution(ctx, r, now)
	var limit *hostedplans.LimitError
	if refusal != nil && !errors.As(refusal, &limit) {
		return modelChoice, refusal, refusal
	}
	if d.HasLuna() && d.CreditMode() != "" {
		account, err := r.BillingCreditAccount(ctx, d.CreditMode())
		if err != nil {
			return modelChoice, refusal, err
		}
		balance, failure := account.BalanceMicros, account.Failure
		refusal = nil
		included, err := d.IncludedCredits(ctx, r, now)
		if err != nil {
			return modelChoice, refusal, err
		}
		if balance+included <= 0 {
			owner := false
			credential, err := r.AccessCredential(ctx, principal)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return modelChoice, refusal, err
			}
			if credential.UserID != "" {
				member, err := r.AccessMember(ctx, credential.UserID)
				if err != nil && !errors.Is(err, corestore.ErrNotFound) {
					return modelChoice, refusal, err
				}
				owner = member.Active && member.Role == "owner"
			}
			message := "AI credits are exhausted. Ask an organization owner to buy credits in billing settings."
			if owner {
				message = "AI credits are exhausted. Buy credits in billing settings."
				if failure != "" {
					message += " Auto-fund is disabled: " + failure + "."
				}
			}
			refusal = errors.New(message)
		}
	}
	if refusal == nil && d.HasLuna() {
		modelChoice, err = d.ModelChoice(ctx, r, record.OrganizationID, now)
		if err != nil {
			return modelChoice, refusal, err
		}
		if model := record.Preferences.ModelValue(); model != "" {
			models, err := d.Models(ctx, r, record.ProjectID, now)
			if err != nil {
				return modelChoice, refusal, err
			}
			if !slices.ContainsFunc(models, func(choice conversations.ConversationModel) bool { return choice.ID == model }) {
				refusal = fmt.Errorf("preferences: model %q is not one of the available choices", model)
			}
		}
	}
	return modelChoice, refusal, nil
}
