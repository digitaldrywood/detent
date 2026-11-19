package hostedaccounts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type Deps struct {
	Now                func() time.Time
	HostedOrganization string
	PublicURL          string
	ReadReceipt        func(context.Context, string, string) (string, error)
	StoreReceipt       func(context.Context, hubapi.APICredential, Receipt, mutation.Metadata, bool) error
	CreateOrganization func(context.Context, hubapi.APICredential, string) (string, error)
	StoredSession      func(context.Context, string, time.Time) (auth.Session, error)
	AcceptInvitation   func(context.Context, auth.Identity, string) error
}

type Service struct {
	deps Deps
}

func New(deps Deps) *Service {
	return &Service{deps: deps}
}

var ErrReceiptExists = errors.New("account receipt already exists")

type Receipt struct {
	Kind    string               `json:"kind"`
	Retry   string               `json:"retry"`
	Hash    string               `json:"hash"`
	Outcome string               `json:"outcome"`
	Output  operatoradmin.Output `json:"output"`
}

func (s *Service) replayReceipt(ctx context.Context, credential hubapi.APICredential, m mutation.Metadata) (operatoradmin.Output, bool, error) {
	raw, err := s.deps.ReadReceipt(ctx, credential.Hosted.Subject, m.RetryIdentity)
	if errors.Is(err, sql.ErrNoRows) {
		return operatoradmin.Output{}, false, nil
	}
	if err != nil {
		return operatoradmin.Output{}, true, err
	}
	var receipt Receipt
	if json.Unmarshal([]byte(raw), &receipt) != nil {
		return operatoradmin.Output{}, true, operatoradmin.ErrUnavailable
	}
	if receipt.Hash != m.InputHash {
		return operatoradmin.Output{}, true, mutation.ErrConflict
	}
	if receipt.Outcome != "succeeded" {
		return operatoradmin.Output{}, true, mutation.ErrUncertain
	}
	return receipt.Output, true, nil
}

// Account setup has no customer token row. Its receipts therefore reuse the
// existing hosted audit ledger; the pending receipt is reserved under the
// organization row lock before any provider call.
func (s *Service) Execute(ctx context.Context, credential hubapi.APICredential, name string, in operatoradmin.Input, m mutation.Metadata) (operatoradmin.Output, error) {
	if output, replayed, err := s.replayReceipt(ctx, credential, m); replayed {
		return output, err
	}
	record := func(recordCtx context.Context, outcome string, out operatoradmin.Output, reserve bool) error {
		return s.deps.StoreReceipt(recordCtx, credential, Receipt{Kind: "account_administration", Retry: m.RetryIdentity, Hash: m.InputHash, Outcome: outcome, Output: out}, m, reserve)
	}
	if err := record(ctx, "pending", operatoradmin.Output{}, true); errors.Is(err, ErrReceiptExists) {
		output, replayed, err := s.replayReceipt(ctx, credential, m)
		if !replayed {
			return operatoradmin.Output{}, mutation.ErrUncertain
		}
		return output, err
	} else if err != nil {
		return operatoradmin.Output{}, err
	}
	output := operatoradmin.Output{Reconnect: true}
	var err error
	switch name {
	case operatortool.OrganizationCreate:
		output.ResourceID, err = s.deps.CreateOrganization(ctx, credential, in.Name)
		output.URL = s.deps.PublicURL + "/auth/oidc/start"
	case operatortool.InvitationAccept:
		session, sessionErr := s.deps.StoredSession(ctx, credential.SessionHash, s.deps.Now())
		if sessionErr != nil {
			return operatoradmin.Output{}, sessionErr
		}
		err = s.deps.AcceptInvitation(ctx, auth.Identity{Subject: session.Identity.Subject, Email: session.Email, EmailVerified: true, Hosted: session.Identity}, in.InvitationID)
		output.ResourceID = in.InvitationID
		output.URL = s.deps.PublicURL + "/auth/oidc/start"
	case operatortool.SupportStart:
		output.ResourceID = s.deps.HostedOrganization
		output.URL = s.deps.PublicURL + "/support"
		output.Data = json.RawMessage(`{"interactive_required":true}`)
	default:
		return operatoradmin.Output{}, operatoradmin.ErrUnavailable
	}
	if err != nil {
		return operatoradmin.Output{}, err
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := record(persist, "succeeded", output, false); err != nil {
		return operatoradmin.Output{}, mutation.ErrUncertain
	}
	return output, nil
}
