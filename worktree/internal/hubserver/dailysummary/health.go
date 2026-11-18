package dailysummary

import (
	"context"
	"errors"
	"slices"

	"github.com/digitaldrywood/detent/internal/emailtemplate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readSummaryHealthTenant(ctx context.Context, recipient summaryRecipient, projects []string, base, orgSlug string) ([]summaryEntry, error) {
	out := []summaryEntry{}
	err := s.deps.Repository.ReadTenant(ctx, tracker.OrganizationID(recipient.organization), corestore.Serving, func(r tenantstore.Reader) error {
		reader, ok := r.(healthstore.Reader)
		if !ok {
			return errors.New("daily summary health owner is unavailable")
		}
		allProjects, err := r.Projects(ctx, "", 101)
		if err != nil {
			return err
		}
		findings, err := reader.HealthSummaryFindings(ctx, 100)
		if err != nil {
			return err
		}
		for _, finding := range findings {
			if len(finding.Projects) == 0 && len(allProjects) != len(projects) || slices.ContainsFunc(finding.Projects, func(id tracker.ProjectID) bool { return !slices.Contains(projects, string(id)) }) {
				continue
			}
			link := base + s.deps.HostedPath("/fleet")
			if finding.SubjectKind == "work_item" {
				item, err := r.WorkItemByNativeID(ctx, tracker.NativeWorkItemID(finding.SubjectID))
				if err != nil {
					return err
				}
				if !slices.Contains(projects, string(item.ProjectID)) {
					continue
				}
				project, err := r.Project(ctx, item.ProjectID)
				if err != nil {
					return err
				}
				link = base + s.deps.IssuePath(orgSlug, project.Slug, int(item.Number))
			}
			out = append(out, summaryEntry{SummaryRow: emailtemplate.SummaryRow{Title: s.deps.RenderText(finding.Signal), Detail: s.deps.RenderText(finding.Summary), URL: link}, at: finding.OpenedAt})
		}
		return nil
	})
	return out, err
}
