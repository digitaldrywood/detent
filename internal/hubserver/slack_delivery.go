package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
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

func renderSlackFinding(publicURL string, basePath string, finding healthFinding, event string, at time.Time) (string, error) {
	base, err := url.Parse(publicURL)
	if err != nil || base.Scheme != "https" && base.Scheme != "http" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", errors.New("slack app link is unavailable")
	}
	path := "/diagnostics"
	safeSubject := !slackPrivateText.MatchString(finding.Subject.ID)
	switch finding.Subject.Kind {
	case "work_item":
		if safeSubject {
			path = "/work/i/" + url.PathEscape(finding.Subject.ID) + "?tab=diagnostics"
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

func enqueueSlackOpened(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, finding string, at time.Time) error {
	now := formatHubTime(at)
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO health_slack_deliveries(finding_id,event,queued_at,next_attempt_at) SELECT ?, 'opened', ?, ? WHERE EXISTS(SELECT 1 FROM organization_secrets WHERE organization_id=? AND kind=?)`, finding, now, now, organization, slackWebhookSecret)
	return err
}

func enqueueSlackResolved(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, fingerprints []byte, at time.Time) error {
	now := formatHubTime(at)
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO health_slack_deliveries(finding_id,event,queued_at,next_attempt_at)
 SELECT f.id,'resolved',?,? FROM health_findings f WHERE f.organization_id=? AND f.resolved_at IS NULL AND f.severity='attention' AND f.fingerprint NOT IN (SELECT value FROM json_each(?))
 AND EXISTS(SELECT 1 FROM organization_secrets WHERE organization_id=? AND kind=?) AND EXISTS(SELECT 1 FROM health_slack_deliveries d WHERE d.finding_id=f.id AND d.event='opened')`, now, now, organization, string(fingerprints), organization, slackWebhookSecret)
	return err
}

func (s *Service) readSlackWebhook(ctx context.Context, organization tracker.OrganizationID, actor string) ([]byte, error) {
	var envelope hubsecrets.Envelope
	err := s.database.db.QueryRowContext(ctx, `SELECT ciphertext,nonce,wrapped_data_key,master_key_version FROM organization_secrets WHERE organization_id=? AND kind=?`, organization, slackWebhookSecret).Scan(&envelope.Ciphertext, &envelope.Nonce, &envelope.WrappedKey, &envelope.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	value, err := s.config.SecretKeys.Open(envelope, secretAAD(string(organization), "", slackWebhookSecret))
	if err != nil {
		return nil, err
	}
	if err := secretAudit(ctx, s.database.db, string(organization), "", actor, slackWebhookSecret, "use", envelope.Version, formatHubTime(s.config.now())); err != nil {
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
	return response.StatusCode, response.StatusCode >= 200 && response.StatusCode < 300
}

func (s *Service) recordSlackResult(ctx context.Context, db hostedExecer, organization tracker.OrganizationID, finding string, status int, delivered bool, at time.Time) error {
	now := formatHubTime(at)
	if delivered {
		if _, err := db.ExecContext(ctx, `UPDATE slack_integrations SET last_success_at=?,last_status_code=?,last_delivery_failed=0 WHERE organization_id=?`, now, status, organization); err != nil {
			return err
		}
		if finding != "" {
			_, err := db.ExecContext(ctx, `UPDATE health_findings SET slack_unavailable_at=NULL,slack_status_code=NULL WHERE organization_id=? AND id=?`, organization, finding)
			return err
		}
		return nil
	}
	if _, err := db.ExecContext(ctx, `UPDATE slack_integrations SET last_failure_at=?,last_status_code=?,last_delivery_failed=1 WHERE organization_id=?`, now, status, organization); err != nil {
		return err
	}
	if finding != "" {
		_, err := db.ExecContext(ctx, `UPDATE health_findings SET slack_unavailable_at=?,slack_status_code=? WHERE organization_id=? AND id=?`, now, status, organization, finding)
		return err
	}
	return nil
}

func (s *Service) processSlackDelivery(ctx context.Context) (bool, error) {
	now := s.config.now()
	var organization tracker.OrganizationID
	var finding healthFinding
	var event, subject, opened, queued string
	var attempts int
	err := s.database.db.QueryRowContext(ctx, `SELECT f.organization_id,f.id,f.signal,f.subject_json,f.summary,f.next_action,f.opened_at,d.event,d.queued_at,d.attempts
 FROM health_slack_deliveries d JOIN health_findings f ON f.id=d.finding_id
 WHERE d.completed_at IS NULL AND d.attempts<2 AND julianday(d.next_attempt_at)<=julianday(?)
 AND (d.event='opened' OR NOT EXISTS(SELECT 1 FROM health_slack_deliveries o WHERE o.finding_id=d.finding_id AND o.event='opened' AND o.completed_at IS NULL AND o.attempts<2))
 ORDER BY d.queued_at,d.event LIMIT 1`, formatHubTime(now)).Scan(&organization, &finding.ID, &finding.Signal, &subject, &finding.Summary, &finding.NextAction, &opened, &event, &queued, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal([]byte(subject), &finding.Subject); err != nil {
		return false, err
	}
	finding.OpenedAt, err = parseTimeValue(opened)
	if err != nil {
		return false, err
	}
	at, err := parseTimeValue(queued)
	if err != nil {
		return false, err
	}
	webhook, err := s.readSlackWebhook(ctx, organization, "health-slack")
	if err != nil {
		return false, err
	}
	defer clear(webhook)
	if len(webhook) == 0 {
		_, err := s.database.db.ExecContext(ctx, `UPDATE health_slack_deliveries SET completed_at=? WHERE finding_id=? AND event=?`, formatHubTime(now), finding.ID, event)
		return true, err
	}
	publicURL := ""
	if s.config.Hosted != nil {
		publicURL = s.config.Hosted.PublicURL
	}
	text, err := renderSlackFinding(publicURL, s.hostedBase(), finding, event, at)
	if err != nil {
		return false, err
	}
	result, err := s.database.db.ExecContext(ctx, `UPDATE health_slack_deliveries SET attempts=attempts+1,next_attempt_at=? WHERE finding_id=? AND event=? AND attempts=? AND completed_at IS NULL`, formatHubTime(now.Add(time.Minute)), finding.ID, event, attempts)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return false, err
	}
	status, delivered := s.postSlack(ctx, webhook, text)
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.config.BusyTimeout)
	defer cancel()
	tx, err := s.database.db.BeginTx(finishCtx, nil)
	if err != nil {
		return true, err
	}
	defer tx.Rollback()
	at = s.config.now()
	if err := s.recordSlackResult(finishCtx, tx, organization, finding.ID, status, delivered, at); err != nil {
		return true, err
	}
	var completed *string
	if delivered || attempts == 1 {
		value := formatHubTime(at)
		completed = &value
	}
	if _, err := tx.ExecContext(finishCtx, `UPDATE health_slack_deliveries SET completed_at=?,next_attempt_at=? WHERE finding_id=? AND event=?`, completed, formatHubTime(at.Add(time.Minute)), finding.ID, event); err != nil {
		return true, err
	}
	return true, tx.Commit()
}
