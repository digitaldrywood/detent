package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var slackMessageTemplate = template.Must(template.New("health-slack").Parse(`*{{.Signal}}* · {{.State}}
<{{.URL}}|{{.Subject}}>
{{.Summary}}
Next action: {{.NextAction}}
{{.Age}}`))
var slackPrivateText = regexp.MustCompile(`(?i)(?s:-----BEGIN (?:[A-Z]+ )?PRIVATE KEY-----.*?-----END (?:[A-Z]+ )?PRIVATE KEY-----)|\b(?:AKIA|ASIA)[A-Z0-9]{16}\b|https?://[^\s<>]+|\b(?:xox[a-z]-|sk-|gh[pousr]_|github_pat_|glpat-|AIza|detent_|det_)[a-z0-9_-]+|\bBearer\s+[^\s,;]+|\b[a-z0-9_]*(?:token|secret|password|api_key)[a-z0-9_]*\s*[=:]\s*[^\s,;]+|\beyJ[a-z0-9_-]+\.[a-z0-9_-]+\.[a-z0-9_-]+`)

func slackText(value string) string {
	value = slackPrivateText.ReplaceAllString(value, "[redacted]")
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	value = string(runes[:min(len(runes), 500)])
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "|", "¦", "*", "", "`", "").Replace(value)
}

func renderSlackFinding(publicURL string, basePath string, finding healthFinding, event string, at time.Time, issuePath string) (string, error) {
	base, err := url.Parse(publicURL)
	if err != nil || base.Scheme != "https" && base.Scheme != "http" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", errors.New("slack app link is unavailable")
	}
	path := "/diagnostics"
	safeSubject := !slackPrivateText.MatchString(finding.Subject.ID)
	switch finding.Subject.Kind {
	case "work_item":
		if safeSubject {
			if issuePath != "" {
				path = issuePath + "?tab=diagnostics"
				basePath = ""
			}
		}
	case "runner", "machine":
		path = "/fleet"
	}
	root := strings.TrimRight(base.String(), "/")
	if basePath != "" && strings.HasSuffix(strings.TrimRight(base.Path, "/"), basePath) {
		basePath = ""
	}
	link := root + basePath + path
	state, age := "Opened", "Age: "+max(at.Sub(finding.OpenedAt), 0).Round(time.Second).String()
	if event == "resolved" {
		state, age = "Resolved", "Duration: "+max(at.Sub(finding.OpenedAt), 0).Round(time.Second).String()
	}
	var body bytes.Buffer
	err = slackMessageTemplate.Execute(&body, struct{ Signal, State, URL, Subject, Summary, NextAction, Age string }{
		slackText(finding.Signal), state, link, slackText(finding.Subject.Kind + " " + finding.Subject.ID), slackText(finding.Summary), slackText(finding.NextAction), age,
	})
	return body.String(), err
}

func (s *Service) readSlackWebhook(ctx context.Context, organization tracker.OrganizationID, actor string) ([]byte, error) {
	readCtx, cancelRead := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancelRead()
	var secret accessstore.Secret
	err := s.accessRead(readCtx, organization, func(r tenantstore.Reader) error {
		owner, ok := r.(postgres.SlackReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		settings, err := owner.SlackIntegration(readCtx)
		if err != nil {
			return err
		}
		if !settings.Enabled {
			return corestore.ErrNotFound
		}
		secret, err = r.AccessSecret(readCtx, "", slackWebhookSecret)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if s.config.SecretKeys == nil {
		return nil, errors.New("Slack secret storage is unavailable")
	}
	value, err := s.config.SecretKeys.Open(secret.Envelope, accessstore.SecretAAD(organization, "", slackWebhookSecret))
	if err != nil {
		return nil, err
	}
	err = s.accessWrite(ctx, organization, func(w tenantstore.Writer) error {
		return w.RecordAccessSecretAudit(ctx, accessstore.SecretAudit{OrganizationID: organization, Actor: actor, Kind: slackWebhookSecret, Event: "use", KeyVersion: secret.Envelope.Version, RecordedAt: s.config.now()})
	})
	if err != nil {
		clear(value)
		return nil, err
	}
	return value, nil
}

func (s *Service) postSlack(ctx context.Context, webhook []byte, text string) (int, bool) {
	if !validSlackWebhook(string(webhook)) {
		return 0, false
	}
	body, err := json.Marshal(struct {
		Text        string `json:"text"`
		UnfurlLinks bool   `json:"unfurl_links"`
		UnfurlMedia bool   `json:"unfurl_media"`
	}{Text: text})
	if err != nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, string(webhook), bytes.NewReader(body))
	if err != nil {
		return 0, false
	}
	request.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 10 * time.Second}
	if s.config.SlackHTTPClient != nil {
		client = *s.config.SlackHTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return 0, false
	}
	defer response.Body.Close()
	acknowledgment, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	defer clear(acknowledgment)
	return response.StatusCode, err == nil && len(acknowledgment) <= 4096 && response.StatusCode >= 200 && response.StatusCode < 300 && strings.TrimSpace(string(acknowledgment)) == "ok"
}
