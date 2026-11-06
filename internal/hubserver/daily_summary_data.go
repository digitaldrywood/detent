package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/emailtemplate"
	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type summaryProjectRef struct{ id, name, slug string }

type summaryRecipient struct {
	user, organization, email, principal, role, token string
	eligible                                          bool
	minute                                            int
	zone                                              *time.Location
	weekdays                                          bool
	end                                               time.Time
}

type summaryCosts struct {
	spend, shippedSpend, notShipped float64
	tokens                          int64
	shipped                         []nativeShippedIssue
	perIssue                        map[string]float64
}

type summaryEntry struct {
	emailtemplate.SummaryRow
	at       time.Time
	blocked  bool
	priority int
}

func (s *Service) buildDailySummary(ctx context.Context, recipient summaryRecipient) (emailtemplate.DailySummary, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	out := emailtemplate.DailySummary{Organization: "", Date: recipient.end.In(recipient.zone).Format("Mon Jan 2"), Cost: recipient.role != "reporter"}
	start := summarySchedule(recipient.end, recipient.minute, recipient.zone, recipient.weekdays, -1)
	out.Window = fmt.Sprintf("Last %g hours, %s to %s (%s).", recipient.end.Sub(start).Hours(), start.In(recipient.zone).Format("3:04 PM Mon Jan 2"), recipient.end.In(recipient.zone).Format("3:04 PM Mon Jan 2"), recipient.zone.String())
	if s.config.TenantRepository == nil {
		return out, false, workItemStorageUnavailable()
	}
	var slug string
	projects := []summaryProjectRef{}
	err := s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(recipient.organization), corestore.Serving, func(r tenantstore.Reader) error {
		organization, err := r.AccessOrganization(ctx)
		if err != nil {
			return err
		}
		out.Organization, slug = organization.Name, organization.Slug
		owner, ok := r.(accountstore.SummaryProjectReader)
		if !ok {
			return errors.New("daily summary project owner is unavailable")
		}
		granted, err := owner.SummaryProjects(ctx, recipient.user, 101)
		if err != nil {
			return err
		}
		if len(granted) > 100 {
			return errors.New("daily summary project limit exceeded")
		}
		for _, project := range granted {
			projects = append(projects, summaryProjectRef{id: string(project.ID), name: project.Name, slug: project.Slug})
		}
		return nil
	})
	if err != nil {
		return out, false, err
	}
	base := strings.TrimRight(s.config.Hosted.PublicURL, "/")
	out.BoardURL = base + "/" + url.PathEscape(slug) + "/work"
	out.CostURL = base + "/" + url.PathEscape(slug) + "/reports/cost?project=all&from=" + start.UTC().Format("2006-01-02") + "&to=" + recipient.end.Add(-time.Nanosecond).UTC().Format("2006-01-02")
	out.SettingsURL = base + "/" + url.PathEscape(slug) + "/settings/general#settings-general-daily-summary"
	out.UnsubscribeURL = base + s.hostedPath("/daily-summary/unsubscribe?token="+url.QueryEscape(recipient.token))
	out.MoreURL = out.BoardURL + "?view=list&tab=closed&state=Done&sort=closed&completed_from=" + url.QueryEscape(start.UTC().Format(time.RFC3339)) + "&completed_to=" + url.QueryEscape(recipient.end.UTC().Format(time.RFC3339))
	var total, average summaryCosts
	var averageShipped int
	needs, watch, shipped := []summaryEntry{}, []summaryEntry{}, []summaryEntry{}
	for _, p := range projects {
		scope := nativeScope{organization: tracker.OrganizationID(recipient.organization), project: tracker.ProjectID(p.id)}
		principal := ""
		if recipient.role == "reporter" {
			principal = recipient.principal
		}
		cost, err := s.readSummaryCostsTenant(ctx, scope, principal, usageWindow{From: start, To: recipient.end})
		if err != nil {
			return out, false, err
		}
		prior, err := s.readSummaryCostsTenant(ctx, scope, principal, usageWindow{From: start.AddDate(0, 0, -7), To: start})
		if err != nil {
			return out, false, err
		}
		total.spend += cost.spend
		total.shippedSpend += cost.shippedSpend
		total.tokens += cost.tokens
		total.notShipped += cost.notShipped
		average.spend += prior.spend
		average.shippedSpend += prior.shippedSpend
		averageShipped += len(prior.shipped)
		out.ShippedCount += len(cost.shipped)
		perIssue := "—"
		if len(cost.shipped) > 0 {
			perIssue = summaryMoney(cost.shippedSpend / float64(len(cost.shipped)))
		}
		out.Projects = append(out.Projects, emailtemplate.SummaryProject{Name: p.name, Spend: summaryMoney(cost.spend), Shipped: len(cost.shipped), PerIssue: perIssue, NotShipped: summaryMoney(cost.notShipped), CostPerIssue: func() float64 {
			if len(cost.shipped) == 0 {
				return 0
			}
			return cost.shippedSpend / float64(len(cost.shipped))
		}()})
		for _, closed := range cost.shipped {
			if len(shipped) >= 5 && !closed.ClosedAt.After(shipped[len(shipped)-1].at) {
				continue
			}
			var title string
			var number int
			err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				item, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(closed.ID))
				if err != nil {
					return err
				}
				if principal != "" && stringValue(item.ActorPrincipalID) != principal {
					return corestore.ErrNotFound
				}
				title, number = item.Title, int(item.Number)
				return nil
			})
			if err != nil {
				return out, false, err
			}
			path := issueBrowserPath(slug, p.slug, number)
			detail := p.name
			if out.Cost {
				detail += " · " + summaryMoney(cost.perIssue[closed.ID])
			}
			shipped = append(shipped, summaryEntry{SummaryRow: emailtemplate.SummaryRow{Title: fmt.Sprintf("#%d %s", number, title), Detail: detail, URL: base + path}, at: closed.ClosedAt})
			slices.SortFunc(shipped, func(a, b summaryEntry) int { return b.at.Compare(a.at) })
			shipped = shipped[:min(5, len(shipped))]
		}
		n, w, err := s.readSummaryAttentionTenant(ctx, recipient, p, start, base, slug, out.Cost)
		if err != nil {
			return out, false, err
		}
		needs = append(needs, n...)
		watch = append(watch, w...)
	}
	if recipient.role == "owner" {
		health, err := s.readSummaryHealthTenant(ctx, recipient, projectsToIDs(projects), base, slug)
		if err != nil {
			return out, false, err
		}
		needs = append(needs, health...)
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
	for _, row := range needs {
		out.Needs = append(out.Needs, row.SummaryRow)
	}
	for _, row := range watch[:min(5, len(watch))] {
		out.Watch = append(out.Watch, row.SummaryRow)
	}
	for _, row := range shipped {
		out.Shipped = append(out.Shipped, row.SummaryRow)
	}
	out.More = out.ShippedCount - len(out.Shipped)
	perIssue := "—"
	if out.ShippedCount > 0 {
		perIssue = summaryMoney(total.shippedSpend / float64(out.ShippedCount))
	}
	share := 0.0
	if total.spend > 0 {
		share = 100 * total.notShipped / total.spend
	}
	out.Tiles = []emailtemplate.SummaryTile{{Label: "Shipped", Value: strconv.Itoa(out.ShippedCount), Positive: float64(out.ShippedCount) > float64(averageShipped)/7, Detail: fmt.Sprintf("%+.1f vs 7-day avg", float64(out.ShippedCount)-float64(averageShipped)/7)}, {Label: "Spend", Value: summaryMoney(total.spend), Detail: "7-day avg " + summaryMoney(average.spend/7)}, {Label: "Per shipped issue", Value: perIssue, Detail: summaryTokens(total.tokens) + " tokens total"}, {Label: "Did not ship", Value: summaryMoney(total.notShipped), Detail: fmt.Sprintf("%.0f%% of spend", share)}}
	for i := range out.Projects {
		if averageShipped > 0 {
			out.Projects[i].Highlight = out.Projects[i].CostPerIssue > average.shippedSpend/float64(averageShipped)
		}
	}
	out.Subject = out.Organization + " · " + out.Date
	clauses := []string{}
	if out.ShippedCount > 0 {
		clauses = append(clauses, fmt.Sprintf("%d shipped", out.ShippedCount))
	}
	if len(out.Needs) > 0 {
		clauses = append(clauses, fmt.Sprintf("%d need you", len(out.Needs)))
	}
	if out.Cost && total.spend > 0 {
		clauses = append(clauses, fmt.Sprintf("$%.0f spent", total.spend))
	}
	if len(clauses) > 0 {
		out.Subject += ": " + strings.Join(clauses, ", ")
	}
	out.Subject = strings.NewReplacer("\r", " ", "\n", " ").Replace(out.Subject)
	return out, out.ShippedCount == 0 && len(out.Needs) == 0 && total.spend == 0, nil
}

func projectsToIDs(projects []summaryProjectRef) []string {
	ids := make([]string, 0, len(projects))
	for _, p := range projects {
		ids = append(ids, p.id)
	}
	return ids
}

func summaryMoney(value float64) string { return fmt.Sprintf("$%.2f", value) }
func summaryTokens(value int64) string {
	if value >= 1000000 {
		return fmt.Sprintf("%.1fM", float64(value)/1000000)
	}
	if value >= 1000 {
		return fmt.Sprintf("%.1fK", float64(value)/1000)
	}
	return strconv.FormatInt(value, 10)
}
