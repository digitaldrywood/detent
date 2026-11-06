package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/emailtemplate"
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

func (s *Service) renderHealthEmail(ctx context.Context, f healthFinding) (auth.EmailMessage, error) {
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
		state, resolved = "resolved", f.ResolvedAt.UTC().Format("Jan 2, 2006 15:04 UTC")
	}
	link := s.config.Hosted.PublicURL + s.hostedPath("/fleet")
	if f.Subject.Kind == "work_item" {
		var err error
		link, err = s.issueBrowserURL(ctx, f.Subject.ID)
		if errors.Is(err, sql.ErrNoRows) {
			link = s.config.Hosted.PublicURL + s.hostedPath("/diagnostics")
		} else if err != nil {
			return auth.EmailMessage{}, err
		}
	}
	link, err := s.canonicalBrowserLink(ctx, link)
	if err != nil {
		return auth.EmailMessage{}, err
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
		ActionLabel: "Open " + strings.ReplaceAll(healthEmailText(f.Subject.Kind), "_", " "),
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
