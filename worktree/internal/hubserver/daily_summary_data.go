package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/emailtemplate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type summaryProjectRef struct{ id, name string }

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

func readSummaryCosts(ctx context.Context, q nativeQueryer, scope nativeScope, window usageWindow) (summaryCosts, error) {
	out := summaryCosts{}
	closed, err := readNativeClosedPopulation(ctx, q, scope, "", nil, window.From, window.To)
	if err != nil {
		return out, err
	}
	out.shipped = closed.Shipped
	out.spend, _, err = readProjectSpend(ctx, q, scope, window.From, window.To)
	if err != nil {
		return out, err
	}
	out.shippedSpend, out.perIssue, err = readNativeShippedSpend(ctx, q, scope, closed.shippedIDs(), window.To)
	if err != nil {
		return out, err
	}
	usage, err := readUsageRows(ctx, q, string(scope.organization), window, []string{string(scope.project)}, maxAnalyticsPopulation)
	if err != nil {
		return out, err
	}
	population, err := loadNativeAnalyticsAttempts(ctx, q, scope, operatortool.AnalyticsWindow{From: window.From, To: window.To}, "")
	if err != nil {
		return out, err
	}
	byAttempt := map[string][]usageRow{}
	for _, u := range usage {
		if !u.Period.Before(window.To) {
			continue
		}
		out.tokens += u.tokens()
		byAttempt[u.AttemptID] = append(byAttempt[u.AttemptID], u)
	}
	inputs := reportsInputs{done: reportsDoneFrom(out.shipped), lanes: func(time.Time, time.Time) map[string]float64 { return nil }, timing: func([]string, time.Time) (reportsCompletion, error) { return reportsCompletion{}, nil }}
	series, err := reportsSeries(inputs, population.Items, byAttempt, operatortool.AnalyticsWindow{From: window.From, To: window.To})
	for _, bucket := range series {
		out.notShipped += bucket.NotShippedCost
	}
	return out, err
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
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, false, err
	}
	defer tx.Rollback()
	var slug string
	if err := tx.QueryRowContext(ctx, "SELECT name,slug FROM organizations WHERE id=?", recipient.organization).Scan(&out.Organization, &slug); err != nil {
		return out, false, err
	}
	base := strings.TrimRight(s.config.Hosted.PublicURL, "/")
	out.BoardURL = base + "/" + url.PathEscape(slug) + "/work"
	out.CostURL = base + "/" + url.PathEscape(slug) + "/reports/cost?project=all&from=" + start.UTC().Format("2006-01-02") + "&to=" + recipient.end.Add(-time.Nanosecond).UTC().Format("2006-01-02")
	out.SettingsURL = base + "/" + url.PathEscape(slug) + "/settings/general#settings-general-daily-summary"
	out.UnsubscribeURL = base + s.hostedPath("/daily-summary/unsubscribe?token="+url.QueryEscape(recipient.token))
	out.MoreURL = out.BoardURL + "?view=list&tab=closed&state=Done&sort=closed&completed_from=" + url.QueryEscape(start.UTC().Format(time.RFC3339)) + "&completed_to=" + url.QueryEscape(recipient.end.UTC().Format(time.RFC3339))
	rows, err := tx.QueryContext(ctx, `SELECT p.id,p.name FROM hosted_project_grants g JOIN projects p ON p.id=g.project_id AND p.organization_id=g.organization_id WHERE g.user_id=? AND g.organization_id=? AND p.profile='native' ORDER BY p.id LIMIT 101`, recipient.user, recipient.organization)
	if err != nil {
		return out, false, err
	}
	defer rows.Close()
	projects := []summaryProjectRef{}
	for rows.Next() {
		var p summaryProjectRef
		if err := rows.Scan(&p.id, &p.name); err != nil {
			return out, false, err
		}
		projects = append(projects, p)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return out, false, err
	}
	if len(projects) > 100 {
		return out, false, errors.New("daily summary project limit exceeded")
	}
	var total, average summaryCosts
	var averageShipped int
	needs, watch, shipped := []summaryEntry{}, []summaryEntry{}, []summaryEntry{}
	for _, p := range projects {
		scope := nativeScope{organization: tracker.OrganizationID(recipient.organization), project: tracker.ProjectID(p.id)}
		projectCtx := ctx
		if recipient.role == "reporter" {
			filter := reportsScope{issues: "SELECT native_id FROM issues WHERE organization_id=? AND project_id=? AND actor_principal_id=?", issueArgs: []any{recipient.organization, p.id, recipient.principal}, reporterPrincipal: recipient.principal}
			projectCtx = context.WithValue(ctx, reportsScopeKey{}, filter)
		}
		cost, err := readSummaryCosts(projectCtx, tx, scope, usageWindow{From: start, To: recipient.end})
		if err != nil {
			return out, false, err
		}
		prior, err := readSummaryCosts(projectCtx, tx, scope, usageWindow{From: start.AddDate(0, 0, -7), To: start})
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
			if err := tx.QueryRowContext(ctx, "SELECT title,number FROM issues WHERE native_id=?", closed.ID).Scan(&title, &number); err != nil {
				return out, false, err
			}
			path, err := readIssueBrowserPath(ctx, tx, closed.ID)
			if err != nil {
				return out, false, err
			}
			detail := p.name
			if out.Cost {
				detail += " · " + summaryMoney(cost.perIssue[closed.ID])
			}
			shipped = append(shipped, summaryEntry{SummaryRow: emailtemplate.SummaryRow{Title: fmt.Sprintf("#%d %s", number, title), Detail: detail, URL: base + path}, at: closed.ClosedAt})
			slices.SortFunc(shipped, func(a, b summaryEntry) int { return b.at.Compare(a.at) })
			shipped = shipped[:min(5, len(shipped))]
		}
		n, w, err := readSummaryAttention(projectCtx, tx, recipient, p.id, p.name, start, base, out.Cost)
		if err != nil {
			return out, false, err
		}
		needs = append(needs, n...)
		watch = append(watch, w...)
	}
	if recipient.role == "owner" {
		health, err := s.readSummaryHealth(ctx, tx, recipient, projectsToIDs(projects), base)
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
	if err := tx.Commit(); err != nil {
		return out, false, err
	}
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

func readSummaryAttention(ctx context.Context, q nativeQueryer, r summaryRecipient, project, name string, start time.Time, base string, cost bool) ([]summaryEntry, []summaryEntry, error) {
	condition, args := reportsIssueClause(ctx, "i.native_id")
	rows, err := q.QueryContext(ctx, `WITH candidates AS (
SELECT i.native_id AS id,i.number,i.title,ws.detent_state AS state,coalesce(i.verification_state,'') AS verification,coalesce(i.actor_principal_id,'') AS actor,
coalesce((SELECT e.recorded_at FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned' AND e.from_state<>e.to_state ORDER BY e.sequence DESC LIMIT 1),i.native_created_at) AS at,
coalesce((SELECT a.disposition_human_action FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.project_id=i.project_id AND a.work_item_id=i.native_id ORDER BY a.fencing_token DESC LIMIT 1),0) AS human,
(SELECT count(*) FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.project_id=i.project_id AND a.work_item_id=i.native_id) AS attempts,
(SELECT count(*) FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned' AND e.to_state='Rework' AND e.analytics_recorded_ns>=? AND e.analytics_recorded_ns<?) AS reworks
FROM issues i JOIN workflow_states ws ON ws.id=i.workflow_state_id WHERE i.organization_id=? AND i.project_id=? AND i.archived=0
AND (ws.detent_state IN ('Blocked','Human Review','In Progress') OR (ws.detent_state='Done' AND i.verification_state='pending' AND i.actor_principal_id=?) OR EXISTS (SELECT 1 FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned' AND e.to_state='Rework' AND e.analytics_recorded_ns>=? AND e.analytics_recorded_ns<? LIMIT 1 OFFSET 1))`+condition+`), qualified AS (
SELECT *,CASE WHEN state='Blocked' AND human THEN 2 WHEN state='Human Review' OR (state='Done' AND verification='pending' AND actor=?) THEN 1 ELSE 0 END AS needs_priority,
CASE WHEN state='In Progress' AND (julianday(at)<julianday(?) OR attempts>=3) THEN CASE WHEN attempts>=3 THEN 2 ELSE 0 END WHEN reworks>=2 THEN 1 ELSE -1 END AS watch_priority FROM candidates
), needs AS (SELECT * FROM qualified WHERE needs_priority>0 ORDER BY needs_priority DESC,julianday(at),number LIMIT 100),
watch AS (SELECT * FROM qualified WHERE watch_priority>=0 ORDER BY watch_priority DESC,julianday(at),number LIMIT 5)
SELECT id,number,title,state,verification,actor,at,human,attempts,reworks FROM (SELECT * FROM needs UNION SELECT * FROM watch) ORDER BY needs_priority DESC,julianday(at),number`, append(append([]any{start.UnixNano(), r.end.UnixNano(), r.organization, project, r.principal, start.UnixNano(), r.end.UnixNano()}, args...), r.principal, formatHubTime(r.end.Add(-24*time.Hour)))...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	type candidate struct {
		id, title, state, verification, actor string
		number, attempts, reworks             int
		at                                    time.Time
		human                                 bool
	}
	candidates := []candidate{}
	for rows.Next() {
		var c candidate
		var at string
		if err := rows.Scan(&c.id, &c.number, &c.title, &c.state, &c.verification, &c.actor, &at, &c.human, &c.attempts, &c.reworks); err != nil {
			return nil, nil, err
		}
		c.at, err = parseTimeValue(at)
		if err != nil {
			return nil, nil, err
		}
		candidates = append(candidates, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}
	needs, watch := []summaryEntry{}, []summaryEntry{}
	for _, c := range candidates {
		path, err := readIssueBrowserPath(ctx, q, c.id)
		if err != nil {
			return nil, nil, err
		}
		row := summaryEntry{SummaryRow: emailtemplate.SummaryRow{Title: fmt.Sprintf("#%d %s", c.number, c.title), URL: base + path}, at: c.at}
		wait := fmt.Sprintf("%gh", max(0, r.end.Sub(c.at).Truncate(time.Hour).Hours()))
		switch {
		case c.state == "Blocked" && c.human:
			row.Detail = "Blocked on your answer · asked " + wait + " ago · " + name
			row.blocked = true
			row.Alert = true
			needs = append(needs, row)
		case c.state == "Human Review":
			row.Detail = "Waiting in Human Review for " + wait + " · " + name
			needs = append(needs, row)
		case c.state == "Done" && c.verification == "pending" && c.actor == r.principal:
			row.Detail = "Done · please verify · " + name
			needs = append(needs, row)
		}
		if c.state == "In Progress" && (r.end.Sub(c.at) > 24*time.Hour || c.attempts >= 3) {
			if c.attempts >= 3 {
				row.priority = 2
			}
			row.Detail = fmt.Sprintf("In progress %s · %d attempts", wait, c.attempts)
			if cost {
				var spent float64
				if err := q.QueryRowContext(ctx, `SELECT coalesce(sum(u.cost_estimate),0) FROM native_attempts a JOIN attempt_usage u ON u.attempt_id=a.id WHERE a.organization_id=? AND a.project_id=? AND a.work_item_id=? AND u.period<?`, r.organization, project, c.id, r.end.UTC().Format(usagePeriodLayout)).Scan(&spent); err != nil {
					return nil, nil, err
				}
				row.Detail += " · " + summaryMoney(spent) + " so far"
			}
			watch = append(watch, row)
		} else if c.reworks >= 2 {
			row.priority = 1
			row.Detail = fmt.Sprintf("Back to Rework %d times in %gh", c.reworks, r.end.Sub(start).Hours())
			watch = append(watch, row)
		}
	}
	return needs, watch, nil
}

func (s *Service) readSummaryHealth(ctx context.Context, q nativeQueryer, r summaryRecipient, projects []string, base string) ([]summaryEntry, error) {
	var totalProjects int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM projects WHERE organization_id=?", r.organization).Scan(&totalProjects); err != nil {
		return nil, err
	}

	rows, err := q.QueryContext(ctx, `SELECT signal,summary,opened_at,projects_json,subject_json FROM health_findings WHERE organization_id=? AND resolved_at IS NULL AND severity='attention' ORDER BY julianday(opened_at),id LIMIT 100`, r.organization)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []summaryEntry{}
	for rows.Next() {
		var signal, summary, opened, raw, subjectRaw string
		if err := rows.Scan(&signal, &summary, &opened, &raw, &subjectRaw); err != nil {
			return nil, err
		}
		var ids []string
		if err := json.Unmarshal([]byte(raw), &ids); err != nil {
			return nil, err
		}
		if len(ids) == 0 && totalProjects != len(projects) || slices.ContainsFunc(ids, func(id string) bool { return !slices.Contains(projects, id) }) {
			continue
		}
		var subject healthSubject
		if err := json.Unmarshal([]byte(subjectRaw), &subject); err != nil {
			return nil, err
		}
		at, err := parseTimeValue(opened)
		if err != nil {
			return nil, err
		}
		link := base + s.hostedPath("/fleet")
		if subject.Kind == "work_item" {
			path, err := readIssueBrowserPath(ctx, q, subject.ID)
			if err != nil {
				return nil, err
			}
			link = base + path
		}
		out = append(out, summaryEntry{SummaryRow: emailtemplate.SummaryRow{Title: healthEmailText(signal), Detail: healthEmailText(summary), URL: link}, at: at})
	}
	return out, rows.Err()
}
