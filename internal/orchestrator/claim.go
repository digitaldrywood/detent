package orchestrator

import (
	"context"
	"sort"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
)

const claimTimeFormat = time.RFC3339Nano

func (o *Orchestrator) claimIssue(ctx context.Context, issue connector.Issue, now time.Time) (connector.Issue, Claimed, bool) {
	issue = cloneIssue(issue)
	claim := Claimed{
		Issue:     issue,
		ClaimedAt: now,
	}
	if !o.cfg.Claiming.Enabled {
		return issue, claim, true
	}

	owner := o.claimOwner()
	if owner == "" || o.cfg.Claiming.LeaseField == "" || o.fieldClaimMissingOwnerField() {
		return connector.Issue{}, Claimed{}, false
	}
	if !o.claimable(issue, owner, now) {
		return connector.Issue{}, Claimed{}, false
	}
	if err := o.writeClaim(ctx, issue.ID, owner, now); err != nil {
		if o.logger != nil {
			o.logger.Warn("claim issue failed", "issue_id", issue.ID, "owner", owner, "error", err)
		}
		return connector.Issue{}, Claimed{}, false
	}

	refreshed, ok := o.refetchClaimedIssue(ctx, issue)
	if !ok {
		return connector.Issue{}, Claimed{}, false
	}
	claim, ok = o.verifiedClaim(refreshed, owner)
	if !ok {
		return connector.Issue{}, Claimed{}, false
	}
	return claim.Issue, claim, true
}

func (o *Orchestrator) fieldClaimMissingOwnerField() bool {
	return o.cfg.Claiming.OwnershipMode == workflowconfig.IdentityOwnershipField &&
		strings.TrimSpace(o.cfg.Claiming.OwnerField) == ""
}

func (o *Orchestrator) writeClaim(ctx context.Context, issueID string, owner string, now time.Time) error {
	switch o.cfg.Claiming.OwnershipMode {
	case workflowconfig.IdentityOwnershipField:
		if err := o.connector.SetField(ctx, issueID, o.cfg.Claiming.OwnerField, owner); err != nil {
			return err
		}
	default:
		if err := o.connector.SetAssignee(ctx, issueID, owner); err != nil {
			return err
		}
	}
	return o.connector.SetField(ctx, issueID, o.cfg.Claiming.LeaseField, formatClaimTime(now))
}

func (o *Orchestrator) refetchClaimedIssue(ctx context.Context, fallback connector.Issue) (connector.Issue, bool) {
	issues, err := o.connector.FetchIssueStatesByIDs(ctx, []string{fallback.ID})
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("refetch claimed issue failed", "issue_id", fallback.ID, "error", err)
		}
		return connector.Issue{}, false
	}
	for _, issue := range issues {
		if issue.ID == fallback.ID {
			return cloneIssue(issue), true
		}
	}
	return connector.Issue{}, false
}

func (o *Orchestrator) claimable(issue connector.Issue, owner string, now time.Time) bool {
	winner := o.claimWinner(issue)
	if winner == "" || sameClaimOwner(winner, owner) {
		return true
	}
	lease, ok := o.issueLease(issue)
	if !ok {
		return true
	}
	return o.leaseStale(lease, now)
}

func (o *Orchestrator) verifiedClaim(issue connector.Issue, owner string) (Claimed, bool) {
	winner := o.claimWinner(issue)
	if !sameClaimOwner(winner, owner) {
		return Claimed{}, false
	}
	lease, ok := o.issueLease(issue)
	if !ok {
		return Claimed{}, false
	}
	issue = cloneIssue(issue)
	return Claimed{
		Issue:          issue,
		ClaimedAt:      lease,
		Owner:          owner,
		LeaseRenewedAt: lease,
		LeaseExpiresAt: o.leaseExpiresAt(lease),
	}, true
}

func (o *Orchestrator) heartbeatRunningClaims(ctx context.Context, state *State, now time.Time) {
	if !o.cfg.Claiming.Enabled || o.cfg.Claiming.LeaseField == "" {
		return
	}
	owner := o.claimOwner()
	if owner == "" {
		return
	}

	for _, issueID := range sortedKeys(state.Running) {
		claimed, ok := state.Claimed[issueID]
		if !ok {
			continue
		}
		if claimed.Owner != "" && !sameClaimOwner(claimed.Owner, owner) {
			continue
		}
		if !o.claimHeartbeatDue(claimed, now) {
			continue
		}
		if err := o.connector.SetField(ctx, issueID, o.cfg.Claiming.LeaseField, formatClaimTime(now)); err != nil {
			if o.logger != nil {
				o.logger.Warn("claim heartbeat failed", "issue_id", issueID, "owner", owner, "error", err)
			}
			continue
		}
		claimed.Owner = owner
		claimed.LeaseRenewedAt = now
		claimed.LeaseExpiresAt = o.leaseExpiresAt(now)
		claimed.Issue = issueWithLeaseField(claimed.Issue, o.cfg.Claiming.LeaseField, now)
		state.Claimed[issueID] = claimed

		running := state.Running[issueID]
		running.Issue = issueWithLeaseField(running.Issue, o.cfg.Claiming.LeaseField, now)
		state.Running[issueID] = running
	}
}

func (o *Orchestrator) claimHeartbeatDue(claim Claimed, now time.Time) bool {
	if claim.LeaseRenewedAt.IsZero() {
		return true
	}
	interval := o.cfg.Claiming.HeartbeatInterval
	if interval <= 0 {
		interval = o.cfg.Claiming.LeaseTTL / 2
	}
	if interval <= 0 {
		return false
	}
	return !now.Before(claim.LeaseRenewedAt.Add(interval))
}

func (o *Orchestrator) claimOwner() string {
	if o.cfg.Claiming.OwnershipMode == workflowconfig.IdentityOwnershipField {
		for _, value := range []string{o.cfg.Claiming.Owner, o.cfg.Claiming.AssigneeLogin, o.cfg.SelectorContext.Persona, o.cfg.SelectorContext.InstanceLogin} {
			if owner := strings.TrimSpace(value); owner != "" {
				return owner
			}
		}
		return o.connectorLogin()
	}

	for _, value := range []string{o.cfg.Claiming.AssigneeLogin, o.cfg.SelectorContext.InstanceLogin} {
		if owner := strings.TrimSpace(value); owner != "" {
			return owner
		}
	}
	if owner := o.connectorLogin(); owner != "" {
		return owner
	}
	return strings.TrimSpace(o.cfg.Claiming.Owner)
}

func (o *Orchestrator) connectorLogin() string {
	identifier, ok := o.connector.(connector.InstanceIdentifier)
	if !ok {
		return ""
	}
	return strings.TrimSpace(identifier.InstanceLogin())
}

func (o *Orchestrator) claimWinner(issue connector.Issue) string {
	owners := o.issueOwners(issue)
	if len(owners) == 0 {
		return ""
	}
	sortClaimOwners(owners)
	return owners[0]
}

func (o *Orchestrator) issueOwners(issue connector.Issue) []string {
	if o.cfg.Claiming.OwnershipMode == workflowconfig.IdentityOwnershipField {
		owner := ""
		if issue.Fields != nil {
			owner = strings.TrimSpace(issue.Fields[o.cfg.Claiming.OwnerField])
		}
		if owner == "" {
			return nil
		}
		return []string{owner}
	}

	owners := make([]string, 0, len(issue.Assignees)+1)
	seen := map[string]struct{}{}
	for _, owner := range append([]string{issue.AssigneeID}, issue.Assignees...) {
		owner = strings.TrimSpace(owner)
		if owner == "" {
			continue
		}
		key := normalizeClaimOwner(owner)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		owners = append(owners, owner)
	}
	return owners
}

func (o *Orchestrator) issueLease(issue connector.Issue) (time.Time, bool) {
	if issue.Fields == nil || o.cfg.Claiming.LeaseField == "" {
		return time.Time{}, false
	}
	return parseClaimTime(issue.Fields[o.cfg.Claiming.LeaseField])
}

func (o *Orchestrator) leaseStale(lease time.Time, now time.Time) bool {
	if lease.IsZero() {
		return true
	}
	expires := o.leaseExpiresAt(lease)
	if expires.IsZero() {
		return false
	}
	return !now.Before(expires)
}

func (o *Orchestrator) leaseExpiresAt(lease time.Time) time.Time {
	if lease.IsZero() || o.cfg.Claiming.LeaseTTL <= 0 {
		return time.Time{}
	}
	return lease.Add(o.cfg.Claiming.LeaseTTL)
}

func issueWithLeaseField(issue connector.Issue, fieldName string, value time.Time) connector.Issue {
	issue = cloneIssue(issue)
	if issue.Fields == nil {
		issue.Fields = map[string]string{}
	}
	issue.Fields[fieldName] = formatClaimTime(value)
	return issue
}

func formatClaimTime(value time.Time) string {
	return value.UTC().Format(claimTimeFormat)
}

func parseClaimTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func sameClaimOwner(left string, right string) bool {
	return normalizeClaimOwner(left) == normalizeClaimOwner(right) && normalizeClaimOwner(left) != ""
}

func normalizeClaimOwner(owner string) string {
	return strings.ToLower(strings.TrimSpace(owner))
}

func sortClaimOwners(owners []string) {
	sort.SliceStable(owners, func(i, j int) bool {
		return normalizeClaimOwner(owners[i]) < normalizeClaimOwner(owners[j])
	})
}
