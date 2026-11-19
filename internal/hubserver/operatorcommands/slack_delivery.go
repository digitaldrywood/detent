package operatorcommands

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

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthsignals"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const SlackWebhookSecret = "slack_webhook"

type SlackDeliveryStore interface {
	ReadSlack(context.Context, tracker.OrganizationID, corestore.Workload, func(postgres.SlackReader) error) error
	ReadHealth(context.Context, tracker.OrganizationID, corestore.Workload, func(postgres.HealthReader) error) error
	WriteSlack(context.Context, tracker.OrganizationID, corestore.Workload, func(postgres.SlackWriter) error) error
}

type SlackDeliveryDeps struct {
	Store       SlackDeliveryStore
	AccessRead  func(context.Context, tracker.OrganizationID, func(tenantstore.Reader) error) error
	AccessWrite func(context.Context, tracker.OrganizationID, func(tenantstore.Writer) error) error
	IssuePath   func(context.Context, tracker.OrganizationID, string) (string, error)
	Now         func() time.Time
	BasePath    func() string
	PublicURL   func() string
	Keyring     *hubsecrets.Keyring
	HTTPClient  *http.Client
	BusyTimeout time.Duration
}

var slackWebhookPath = regexp.MustCompile(`^/services/[A-Za-z0-9_-]+/[A-Za-z0-9_-]+/[A-Za-z0-9_-]+$`)

func ValidSlackWebhook(value string) bool {
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	return u.Scheme == "https" && (u.Host == "hooks.slack.com" || u.Host == "hooks.slack-gov.com") && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.RawPath == "" && slackWebhookPath.MatchString(u.Path)
}

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

func RenderSlackFinding(publicURL string, basePath string, finding healthsignals.Finding, event string, at time.Time, issuePath string) (string, error) {
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

func (s SlackDeliveryDeps) ReadWebhook(ctx context.Context, organization tracker.OrganizationID, actor string) ([]byte, error) {
	readCtx, cancelRead := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancelRead()
	var secret accessstore.Secret
	err := s.AccessRead(readCtx, organization, func(r tenantstore.Reader) error {
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
		secret, err = r.AccessSecret(readCtx, "", SlackWebhookSecret)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if s.Keyring == nil {
		return nil, errors.New("slack secret storage is unavailable")
	}
	value, err := s.Keyring.Open(secret.Envelope, accessstore.SecretAAD(organization, "", SlackWebhookSecret))
	if err != nil {
		return nil, err
	}
	err = s.AccessWrite(ctx, organization, func(w tenantstore.Writer) error {
		return w.RecordAccessSecretAudit(ctx, accessstore.SecretAudit{OrganizationID: organization, Actor: actor, Kind: SlackWebhookSecret, Event: "use", KeyVersion: secret.Envelope.Version, RecordedAt: s.Now()})
	})
	if err != nil {
		clear(value)
		return nil, err
	}
	return value, nil
}

type SlackSendResult struct {
	Status  int
	Outcome string
}

func (s SlackDeliveryDeps) Post(ctx context.Context, webhook []byte, text string) SlackSendResult {
	if !ValidSlackWebhook(string(webhook)) {
		return SlackSendResult{Outcome: "rejected"}
	}
	body, err := json.Marshal(struct {
		Text        string `json:"text"`
		UnfurlLinks bool   `json:"unfurl_links"`
		UnfurlMedia bool   `json:"unfurl_media"`
	}{Text: text})
	if err != nil {
		return SlackSendResult{Outcome: "rejected"}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, string(webhook), bytes.NewReader(body))
	if err != nil {
		return SlackSendResult{Outcome: "rejected"}
	}
	request.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 10 * time.Second}
	if s.HTTPClient != nil {
		client = *s.HTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		var dns *net.DNSError
		var operation *net.OpError
		if errors.As(err, &dns) || errors.As(err, &operation) && operation.Op == "dial" {
			return SlackSendResult{Outcome: "rejected"}
		}
		return SlackSendResult{Outcome: "uncertain"}
	}
	defer response.Body.Close()
	acknowledgment, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	defer clear(acknowledgment)
	result := SlackSendResult{Status: response.StatusCode, Outcome: "uncertain"}
	switch {
	case err == nil && len(acknowledgment) <= 4096 && response.StatusCode >= 200 && response.StatusCode < 300 && strings.TrimSpace(string(acknowledgment)) == "ok":
		result.Outcome = "committed"
	case response.StatusCode >= 300 && response.StatusCode < 500 && response.StatusCode != http.StatusRequestTimeout:
		result.Outcome = "rejected"
	}
	return result
}

func (s SlackDeliveryDeps) Process(ctx context.Context, organization tracker.OrganizationID) (bool, error) {
	if s.Keyring == nil || s.Store == nil {
		return false, nil
	}
	now := s.Now()
	var settings postgres.SlackIntegration
	var pending []postgres.SlackDelivery
	err := s.Store.ReadSlack(ctx, organization, corestore.Background, func(r postgres.SlackReader) error {
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
	err = s.Store.ReadHealth(ctx, organization, corestore.Background, func(r postgres.HealthReader) error {
		var err error
		record, err = r.HealthRecord(ctx, postgres.HealthRecordInput{OrganizationID: organization, ID: delivery.FindingID})
		return err
	})
	if err != nil {
		return false, fmt.Errorf("read slack finding: %w", err)
	}
	finding := healthsignals.Finding{ID: record.ID, Signal: record.Signal, Subject: healthsignals.Subject{Kind: record.SubjectKind, ID: record.SubjectID}, OpenedAt: record.OpenedAt, Summary: record.Summary, NextAction: record.NextAction}
	issuePath := ""
	if finding.Subject.Kind == "work_item" {
		issuePath, err = s.IssuePath(ctx, organization, finding.Subject.ID)
		if errors.Is(err, corestore.ErrNotFound) {
			issuePath, err = "", nil
		}
		if err != nil {
			return false, fmt.Errorf("read slack issue path: %w", err)
		}
	}
	text, err := RenderSlackFinding(s.PublicURL(), s.BasePath(), finding, delivery.Event, delivery.EventAt, issuePath)
	if err != nil {
		return false, fmt.Errorf("render slack finding: %w", err)
	}
	webhook, err := s.ReadWebhook(ctx, organization, "health-slack")
	if err != nil {
		return false, fmt.Errorf("read slack webhook: %w", err)
	}
	defer clear(webhook)
	if len(webhook) == 0 {
		return false, nil
	}
	next := now.Add(slackRetryDelay(delivery.Attempts, delivery.ID))
	err = s.Store.WriteSlack(ctx, organization, corestore.Background, func(w postgres.SlackWriter) error {
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
	result := s.Post(ctx, webhook, text)
	delivered := result.Outcome == "committed"
	receipt := ""
	if delivered {
		receipt = "http:" + strconv.Itoa(result.Status)
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.BusyTimeout)
	defer cancel()
	at := s.Now()
	err = s.Store.WriteSlack(finishCtx, organization, corestore.Background, func(w postgres.SlackWriter) error {
		if _, err := w.FinishSlackDelivery(finishCtx, postgres.FinishSlackDeliveryInput{OrganizationID: organization, ID: delivery.ID, ExpectedAttempts: delivery.Attempts + 1, Outcome: result.Outcome, ExternalReceipt: receipt, StatusCode: int32(result.Status), At: at, NextAttemptAt: next}); err != nil {
			return err
		}
		health, ok := w.(postgres.HealthWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if err := health.RecordHealthSlackStatus(finishCtx, postgres.RecordHealthSlackStatusInput{OrganizationID: organization, ID: delivery.FindingID, Failed: !delivered, StatusCode: int32(result.Status), At: at}); err != nil {
			return err
		}
		if err := w.RecordSlackStatus(finishCtx, postgres.RecordSlackStatusInput{OrganizationID: organization, ExpectedRevision: settings.Revision, StatusCode: int32(result.Status), Failed: !delivered, At: at}); err != nil && !errors.Is(err, corestore.ErrRevisionConflict) {
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
