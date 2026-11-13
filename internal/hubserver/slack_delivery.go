package hubserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
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
		var dns *net.DNSError
		var operation *net.OpError
		if errors.As(err, &dns) || errors.As(err, &operation) && operation.Op == "dial" {
			return slackSendResult{outcome: "rejected"}
		}
		return slackSendResult{outcome: "uncertain"}
	}
	defer response.Body.Close()
	acknowledgment, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	defer clear(acknowledgment)
	result := slackSendResult{status: response.StatusCode, outcome: "uncertain"}
	switch {
	case err == nil && len(acknowledgment) <= 4096 && response.StatusCode >= 200 && response.StatusCode < 300 && strings.TrimSpace(string(acknowledgment)) == "ok":
		result.outcome = "committed"
	case response.StatusCode >= 300 && response.StatusCode < 500 && response.StatusCode != http.StatusRequestTimeout:
		result.outcome = "rejected"
	}
	return result
}

func (s *Service) HealthSlackMaintenanceFeature() hubmaintenance.Feature {
	return hubmaintenance.Feature{Name: "health-slack", Bytes: 1 << 20, Step: func(ctx context.Context, tenant hubmaintenance.Tenant, _ hubmaintenance.Budget) error {
		if err := hubmaintenance.RequireTenant(ctx, tenant); err != nil {
			return err
		}
		_, err := s.processSlackDelivery(ctx, tracker.OrganizationID(tenant.Organization))
		return err
	}}
}

func (s *Service) processSlackDelivery(ctx context.Context, organization tracker.OrganizationID) (bool, error) {
	if s.config.SecretKeys == nil || s.database == nil || s.database.postgres == nil {
		return false, nil
	}
	now := s.config.now()
	var settings postgres.SlackIntegration
	var pending []postgres.SlackDelivery
	err := s.database.postgres.ReadSlack(ctx, organization, corestore.Background, func(r postgres.SlackReader) error {
		var err error
		settings, err = r.SlackIntegration(ctx)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		pending, err = r.PendingSlackDeliveries(ctx, postgres.PendingSlackDeliveriesInput{OrganizationID: organization, NextAttemptAt: now, Limit: 1})
		return err
	})
	if err != nil {
		return false, fmt.Errorf("read slack delivery: %w", err)
	}
	if len(pending) == 0 || !settings.Enabled {
		return false, nil
	}
	delivery := pending[0]
	var record postgres.HealthRecord
	err = s.database.postgres.ReadHealth(ctx, organization, corestore.Background, func(r postgres.HealthReader) error {
		var err error
		record, err = r.HealthRecord(ctx, postgres.HealthRecordInput{OrganizationID: organization, ID: delivery.FindingID})
		return err
	})
	if err != nil {
		return false, fmt.Errorf("read slack finding: %w", err)
	}
	finding := healthFinding{ID: record.ID, Signal: record.Signal, Subject: healthSubject{Kind: record.SubjectKind, ID: record.SubjectID}, OpenedAt: record.OpenedAt, Summary: record.Summary, NextAction: record.NextAction}
	issuePath := ""
	if finding.Subject.Kind == "work_item" {
		issuePath, err = s.readIssueBrowserPath(ctx, organization, finding.Subject.ID)
		if errors.Is(err, corestore.ErrNotFound) {
			issuePath, err = "", nil
		}
		if err != nil {
			return false, fmt.Errorf("read slack issue path: %w", err)
		}
	}
	publicURL := ""
	if s.config.Hosted != nil {
		publicURL = s.config.Hosted.PublicURL
	}
	text, err := renderSlackFinding(publicURL, s.hostedBase(), finding, delivery.Event, delivery.EventAt, issuePath)
	if err != nil {
		return false, fmt.Errorf("render slack finding: %w", err)
	}
	webhook, err := s.readSlackWebhook(ctx, organization, "health-slack")
	if err != nil {
		return false, fmt.Errorf("read slack webhook: %w", err)
	}
	defer clear(webhook)
	if len(webhook) == 0 {
		return false, nil
	}
	next := now.Add(slackRetryDelay(delivery.Attempts, delivery.ID))
	err = s.database.postgres.WriteSlack(ctx, organization, corestore.Background, func(w postgres.SlackWriter) error {
		claimed, err := w.ClaimSlackDelivery(ctx, postgres.ClaimSlackDeliveryInput{OrganizationID: organization, ID: delivery.ID, ExpectedAttempts: delivery.Attempts, Now: now, NextAttemptAt: next})
		if err != nil {
			return err
		}
		_, err = w.FinishSlackDelivery(ctx, postgres.FinishSlackDeliveryInput{OrganizationID: organization, ID: delivery.ID, ExpectedAttempts: claimed.Attempts, Outcome: "uncertain", At: now, NextAttemptAt: next})
		return err
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim slack delivery: %w", err)
	}
	result := s.postSlack(ctx, webhook, text)
	delivered := result.outcome == "committed"
	receipt := ""
	if delivered {
		receipt = "http:" + strconv.Itoa(result.status)
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.config.BusyTimeout)
	defer cancel()
	at := s.config.now()
	err = s.database.postgres.WriteSlack(finishCtx, organization, corestore.Background, func(w postgres.SlackWriter) error {
		if _, err := w.FinishSlackDelivery(finishCtx, postgres.FinishSlackDeliveryInput{OrganizationID: organization, ID: delivery.ID, ExpectedAttempts: delivery.Attempts + 1, Outcome: result.outcome, ExternalReceipt: receipt, StatusCode: int32(result.status), At: at, NextAttemptAt: next}); err != nil {
			return err
		}
		health, ok := w.(postgres.HealthWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if err := health.RecordHealthSlackStatus(finishCtx, postgres.RecordHealthSlackStatusInput{OrganizationID: organization, ID: delivery.FindingID, Failed: !delivered, StatusCode: int32(result.status), At: at}); err != nil {
			return err
		}
		if err := w.RecordSlackStatus(finishCtx, postgres.RecordSlackStatusInput{OrganizationID: organization, ExpectedRevision: settings.Revision, StatusCode: int32(result.status), Failed: !delivered, At: at}); err != nil && !errors.Is(err, corestore.ErrRevisionConflict) {
			return err
		}
		return nil
	})
	if err != nil {
		return true, fmt.Errorf("finish slack delivery: %w", err)
	}
	return true, nil
}

func slackRetryDelay(attempts int64, id string) time.Duration {
	base := time.Minute * time.Duration(1<<min(max(attempts, 0), 5))
	sum := sha256.Sum256([]byte(id))
	return base + base*time.Duration(sum[0])/1280
}
