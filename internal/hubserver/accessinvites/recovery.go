package accessinvites

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (s *Service) RecoverPending(ctx context.Context, session auth.Session) (bool, error) {
	if session.Identity == nil || session.Identity.SupportActor != "" || session.Email == "" || emailListed(s.deps.StaffEmails, session.Email) {
		return false, nil
	}
	var id string
	var err error
	var latest time.Time
	after := ""
	err = s.read(ctx, s.deps.Organization, func(r tenantstore.Reader) error {
		for {
			page, err := r.AccessInvitations(ctx, after, 200)
			if err != nil {
				return err
			}
			for _, row := range page {
				if row.AcceptedUserID == "" && strings.EqualFold(row.Email, session.Email) && row.ExpiresAt.After(s.deps.Now()) && row.CreatedAt.After(latest) {
					id, latest = row.ID, row.CreatedAt
				}
			}
			if len(page) < 200 {
				return nil
			}
			after = page[len(page)-1].ID
		}
	})
	if err == nil && id == "" {
		err = sql.ErrNoRows
	}
	if err != nil {
		if !errors.Is(s.deps.Classify(err), operatortool.ErrAccessDenied) {
			return false, s.deps.Classify(err)
		}
		return false, nil
	}
	invitation, err := auth.LookupInvitationID(ctx, s.deps.Provider, id)
	if err != nil && !errors.Is(s.deps.Classify(err), operatortool.ErrAccessDenied) {
		return false, s.deps.Classify(err)
	}
	if err != nil || invitation.State != "accepted" || invitation.AcceptedUserID != session.Identity.Subject {
		return false, nil
	}
	if err := s.Accept(ctx, auth.Identity{Subject: session.Identity.Subject, Email: session.Email, EmailVerified: true, Hosted: session.Identity}, id, true); err != nil {
		if !errors.Is(s.deps.Classify(err), operatortool.ErrAccessDenied) {
			return false, s.deps.Classify(err)
		}
	}
	return true, nil
}

func emailListed(emails []string, value string) bool {
	for _, email := range emails {
		if strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(value)) {
			return true
		}
	}
	return false
}
