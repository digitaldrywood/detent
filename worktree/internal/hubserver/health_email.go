package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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

func healthEmailText(value string) string {
	return boundedFailureText(healthEmailPrivateText.ReplaceAllStringFunc(failureFirstLine(value), func(match string) string {
		if healthEmailSubjectID.MatchString(match) {
			return match
		}
		return "[redacted]"
	}), maxFailureExampleLength)
}

func (s *Service) commitHealthEvaluation(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, now time.Time, findings []healthFinding) error {
	transitions, err := writeHealthEvaluation(ctx, tx, organization, now, findings)
	if err != nil {
		return err
	}
	issues, err := reportHealthFindings(ctx, tx, organization, now)
	if err != nil {
		return err
	}
	notifications, err := s.claimHealthEmails(ctx, tx, organization, transitions)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if s.outbox != nil {
		s.outbox.signal()
	}
	for _, issue := range issues {
		raw, err := json.Marshal(issue)
		if err != nil {
			return err
		}
		s.wakeSpriteRunnersAfter(healthIssueScope(organization, issue.ProjectID), raw)
	}
	if len(notifications) == 0 {
		return nil
	}
	return s.sendHealthEmails(ctx, organization, notifications)
}

func (s *Service) claimHealthEmails(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, transitions []string) ([]healthFinding, error) {
	if len(transitions) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(transitions)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,signal,subject_json,opened_at,resolved_at,summary,next_action,evidence_json FROM health_findings
WHERE id IN (SELECT value FROM json_each(?)) AND organization_id=? AND severity='attention' AND
((resolved_at IS NULL AND email_open_attempted=0) OR (resolved_at IS NOT NULL AND email_resolve_attempted=0))
ORDER BY rowid LIMIT ?`, string(raw), organization, healthReadLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	findings := []healthFinding{}
	for rows.Next() {
		var f healthFinding
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
		if len(findings) > healthReadLimit {
			return nil, errHealthReadLimit
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	unavailable := s.config.Hosted == nil || s.config.Hosted.EmailSender == nil
	for _, f := range findings {
		query := `UPDATE health_findings SET email_open_attempted=1,email_unavailable=MAX(email_unavailable,?) WHERE organization_id=? AND id=?`
		if f.ResolvedAt != nil {
			query = `UPDATE health_findings SET email_resolve_attempted=1,email_unavailable=MAX(email_unavailable,?) WHERE organization_id=? AND id=?`
		}
		if _, err := tx.ExecContext(ctx, query, unavailable, organization, f.ID); err != nil {
			return nil, err
		}
	}
	if unavailable {
		return nil, nil
	}
	return findings, nil
}

func (s *Service) sendHealthEmails(ctx context.Context, organization tracker.OrganizationID, findings []healthFinding) error {
	if string(organization) != s.config.Hosted.OrganizationID {
		return auth.ErrHostedIdentity
	}
	providerID, err := s.hostedProviderOrganization(ctx)
	if err != nil {
		return err
	}
	members, err := s.config.Hosted.Provider.Memberships(ctx, "", providerID)
	if err != nil {
		return err
	}
	recipients := []string{}
	for _, member := range members {
		if member.OrganizationID != providerID || member.Status != "active" || member.Role.Slug != "owner" {
			continue
		}
		user, err := auth.LookupHostedUser(ctx, s.config.Hosted.Provider, member.UserID)
		if err != nil {
			return err
		}
		recipients = append(recipients, strings.ToLower(strings.TrimSpace(user.Email)))
	}
	slices.Sort(recipients)
	recipients = slices.Compact(recipients)
	var failures error
	for _, f := range findings {
		message, err := s.renderHealthEmail(f)
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		for _, recipient := range recipients {
			message.To = recipient
			if err := s.config.Hosted.EmailSender.SendEmail(ctx, message); err != nil {
				failures = errors.Join(failures, fmt.Errorf("send health finding %s email: %w", f.ID, err))
			}
		}
	}
	return failures
}

func (s *Service) renderHealthEmail(f healthFinding) (auth.EmailMessage, error) {
	type count struct {
		Name  string
		Value int
	}
	counts := make([]count, 0, len(f.Evidence.Counts))
	for name, value := range f.Evidence.Counts {
		counts = append(counts, count{healthEmailText(name), value})
	}
	slices.SortFunc(counts, func(a, b count) int { return strings.Compare(a.Name, b.Name) })
	state, resolved := "opened", ""
	if f.ResolvedAt != nil {
		state, resolved = "resolved", formatHubTime(*f.ResolvedAt)
	}
	path := "/fleet"
	if f.Subject.Kind == "work_item" {
		path = "/work/i/" + url.PathEscape(f.Subject.ID)
	}
	data := struct {
		State, Signal, Subject, Summary, NextAction, OpenedAt, ResolvedAt, URL string
		Events, Attempts                                                       int
		Counts                                                                 []count
	}{
		State:      state,
		Signal:     healthEmailText(f.Signal),
		Subject:    healthEmailText(f.Subject.Kind + " " + f.Subject.ID),
		Summary:    healthEmailText(f.Summary),
		NextAction: healthEmailText(f.NextAction),
		OpenedAt:   formatHubTime(f.OpenedAt),
		ResolvedAt: resolved,
		URL:        s.config.Hosted.PublicURL + s.hostedPath(path),
		Events:     len(f.Evidence.EventIDs),
		Attempts:   len(f.Evidence.AttemptIDs),
		Counts:     counts,
	}
	var body bytes.Buffer
	if err := healthEmailTemplate.Execute(&body, data); err != nil {
		return auth.EmailMessage{}, err
	}
	return auth.EmailMessage{Subject: "[Detent] " + data.Signal + ": " + data.Subject, Body: body.String()}, nil
}
