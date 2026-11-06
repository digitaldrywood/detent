package hubserver

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
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type summaryAttentionRecord struct {
	item                       corestore.WorkItem
	version                    eventstore.CollaborationVersion
	attempt                    tenantAttemptSnapshot
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
	query := reportstore.OverviewQuery{Projects: []tracker.ProjectID{tracker.ProjectID(project.id)}, Principal: principal, From: time.Unix(0, 0).UTC(), Split: start, To: r.end, Limit: 5000}
	records := []summaryAttentionRecord{}
	err := s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(r.organization), corestore.Serving, func(reader tenantstore.Reader) error {
		historyReader, ok := reader.(reportstore.HistoryReader)
		if !ok {
			return fmt.Errorf("daily summary history owner is unavailable")
		}
		attemptReader, ok := reader.(reportstore.AttemptReader)
		if !ok {
			return fmt.Errorf("daily summary attempt owner is unavailable")
		}
		history, err := historyReader.ReadHistory(ctx, query)
		if err != nil {
			return err
		}
		attempts, err := attemptReader.ReadAttemptFacts(ctx, reportstore.AttemptQuery{Projects: query.Projects, Principal: principal, From: query.From, To: r.end, Limit: 50000})
		if err != nil {
			return err
		}
		counts := map[tracker.NativeWorkItemID]int{}
		for _, attempt := range attempts {
			counts[attempt.WorkItemID]++
		}
		spend := map[tracker.NativeWorkItemID]float64{}
		if cost {
			usageReader, ok := reader.(reportstore.UsageReader)
			if !ok {
				return fmt.Errorf("daily summary usage owner is unavailable")
			}
			usageQuery := query
			usageQuery.Limit = 100000
			usage, err := usageReader.ReadReportUsage(ctx, usageQuery)
			if err != nil {
				return err
			}
			if _, err := postgresReportUsageRows(usage); err != nil {
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
			return fmt.Errorf("daily summary work item owner is unavailable")
		}
		for _, group := range history {
			items := map[tracker.NativeWorkItemID]corestore.WorkItem{}
			for offset := 0; offset < len(group.Issues); offset += corestore.MaxPageSize {
				ids := []tracker.NativeWorkItemID{}
				for _, fact := range group.Issues[offset:min(offset+corestore.MaxPageSize, len(group.Issues))] {
					ids = append(ids, fact.WorkItemID)
				}
				rows, err := batchReader.WorkItemsByNativeIDs(ctx, group.ProjectID, ids)
				if err != nil {
					return err
				}
				for _, item := range rows {
					items[item.NativeID] = item
				}
			}
			for _, fact := range group.Issues {
				item, ok := items[fact.WorkItemID]
				if !ok {
					return corestore.ErrNotFound
				}
				if item.Archived || principal != "" && stringValue(item.ActorPrincipalID) != principal {
					continue
				}
				record := summaryAttentionRecord{item: item, at: item.NativeCreatedAt, attempts: counts[item.NativeID], spent: spend[item.NativeID]}
				for _, event := range fact.Events {
					if event.Type != "workflow.transitioned" {
						continue
					}
					if event.FromState != event.ToState {
						record.at = event.At
					}
					if event.ToState == "Rework" && !event.At.Before(start) && event.At.Before(r.end) {
						record.reworks++
					}
				}
				state := stringValue(item.WorkflowStateName)
				if state != "Blocked" && state != "Human Review" && state != "In Progress" && !(state == "Done" && stringValue(item.ActorPrincipalID) == r.principal) && record.reworks < 2 {
					continue
				}
				if state == "Done" {
					record.version, err = reader.CollaborationVersion(ctx, string(item.NativeID), item.Revision)
					if err != nil {
						return err
					}
				}
				if state == "Blocked" {
					record.attempt.attempt, err = reader.LatestAttempt(ctx, item.ID)
					if err != nil && !errors.Is(err, corestore.ErrNotFound) {
						return err
					}
					if err == nil {
						record.hasAttempt = true
						record.attempt.lease, err = reader.Lease(ctx, record.attempt.attempt.LeaseID)
						if err != nil {
							return err
						}
						if record.attempt.lease.IssueID != item.ID || record.attempt.lease.FencingToken != record.attempt.attempt.FencingToken {
							return artifact.ErrIntegrity
						}
						record.attempt.event, err = reader.LatestRunEvent(ctx, item.ProjectID, record.attempt.attempt.ID)
						if err != nil {
							return err
						}
						if record.attempt.event.WorkItemID != item.NativeID {
							return artifact.ErrIntegrity
						}
						if err := readTenantAttemptPayload(ctx, reader, &record.attempt); err != nil {
							return err
						}
					}
				}
				records = append(records, record)
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	for i := range records {
		record := &records[i]
		if record.hasAttempt {
			run, err := s.hydrateNativeAttemptTenant(ctx, record.attempt, r.end)
			if err != nil {
				return nil, nil, err
			}
			record.human = run.Disposition != nil && run.Disposition.HumanAction
		}
		if stringValue(record.item.WorkflowStateName) == "Done" {
			issue, err := s.hydrateWorkItemRecord(ctx, record.item, record.version)
			if err != nil {
				return nil, nil, err
			}
			record.verificationPending = issue.Verification != nil && issue.Verification.State == "pending" && issue.Actor.PrincipalID == r.principal
		}
	}
	needs, watch := summaryAttentionEntries(records, r, project, start, base, orgSlug, cost)
	return needs, watch, nil
}

func summaryAttentionEntries(records []summaryAttentionRecord, r summaryRecipient, project summaryProjectRef, start time.Time, base, orgSlug string, cost bool) ([]summaryEntry, []summaryEntry) {
	needs, watch := []summaryEntry{}, []summaryEntry{}
	for _, record := range records {
		item := record.item
		state := stringValue(item.WorkflowStateName)
		row := summaryEntry{SummaryRow: emailtemplate.SummaryRow{Title: fmt.Sprintf("#%d %s", item.Number, item.Title), URL: base + issueBrowserPath(orgSlug, project.slug, int(item.Number))}, at: record.at}
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
