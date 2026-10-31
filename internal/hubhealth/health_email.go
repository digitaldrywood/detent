package hubhealth

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/emailtemplate"
	"github.com/digitaldrywood/detent/internal/hubsql"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Store) CommitEvaluation(ctx context.Context, organization tracker.OrganizationID, now time.Time, findings []Finding, unavailable []BaselineUnavailable, emailUnavailable bool) error {
	cancellations, err := readCancellations(ctx, s.reader, organization)
	if err != nil {
		return err
	}
	if unavailable == nil {
		unavailable = []BaselineUnavailable{}
	}
	raw, err := json.Marshal(unavailable)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO health_detector_ticks(organization_id,last_tick_at,baseline_unavailable_json) VALUES(?,?,?)
 ON CONFLICT(organization_id) DO UPDATE SET baseline_unavailable_json=excluded.baseline_unavailable_json`, organization, formatHubTime(now), string(raw)); err != nil {
		return err
	}
	transitions, err := s.WriteEvaluation(ctx, tx, organization, now, findings)
	if err != nil {
		return err
	}
	issues, err := s.ReportFindings(ctx, tx, organization, now, cancellations)
	if err != nil {
		return err
	}
	notifications, err := s.ClaimEmails(ctx, tx, organization, transitions, emailUnavailable)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := s.hooks.AfterCommit(ctx, organization, issues); err != nil {
		return err
	}

	if len(notifications) == 0 {
		return nil
	}
	return s.hooks.SendEmails(ctx, organization, notifications)
}

var healthEmailTemplate = template.Must(template.New("health").Parse(`Finding {{.State}}: {{.Signal}}
Subject: {{.Subject}}

Summary: {{.Summary}}
Next action: {{.NextAction}}
Opened: {{.OpenedAt}}
{{if .ResolvedAt}}Resolved: {{.ResolvedAt}}
{{end}}Evidence counts:
Events: {{.Events}}
Attempts: {{.Attempts}}
{{range .Counts}}{{.Name}}: {{.Value}}
{{end}}
Open subject: {{.URL}}
`))

var healthEmailPrivateText = regexp.MustCompile(`(?i)\bauthorization\s*[:=]\s*(?:(?:bearer|basic)\s+)?[^\s,;]+|\b[a-z0-9_]*(?:token|secret|password|api[_-]?key)[a-z0-9_]*\s*[=:]\s*(?:"[^"]*"|'[^']*'|[^\s,;]+)|\b(?:bearer|basic)\s+[^\s,;]+|\b(?:gh[pousr]_|github_pat_|sk[-_]|AKIA|ASIA)[a-z0-9_-]+|\beyJ[a-z0-9_-]+\.[a-z0-9_-]+\.[a-z0-9_-]+|(?:https?://|file://)[^\s]+|\b[a-z0-9_+/=-]{32,}\b`)

var healthEmailSubjectID = regexp.MustCompile(`^(?:wi|prj|org|runner|machine)_[0-9a-f]{32}$`)

func EmailText(value string) string {
	return BoundedFailureText(healthEmailPrivateText.ReplaceAllStringFunc(FailureFirstLine(value), func(match string) string {
		if healthEmailSubjectID.MatchString(match) {
			return match
		}
		return "[redacted]"
	}), emailExampleLimit)
}

func (s *Store) ClaimEmails(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, transitions []string, unavailable bool) ([]Finding, error) {
	if len(transitions) == 0 {
		return nil, nil
	}
	raw, err := hubsql.List(transitions)
	if err != nil {
		return nil, err
	}
	rows, err := hubsql.Query(ctx, tx, `SELECT id,signal,subject_json,opened_at,resolved_at,summary,next_action,evidence_json FROM health_findings
WHERE id IN (SELECT value FROM sql_rows(?)) AND organization_id=? AND severity='attention' AND
((resolved_at IS NULL AND email_open_attempted=0) OR (resolved_at IS NOT NULL AND email_resolve_attempted=0))
ORDER BY rowid LIMIT ?`, raw, organization, ReadLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	findings := []Finding{}
	for rows.Next() {
		var f Finding
		var subject, opened, evidence string
		var resolved sql.NullString
		if err := rows.Scan(&f.ID, &f.Signal, &subject, &opened, &resolved, &f.Summary, &f.NextAction, &evidence); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(subject), &f.Subject); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(evidence), &f.Evidence); err != nil {
			return nil, err
		}
		if f.OpenedAt, err = parseTimeValue(opened); err != nil {
			return nil, err
		}
		if resolved.Valid {
			at, err := parseTimeValue(resolved.String)
			if err != nil {
				return nil, err
			}
			f.ResolvedAt = &at
		}
		findings = append(findings, f)
		if len(findings) > ReadLimit {
			return nil, ErrReadLimit
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for _, f := range findings {
		query := `UPDATE health_findings SET email_open_attempted=1,email_unavailable=MAX(email_unavailable,?) WHERE organization_id=? AND id=?`
		if f.ResolvedAt != nil {
			query = `UPDATE health_findings SET email_resolve_attempted=1,email_unavailable=MAX(email_unavailable,?) WHERE organization_id=? AND id=?`
		}
		if _, err := hubsql.Exec(ctx, tx, query, unavailable, organization, f.ID); err != nil {
			return nil, err
		}
	}
	if unavailable {
		return nil, nil
	}
	return findings, nil
}

const emailExampleLimit = 512

func RenderEmail(f Finding, link string) (auth.EmailMessage, error) {
	type count struct {
		Name  string
		Value int
	}
	counts := make([]count, 0, len(f.Evidence.Counts))
	for name, value := range f.Evidence.Counts {
		counts = append(counts, count{EmailText(name), value})
	}
	slices.SortFunc(counts, func(a, b count) int { return strings.Compare(a.Name, b.Name) })
	state, resolved := "opened", ""
	if f.ResolvedAt != nil {
		state, resolved = "resolved", f.ResolvedAt.UTC().Format("Jan 2, 2006 15:04 UTC")
	}
	data := struct {
		State, Signal, Subject, Summary, NextAction, OpenedAt, ResolvedAt, URL string
		Events, Attempts                                                       int
		Counts                                                                 []count
	}{
		State:      state,
		Signal:     EmailText(f.Signal),
		Subject:    EmailText(f.Subject.Kind + " " + f.Subject.ID),
		Summary:    EmailText(f.Summary),
		NextAction: EmailText(f.NextAction),
		OpenedAt:   f.OpenedAt.UTC().Format("Jan 2, 2006 15:04 UTC"),
		ResolvedAt: resolved,
		URL:        link,
		Events:     len(f.Evidence.EventIDs),
		Attempts:   len(f.Evidence.AttemptIDs),
		Counts:     counts,
	}
	var body bytes.Buffer
	if err := healthEmailTemplate.Execute(&body, data); err != nil {
		return auth.EmailMessage{}, err
	}
	details := []emailtemplate.Detail{{Label: "Opened", Value: data.OpenedAt}}
	if data.ResolvedAt != "" {
		details = append(details, emailtemplate.Detail{Label: "Resolved", Value: data.ResolvedAt})
	}
	evidence := []emailtemplate.Detail{
		{Label: "Events", Value: strconv.Itoa(data.Events)},
		{Label: "Attempts", Value: strconv.Itoa(data.Attempts)},
	}
	for _, count := range data.Counts {
		evidence = append(evidence, emailtemplate.Detail{Label: count.Name, Value: strconv.Itoa(count.Value)})
	}
	message, err := emailtemplate.Render(emailtemplate.Content{
		Title:       "Health finding",
		Status:      &emailtemplate.Status{Label: "Finding " + data.State, Resolved: f.ResolvedAt != nil},
		Heading:     data.Signal,
		Subtitle:    data.Subject,
		Summary:     data.Summary,
		NextAction:  data.NextAction,
		Details:     details,
		Evidence:    evidence,
		ActionLabel: "Open " + strings.ReplaceAll(EmailText(f.Subject.Kind), "_", " "),
		ActionURL:   data.URL,
		Footer:      "You get this because you own this Detent organization.",
	})
	if err != nil {
		return auth.EmailMessage{}, err
	}
	message.Subject = "[Detent] " + data.Signal + ": " + data.Subject
	message.Body = body.String()
	return message, nil
}

func LogText(value string) string {
	return healthEmailPrivateText.ReplaceAllStringFunc(strings.Join(strings.Fields(value), " "), func(match string) string {
		if healthEmailSubjectID.MatchString(match) || uuid.Validate(match) == nil {
			return match
		}
		return "[redacted]"
	})
}
