package boardoverlay

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const pending = "pending"

type Overlay struct {
	current        hubrecords.CurrentChangePolicyRows
	currentMissing bool
	policies       map[string]policystore.Compatibility
	reviews        map[string]policystore.ReviewCompatibility
}

func evaluated(row viewstore.BoardRecord) bool {
	p := row.Display.ChangeProvenance
	return row.Display.Change != nil && p != nil && p.VersionID != "" && p.BaseStatus != "landed"
}

func Read(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, records []viewstore.BoardRecord) (Overlay, error) {
	var overlay Overlay
	var policies, reviews []string
	seen := map[string]bool{}
	add := func(list *[]string, kind, id string) {
		if id != "" && !seen[kind+id] {
			seen[kind+id] = true
			*list = append(*list, id)
		}
	}
	for _, row := range records {
		if evaluated(row) {
			add(&policies, "policy:", row.Display.ChangeProvenance.PolicyID)
			add(&reviews, "review:", row.Display.ChangeProvenance.ReviewPolicyID)
		}
	}
	if len(policies) == 0 {
		return overlay, nil
	}
	current, err := hubrecords.ReadChangeSummaryPolicyRows(ctx, reader, scope)
	if errors.Is(err, hubrecords.ErrNoApprovedRepositoryPolicy) {
		overlay.currentMissing = true
	} else if err != nil {
		return overlay, err
	}
	overlay.current = current
	add(&policies, "policy:", current.Project.PolicyID)
	if current.HasReview {
		add(&reviews, "review:", current.Review.ReviewPolicyID)
	}
	if overlay.policies, err = reader.PolicyCompatibility(ctx, scope.Project, policies); err != nil {
		return overlay, err
	}
	overlay.reviews, err = reader.ReviewCompatibility(ctx, scope.Project, reviews)
	return overlay, err
}

func (o Overlay) reviewRules(id, policyID string) (tracker.ChangeReviewPolicy, bool) {
	if facts, found := o.reviews[id]; found {
		return tracker.ChangeReviewPolicy{ID: id, PolicyID: facts.PolicyID, RequireReview: facts.RequireReview, RequiredChecks: facts.RequiredChecks}, true
	}
	policy, found := o.policies[policyID]
	if !found {
		return tracker.ChangeReviewPolicy{}, false
	}
	rules := tracker.ChangeReviewPolicy{PolicyID: policyID, RequireReview: policy.Gates.HumanReview, RequiredChecks: []tracker.ChangeCheckSpec{}}
	rules.ID = changerequest.PolicyID(rules)
	return rules, id == "" || rules.ID == id
}

func (o Overlay) Status(row viewstore.BoardRecord) string {
	p := row.Display.ChangeProvenance
	switch {
	case row.Display.Change == nil:
		return ""
	case p == nil:
		return pending
	case !evaluated(row):
		return p.BaseStatus
	}
	var approved changerequest.CompatibilityPolicy
	var rules tracker.ChangeReviewPolicy
	if !o.currentMissing {
		currentID := o.current.Project.PolicyID
		facts, found := o.policies[currentID]
		if !found {
			return pending
		}
		approved = changerequest.CompatibilityPolicy{ID: currentID, Gates: facts.Gates}
		reviewID := ""
		if o.current.HasReview {
			reviewID = o.current.Review.ReviewPolicyID
		}
		if rules, found = o.reviewRules(reviewID, currentID); !found {
			return pending
		}
	}
	facts, found := o.policies[p.PolicyID]
	if !found {
		return pending
	}
	var versionRules tracker.ChangeReviewPolicy
	if p.ReviewPolicyID != "" {
		if versionRules, found = o.reviewRules(p.ReviewPolicyID, p.PolicyID); !found {
			return pending
		}
	}
	version := changerequest.CompatibilityVersion{PolicyID: p.PolicyID, Policy: changerequest.CompatibilityPolicy{ID: p.PolicyID, Gates: facts.Gates}, ReviewPolicy: versionRules}
	if policyID, reviewID := changerequest.CompatiblePolicyIdentities(&version, approved, rules); policyID != p.PolicyID || reviewID != p.ReviewPolicyID {
		return "stale_policy"
	}
	return p.BaseStatus
}
