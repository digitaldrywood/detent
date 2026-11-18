package dailysummary

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/emailtemplate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type summaryAttentionRecord struct {
	item                       corestore.WorkItem
	version                    eventstore.CollaborationVersion
	attempt                    any
	hasAttempt                 bool
	at                         time.Time
	attempts, reworks          int
	spent                      float64
	human, verificationPending bool
}

func (s *Service) readSummaryAttentionTenant(ctx context.Context, r summaryRecipient, project summaryProjectRef, start time.Time, base, orgSlug string, cost bool) ([]summaryEntry, []summaryEntry, error) {
	principal := ""
	if r.role == "reporter" {
		principal = r.principal
	}
	query := reportstore.SummaryAttentionQuery{Project: tracker.ProjectID(project.id), Principal: principal, Recipient: r.principal, From: start, To: r.end, Limit: 5000}
	records := []summaryAttentionRecord{}
	err := s.deps.Repository.ReadTenant(ctx, tracker.OrganizationID(r.organization), corestore.Serving, func(reader tenantstore.Reader) error {
		candidateReader, ok := reader.(reportstore.SummaryAttentionReader)
		if !ok {
			return errors.New("daily summary attention owner is unavailable")
		}
		candidates, err := candidateReader.ReadSummaryAttention(ctx, query)
		if err != nil {
			return err
		}
		ids := make([]tracker.NativeWorkItemID, len(candidates))
		for i, candidate := range candidates {
			ids[i] = candidate.WorkItemID
		}
		spend := map[tracker.NativeWorkItemID]float64{}
		if cost {
			usageReader, ok := reader.(reportstore.SummaryUsageReader)
			if !ok {
				return errors.New("daily summary usage owner is unavailable")
			}
			usage, err := usageReader.ReadSummaryUsage(ctx, query.Project, principal, r.end, r.end, ids)
			if err != nil {
				return err
			}
			if err := s.deps.Reports.ValidateUsage(usage); err != nil {
				return err
			}
			for _, row := range usage {
				spend[row.WorkItemID] += row.Cost
			}
		}
		batchReader, ok := reader.(interface {
			WorkItemsByNativeIDs(context.Context, tracker.ProjectID, []tracker.NativeWorkItemID) ([]corestore.WorkItem, error)
		})
		if !ok {
			return errors.New("daily summary work item owner is unavailable")
		}
		items := map[tracker.NativeWorkItemID]corestore.WorkItem{}
		for offset := 0; offset < len(ids); offset += corestore.MaxPageSize {
			rows, err := batchReader.WorkItemsByNativeIDs(ctx, query.Project, ids[offset:min(offset+corestore.MaxPageSize, len(ids))])
			if err != nil {
				return err
			}
			for _, item := range rows {
				items[item.NativeID] = item
			}
		}
		for _, candidate := range candidates {
			item, ok := items[candidate.WorkItemID]
			if !ok {
				return corestore.ErrNotFound
			}
			if item.Archived || principal != "" && hubapi.StringValue(item.ActorPrincipalID) != principal {
				continue
			}
			record := summaryAttentionRecord{item: item, at: candidate.At, attempts: int(candidate.Attempts), reworks: int(candidate.Reworks), spent: spend[item.NativeID]}
			state := hubapi.StringValue(item.WorkflowStateName)
			if state != "Blocked" && state != "Human Review" && state != "In Progress" && (state != "Done" || hubapi.StringValue(item.ActorPrincipalID) != r.principal) && record.reworks < 2 {
				continue
			}
			if state == "Done" {
				record.version, err = reader.CollaborationVersion(ctx, string(item.NativeID), item.Revision)
				if err != nil {
					return err
				}
			}
			if state == "Blocked" {
				attempt, err := reader.LatestAttempt(ctx, item.ID)
				if err != nil && !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				if err == nil {
					record.hasAttempt = true
					lease, err := reader.Lease(ctx, attempt.LeaseID)
					if err != nil {
						return err
					}
					if lease.IssueID != item.ID || lease.FencingToken != attempt.FencingToken {
						return artifact.ErrIntegrity
					}
					event, err := reader.LatestRunEvent(ctx, item.ProjectID, attempt.ID)
					if err != nil {
						return err
					}
					if event.WorkItemID != item.NativeID {
						return artifact.ErrIntegrity
					}
					record.attempt, err = s.deps.WorkItems.AttemptSnapshot(ctx, reader, attempt, lease, event)
					if err != nil {
						return err
					}
				}
			}
			records = append(records, record)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	for i := range records {
		record := &records[i]
		if record.hasAttempt {
			run, err := s.deps.WorkItems.Attempt(ctx, record.attempt, r.end)
			if err != nil {
				return nil, nil, err
			}
			record.human = run.Disposition != nil && run.Disposition.HumanAction
		}
		if hubapi.StringValue(record.item.WorkflowStateName) == "Done" {
			issue, err := s.deps.WorkItems.Issue(ctx, record.item, record.version)
			if err != nil {
				return nil, nil, err
			}
			record.verificationPending = issue.Verification != nil && issue.Verification.State == "pending" && issue.Actor.PrincipalID == r.principal
		}
	}
	needs, watch := s.summaryAttentionEntries(records, r, project, start, base, orgSlug, cost)
	return needs, watch, nil
}

func (s *Service) summaryAttentionEntries(records []summaryAttentionRecord, r summaryRecipient, project summaryProjectRef, start time.Time, base, orgSlug string, cost bool) ([]summaryEntry, []summaryEntry) {
	needs, watch := []summaryEntry{}, []summaryEntry{}
	for _, record := range records {
		item := record.item
		state := hubapi.StringValue(item.WorkflowStateName)
		row := summaryEntry{SummaryRow: emailtemplate.SummaryRow{Title: fmt.Sprintf("#%d %s", item.Number, item.Title), URL: base + s.deps.IssuePath(orgSlug, project.slug, int(item.Number))}, at: record.at}
		wait := fmt.Sprintf("%gh", max(0, r.end.Sub(record.at).Truncate(time.Hour).Hours()))
		switch state {
		case "Blocked":
			if record.human {
				row.Detail = "Blocked on your answer · asked " + wait + " ago · " + project.name
				row.blocked = true
				row.Alert = true
				needs = append(needs, row)
			}
		case "Human Review":
			row.Detail = "Waiting in Human Review for " + wait + " · " + project.name
			needs = append(needs, row)
		case "Done":
			if record.verificationPending {
				row.Detail = "Done · please verify · " + project.name
				needs = append(needs, row)
			}
		}

		if state == "In Progress" && (r.end.Sub(record.at) > 24*time.Hour || record.attempts >= 3) {
			if record.attempts >= 3 {
				row.priority = 2
			}
			row.Detail = fmt.Sprintf("In progress %s · %d attempts", wait, record.attempts)
			if cost {
				row.Detail += " · " + summaryMoney(record.spent) + " so far"
			}
			watch = append(watch, row)
		} else if record.reworks >= 2 {
			row.priority = 1
			row.Detail = fmt.Sprintf("Back to Rework %d times in %gh", record.reworks, r.end.Sub(start).Hours())
			watch = append(watch, row)
		}
	}
	slices.SortStableFunc(needs, func(a, b summaryEntry) int {
		if a.blocked != b.blocked {
			if a.blocked {
				return -1
			}
			return 1
		}
		return a.at.Compare(b.at)
	})
	slices.SortStableFunc(watch, func(a, b summaryEntry) int {
		if a.priority != b.priority {
			return b.priority - a.priority
		}
		return a.at.Compare(b.at)
	})
	return needs[:min(100, len(needs))], watch[:min(5, len(watch))]
}
