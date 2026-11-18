package hubserver

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) backfillBoardPolicyFacts(ctx context.Context, organization tracker.OrganizationID, candidate viewstore.BoardCandidate) error {
	var policyRow *policystore.Revision
	var reviewRow *eventstore.ReviewPolicy
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		card, err := r.BoardItem(ctx, candidate.ProjectID, candidate.WorkItemID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		provenance := card.Display.ChangeProvenance
		if card.Display.Change == nil || provenance == nil || provenance.VersionID == "" {
			return nil
		}
		policies, reviews := []string{provenance.PolicyID}, []string{provenance.ReviewPolicyID}
		if current, err := r.ProjectPolicy(ctx, candidate.ProjectID); err == nil {
			policies = append(policies, current.PolicyID)
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if current, err := r.ReviewPolicy(ctx, candidate.ProjectID); err == nil {
			reviews = append(reviews, current.ReviewPolicyID)
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		known, err := r.PolicyCompatibility(ctx, candidate.ProjectID, policies)
		if err != nil {
			return err
		}
		for _, id := range policies {
			if _, found := known[id]; found || id == "" {
				continue
			}
			row, err := r.ProjectPolicyRevision(ctx, candidate.ProjectID, id)
			if errors.Is(err, corestore.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			policyRow = &row
			return nil
		}
		knownReviews, err := r.ReviewCompatibility(ctx, candidate.ProjectID, reviews)
		if err != nil {
			return err
		}
		for _, id := range reviews {
			if _, found := knownReviews[id]; found || id == "" {
				continue
			}
			row, err := r.ReviewPolicyRevision(ctx, candidate.ProjectID, id)
			if errors.Is(err, corestore.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			reviewRow = &row
			return nil
		}
		return nil
	})
	if err != nil {
		return err
	}
	content := eventstore.NewContent(s.runDetailStore())
	if policyRow != nil {
		descriptor, err := content.ProjectPolicy(ctx, *policyRow)
		if err != nil {
			return err
		}
		row := *policyRow
		row.Compatibility = policystore.CompatibilityOf(descriptor)
		return s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
			return w.BackfillPolicyCompatibility(ctx, row)
		})
	}
	if reviewRow == nil {
		return nil
	}
	rules, err := content.ReviewPolicy(ctx, *reviewRow)
	if err != nil {
		return err
	}
	row := *reviewRow
	if row.Compatibility = eventstore.ReviewCompatibilityOf(rules); row.Compatibility == nil {
		return nil
	}
	return s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		return w.BackfillReviewCompatibility(ctx, row)
	})
}
