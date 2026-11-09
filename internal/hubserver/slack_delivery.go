package hubserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
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
	var secret accessstore.Secret
	err := s.accessRead(ctx, organization, func(r tenantstore.Reader) error {
		var err error
		secret, err = r.AccessSecret(ctx, "", slackWebhookSecret)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	value, err := accessstore.OpenSecret(s.config.SecretKeys, secret)
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

type slackSendResult struct {
	status  int
	outcome string
}

func (s *Service) postSlack(ctx context.Context, webhook []byte, text string) slackSendResult {
	if !validSlackWebhook(string(webhook)) {
		return slackSendResult{outcome: "rejected"}
	}
	body, err := json.Marshal(struct {
		Text        string `json:"text"`
		UnfurlLinks bool   `json:"unfurl_links"`
		UnfurlMedia bool   `json:"unfurl_media"`
	}{Text: text})
	if err != nil {
		return slackSendResult{outcome: "rejected"}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, string(webhook), bytes.NewReader(body))
	if err != nil {
		return slackSendResult{outcome: "rejected"}
	}
	request.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 10 * time.Second}
	if s.config.SlackHTTPClient != nil {
		client = *s.config.SlackHTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		var op *net.OpError
		var dns *net.DNSError
		if errors.As(err, &dns) || errors.As(err, &op) && op.Op == "dial" {
			return slackSendResult{outcome: "rejected"}
		}
		return slackSendResult{outcome: "uncertain"}
	}
	defer response.Body.Close()
	result := slackSendResult{status: response.StatusCode, outcome: "uncertain"}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		result.outcome = "committed"
	}
	if response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != http.StatusRequestTimeout {
		result.outcome = "rejected"
	}
	return result
}

func (s *Service) processSlackDelivery(ctx context.Context) (bool, error) {
	if s.config.SecretKeys == nil || s.database.postgres == nil {
		return false, nil
	}
	org, now := s.accessOrganization(), s.config.now()
	var settings postgres.SlackIntegration
	var pending []postgres.SlackDelivery
	err := s.database.postgres.ReadSlack(ctx, org, corestore.Background, func(r postgres.SlackReader) error {
		var err error
		settings, err = r.SlackIntegration(ctx)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		pending, err = r.PendingSlackDeliveries(ctx, postgres.PendingSlackDeliveriesInput{OrganizationID: org, NextAttemptAt: now, Limit: 1})
		return err
	})
	if err != nil || len(pending) == 0 {
		return false, err
	}
	delivery := pending[0]
	if !settings.Enabled || delivery.IntegrationRevision <= settings.DisabledRevision {
		err := s.database.postgres.WriteSlack(ctx, org, corestore.Background, func(w postgres.SlackWriter) error {
			_, err := w.SuppressSlackDelivery(ctx, postgres.SuppressSlackDeliveryInput{OrganizationID: org, ID: delivery.ID, ExpectedAttempts: delivery.Attempts, At: now})
			return err
		})
		return err == nil, err
	}
	var record postgres.HealthRecord
	err = s.database.postgres.ReadHealth(ctx, org, corestore.Background, func(r postgres.HealthReader) error {
		var err error
		record, err = r.HealthRecord(ctx, postgres.HealthRecordInput{OrganizationID: org, ID: delivery.FindingID})
		return err
	})
	if err != nil {
		return false, err
	}
	finding := healthFinding{ID: record.ID, Signal: record.Signal, Subject: healthSubject{Kind: record.SubjectKind, ID: record.SubjectID}, OpenedAt: record.OpenedAt, Summary: record.Summary, NextAction: record.NextAction}
	webhook, err := s.readSlackWebhook(ctx, org, "health-slack")
	if err != nil {
		return false, err
	}
	defer clear(webhook)
	if len(webhook) == 0 {
		return false, errors.New("slack enabled without webhook")
	}
	issuePath := ""
	if finding.Subject.Kind == "work_item" {
		issuePath, err = s.readIssueBrowserPath(ctx, finding.Subject.ID)
		if errors.Is(err, corestore.ErrNotFound) {
			err = nil
		}
		if err != nil {
			return false, err
		}
	}
	publicURL := ""
	if s.config.Hosted != nil {
		publicURL = s.config.Hosted.PublicURL
	}
	text, err := renderSlackFinding(publicURL, s.hostedBase(), finding, delivery.Event, delivery.EventAt, issuePath)
	if err != nil {
		return false, err
	}
	next := now.Add(slackRetryDelay(delivery.Attempts, delivery.ID))
	err = s.database.postgres.WriteSlack(ctx, org, corestore.Background, func(w postgres.SlackWriter) error {
		claimed, err := w.ClaimSlackDelivery(ctx, postgres.ClaimSlackDeliveryInput{OrganizationID: org, ID: delivery.ID, ExpectedAttempts: delivery.Attempts, ExpectedSettingsRevision: settings.Revision, Now: now, NextAttemptAt: next})
		if err != nil {
			return err
		}
		_, err = w.FinishSlackDelivery(ctx, postgres.FinishSlackDeliveryInput{OrganizationID: org, ID: delivery.ID, ExpectedAttempts: claimed.Attempts, Outcome: "uncertain", At: now, NextAttemptAt: next})
		return err
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	result := s.postSlack(ctx, webhook, text)
	status, outcome := result.status, result.outcome
	delivered := outcome == "committed"
	receipt := ""
	if delivered {
		receipt = "http:" + strconv.Itoa(status)
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err = s.database.postgres.WriteSlack(finishCtx, org, corestore.Background, func(w postgres.SlackWriter) error {
		if _, err := w.FinishSlackDelivery(finishCtx, postgres.FinishSlackDeliveryInput{OrganizationID: org, ID: delivery.ID, ExpectedAttempts: delivery.Attempts + 1, Outcome: outcome, ExternalReceipt: receipt, StatusCode: int32(status), At: s.config.now(), NextAttemptAt: next}); err != nil {
			return err
		}
		health, ok := w.(postgres.HealthWriter)
		if !ok {
			return workItemStorageUnavailable()
		}
		if err := health.RecordHealthSlackStatus(finishCtx, postgres.RecordHealthSlackStatusInput{OrganizationID: org, ID: delivery.FindingID, Failed: !delivered, StatusCode: int32(status), At: s.config.now()}); err != nil {
			return err
		}
		current, err := w.SlackIntegration(finishCtx)
		if err != nil {
			return err
		}
		if current.Revision != settings.Revision {
			return nil
		}
		return w.RecordSlackStatus(finishCtx, postgres.RecordSlackStatusInput{OrganizationID: org, ExpectedRevision: settings.Revision, StatusCode: int32(status), Failed: !delivered, At: s.config.now()})
	})
	return true, err
}

func slackRetryDelay(attempts int64, id string) time.Duration {
	base := time.Minute * time.Duration(1<<min(max(attempts, 0), 5))
	sum := sha256.Sum256([]byte(id))
	return base + base*time.Duration(sum[0])/1280
}

func enqueueSlackOpened(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, finding string, at time.Time) error {
	now := formatHubTime(at)
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO health_slack_deliveries(finding_id,event,queued_at,next_attempt_at) SELECT ?, 'opened', ?, ? WHERE EXISTS(SELECT 1 FROM organization_secrets WHERE organization_id=? AND kind=?)`, finding, now, now, organization, slackWebhookSecret)
	return err
}

func enqueueSlackResolved(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, fingerprints sqlParameterRows, at time.Time) error {
	now := formatHubTime(at)
	_, err := execSQL(ctx, tx, `INSERT OR IGNORE INTO health_slack_deliveries(finding_id,event,queued_at,next_attempt_at)
 SELECT f.id,'resolved',?,? FROM health_findings f WHERE f.organization_id=? AND f.resolved_at IS NULL AND f.severity='attention' AND f.fingerprint NOT IN (SELECT value FROM sql_rows(?))
 AND EXISTS(SELECT 1 FROM organization_secrets WHERE organization_id=? AND kind=?) AND EXISTS(SELECT 1 FROM health_slack_deliveries d WHERE d.finding_id=f.id AND d.event='opened')`, now, now, organization, fingerprints, organization, slackWebhookSecret)
	return err
}
