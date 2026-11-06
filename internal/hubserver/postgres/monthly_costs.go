package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ReadMonthlyCosts(ctx context.Context, q billingstore.MonthlyCostQuery) (billingstore.MonthlyCostSnapshot, error) {
	out := billingstore.MonthlyCostSnapshot{}
	if q.From.IsZero() || !q.To.After(q.From) {
		return out, errors.New("invalid monthly cost window")
	}
	revisions, err := s.queries.MonthlyCostRevisions(ctx, pgquery.MonthlyCostRevisionsParams{OrganizationID: s.organization, PeriodStart: q.From.UTC().Format(costPeriodLayout), PeriodEnd: q.To.UTC().Format(costPeriodLayout), AllProjects: q.Projects == nil, Projects: q.Projects})
	if err != nil {
		return out, mapError(ctx, err)
	}
	if len(revisions) > 5000 {
		return out, reportstore.ErrPopulationExceeded
	}
	for _, r := range revisions {
		if r.PayloadHash != r.Sha256 {
			return out, errors.New("monthly cost archive identity mismatch")
		}
		received, err := time.Parse(costPeriodLayout, r.ReceivedAt)
		if err != nil {
			return out, err
		}
		out.Revisions = append(out.Revisions, billingstore.MonthlyCostRevision{SourceKey: r.SourceKey, CostRevision: billingstore.CostRevision{ProjectID: r.ProjectID, Revision: r.Revision, ReceivedAt: received, Reference: billingstore.AuditReference{BindingID: r.BindingID, CustomerOwned: r.CustomerOwned, Key: r.ObjectKey, SHA256: r.Sha256, CompressedBytes: r.CompressedBytes, ContentBytes: r.ContentBytes, Codec: r.Codec, FormatVersion: int(r.FormatVersion), Kind: r.Kind, Retention: r.Retention}}})
	}
	usage, err := s.queries.MonthlyConversationUsage(ctx, pgquery.MonthlyConversationUsageParams{OrganizationID: s.organization, PeriodStart: q.From.UnixMicro(), PeriodEnd: q.To.UnixMicro(), AllProjects: q.Projects == nil, Projects: q.Projects})
	if err != nil {
		return out, mapError(ctx, err)
	}
	if len(usage) > 5000 {
		return out, reportstore.ErrPopulationExceeded
	}
	for _, r := range usage {
		u := billingstore.ConversationUsage{OrganizationID: r.OrganizationID, ProjectID: tracker.ProjectID(r.ProjectID), ConversationID: r.ConversationID, TurnID: r.TurnID, Provider: r.Provider, Model: r.Model, OccurredAt: time.UnixMicro(r.OccurredAt).UTC(), Input: r.Input, CachedInput: r.CachedInput, Output: r.Output, ReasoningOutput: r.ReasoningOutput, Outcome: r.Outcome}
		if r.PriceID.Valid {
			u.PriceID = &r.PriceID.Int64
		}
		if r.CostUsd.Valid {
			u.CostUSD = &r.CostUsd.Float64
		}
		if r.CacheSavingsUsd.Valid {
			u.CacheSavingsUSD = &r.CacheSavingsUsd.Float64
		}
		out.Conversations = append(out.Conversations, u)
	}
	resources, err := s.queries.MonthlySpriteResources(ctx, pgquery.MonthlySpriteResourcesParams{OrganizationID: s.organization, AllProjects: q.Projects == nil, Projects: q.Projects})
	if err != nil {
		return out, mapError(ctx, err)
	}
	if len(resources) > 5000 {
		return out, reportstore.ErrPopulationExceeded
	}
	for _, r := range resources {
		out.Resources = append(out.Resources, billingstore.MonthlyCostResource{ProjectID: r.ProjectID, ProviderAccount: r.ProviderAccount, Name: r.Name})
	}
	counts, err := s.queries.MonthlyAttemptCoverage(ctx, pgquery.MonthlyAttemptCoverageParams{OrganizationID: s.organization, PeriodStart: q.From, PeriodEnd: q.To, AllProjects: q.Projects == nil, Projects: q.Projects})
	out.ActiveAttempts, out.UnreportedAttempts = counts.Active, counts.Unreported
	return out, mapError(ctx, err)
}
