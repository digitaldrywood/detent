package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

// A dedicated bootstrap/staff account may not yet have customer membership.
// This identity can use account setup only; it carries no project/admin role.
func (s *Service) hostedAccountCredential(ctx context.Context, hash string) (apiCredential, error) {
	session, err := s.WebSession(ctx, hash, s.config.now())
	if err != nil || session.Identity == nil || session.Identity.SupportActor != "" {
		return apiCredential{}, operatortool.ErrAccessDenied
	}
	return apiCredential{ID: "account:" + session.Identity.Subject, Hash: hash, SessionHash: hash, Scope: apiScopeOperator, NativeOnly: true, Hosted: session.Identity, HostedRole: "account"}, nil
}

func (s *Service) createHostedOrganizationFor(ctx context.Context, credential apiCredential, name string) (string, error) {
	if credential.Hosted == nil || credential.Hosted.SupportActor != "" || credential.Hosted.Subject != s.config.Hosted.BootstrapSubject {
		return "", operatortool.ErrAccessDenied
	}
	session, err := s.storedWebSession(ctx, credential.SessionHash, s.config.now())
	if err != nil || hostedEmailListed(s.config.Hosted.StaffEmails, session.Email) {
		return "", operatortool.ErrAccessDenied
	}
	var members int
	if err := s.database.db.QueryRowContext(ctx, "SELECT count(*) FROM hosted_members").Scan(&members); err != nil || members != 0 {
		return "", operatortool.ErrAccessDenied
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 120 {
		return "", operatortool.ErrInvalidArguments
	}
	providerID, err := s.hostedProviderOrganization(ctx)
	if err != nil {
		return "", err
	}
	if providerID == "" {
		organization, err := s.config.Hosted.Provider.CreateOrganization(ctx, s.config.Hosted.OrganizationID, name)
		if err != nil || organization.ExternalID != s.config.Hosted.OrganizationID || !hostedSafeID(organization.ID) {
			return "", auth.ErrHostedIdentity
		}
		providerID = organization.ID
		if _, err := s.database.db.ExecContext(ctx, "UPDATE hosted_tenant SET provider_id=? WHERE singleton=1 AND provider_id=''", providerID); err != nil {
			return "", err
		}
	}
	membership, err := s.config.Hosted.Provider.CreateMembership(ctx, credential.Hosted.Subject, providerID, "owner")
	if err != nil {
		return "", err
	}
	if err := s.storeHostedMember(ctx, auth.Identity{Subject: credential.Hosted.Subject, Email: session.Email, EmailVerified: true}, membership, name); err != nil {
		return "", err
	}
	return s.config.Hosted.OrganizationID, nil
}

type accountReceipt struct {
	Kind    string               `json:"kind"`
	Retry   string               `json:"retry"`
	Hash    string               `json:"hash"`
	Outcome string               `json:"outcome"`
	Output  operatoradmin.Output `json:"output"`
}

// Account setup has no customer token row. Its receipts therefore reuse the
// existing hosted audit ledger and dashboard mutation serialization.
func (s *Service) executeHostedAccountFor(ctx context.Context, credential apiCredential, name string, in operatoradmin.Input, m mutation.Metadata) (operatoradmin.Output, error) {
	if locked, _ := ctx.Value(hostedMutationContext{}).(bool); !locked {
		s.hostedMutationMu.Lock()
		defer s.hostedMutationMu.Unlock()
	}
	var raw string
	err := s.database.db.QueryRowContext(ctx, `SELECT event FROM hosted_audit WHERE organization_id=? AND actual_actor=? AND json_extract(CASE WHEN json_valid(event) THEN event ELSE '{}' END,'$.kind')='account_administration' AND json_extract(CASE WHEN json_valid(event) THEN event ELSE '{}' END,'$.retry')=? ORDER BY id DESC LIMIT 1`, s.config.Hosted.OrganizationID, credential.Hosted.Subject, m.RetryIdentity).Scan(&raw)
	if err == nil {
		var receipt accountReceipt
		if json.Unmarshal([]byte(raw), &receipt) != nil {
			return operatoradmin.Output{}, operatoradmin.ErrUnavailable
		}
		if receipt.Hash != m.InputHash {
			return operatoradmin.Output{}, mutation.ErrConflict
		}
		if receipt.Outcome != "succeeded" {
			return operatoradmin.Output{}, mutation.ErrUncertain
		}
		return receipt.Output, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return operatoradmin.Output{}, err
	}
	record := func(recordCtx context.Context, outcome string, out operatoradmin.Output) error {
		raw, err := json.Marshal(accountReceipt{Kind: "account_administration", Retry: m.RetryIdentity, Hash: m.InputHash, Outcome: outcome, Output: out})
		if err != nil {
			return err
		}
		return s.hostedAudit(recordCtx, credential.Hosted, string(raw), "mcp/account", "", 0)
	}
	if err := record(ctx, "pending", operatoradmin.Output{}); err != nil {
		return operatoradmin.Output{}, err
	}
	output := operatoradmin.Output{Reconnect: true}
	err = nil
	switch name {
	case operatortool.OrganizationCreate:
		output.ResourceID, err = s.createHostedOrganizationFor(ctx, credential, in.Name)
		output.URL = s.config.Hosted.PublicURL + "/auth/oidc/start"
	case operatortool.InvitationAccept:
		session, sessionErr := s.storedWebSession(ctx, credential.SessionHash, s.config.now())
		if sessionErr != nil {
			return operatoradmin.Output{}, sessionErr
		}
		err = s.acceptHostedInvitationIDFor(ctx, auth.Identity{Subject: session.Identity.Subject, Email: session.Email, EmailVerified: true, Hosted: session.Identity}, in.InvitationID)
		output.ResourceID = in.InvitationID
		output.URL = s.config.Hosted.PublicURL + "/auth/oidc/start"
	case operatortool.SupportStart:
		output.ResourceID = s.config.Hosted.OrganizationID
		output.URL = s.config.Hosted.PublicURL + "/support"
		output.Data = json.RawMessage(`{"interactive_required":true}`)
	default:
		return operatoradmin.Output{}, operatoradmin.ErrUnavailable
	}
	if err != nil {
		return operatoradmin.Output{}, err
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := record(persist, "succeeded", output); err != nil {
		return operatoradmin.Output{}, mutation.ErrUncertain
	}
	return output, nil
}
