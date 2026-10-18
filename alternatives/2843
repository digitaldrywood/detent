package statuspage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultRequestTimeout = 5 * time.Second
	maxResponseBytes      = 2 << 20
)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type ClientConfig struct {
	HTTPClient HTTPDoer
	UserAgent  string
}

type Client struct {
	httpClient HTTPDoer
	userAgent  string
}

type Report struct {
	Page       pagePayload
	Status     statusPayload
	Components []componentPayload
	Incidents  []incidentPayload
}

func NewClient(cfg ClientConfig) *Client {
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultRequestTimeout}
	}
	userAgent := strings.TrimSpace(cfg.UserAgent)
	if userAgent == "" {
		userAgent = "detent-provider-status"
	}
	return &Client{httpClient: httpClient, userAgent: userAgent}
}

func (c *Client) Fetch(ctx context.Context, source Source) (Report, error) {
	source, ok := source.normalize()
	if !ok {
		return Report{}, errors.New("invalid status page source")
	}
	var summary summaryPayload
	if err := c.fetch(ctx, source.BaseURL+"/api/v2/summary.json", &summary); err != nil {
		return Report{}, fmt.Errorf("fetch summary: %w", err)
	}
	if err := validateSummary(summary); err != nil {
		return Report{}, fmt.Errorf("validate summary: %w", err)
	}

	var unresolved unresolvedPayload
	if err := c.fetch(ctx, source.BaseURL+"/api/v2/incidents/unresolved.json", &unresolved); err != nil {
		return Report{}, fmt.Errorf("fetch unresolved incidents: %w", err)
	}
	if err := validateUnresolved(unresolved); err != nil {
		return Report{}, fmt.Errorf("validate unresolved incidents: %w", err)
	}
	if summary.Page.ID != unresolved.Page.ID {
		return Report{}, errors.New("status page feeds identify different pages")
	}
	return Report{
		Page:       summary.Page,
		Status:     summary.Status,
		Components: append([]componentPayload(nil), summary.Components...),
		Incidents:  append([]incidentPayload(nil), unresolved.Incidents...),
	}, nil
}

func (c *Client) fetch(ctx context.Context, endpoint string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", c.userAgent)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxResponseBytes {
		return errors.New("payload exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode JSON: trailing value")
		}
		return fmt.Errorf("decode JSON trailer: %w", err)
	}
	return nil
}

type summaryPayload struct {
	Page                  pagePayload        `json:"page"`
	Components            []componentPayload `json:"components"`
	Incidents             []incidentPayload  `json:"incidents"`
	ScheduledMaintenances []incidentPayload  `json:"scheduled_maintenances"`
	Status                statusPayload      `json:"status"`
}

type unresolvedPayload struct {
	Page      pagePayload       `json:"page"`
	Incidents []incidentPayload `json:"incidents"`
}

type pagePayload struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	TimeZone  string    `json:"time_zone"`
	UpdatedAt time.Time `json:"updated_at"`
}

type statusPayload struct {
	Indicator   string `json:"indicator"`
	Description string `json:"description"`
}

type componentPayload struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	Status             string    `json:"status"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	Position           int       `json:"position"`
	Description        *string   `json:"description"`
	Showcase           bool      `json:"showcase"`
	StartDate          *string   `json:"start_date"`
	GroupID            *string   `json:"group_id"`
	PageID             string    `json:"page_id"`
	Group              bool      `json:"group"`
	OnlyShowIfDegraded bool      `json:"only_show_if_degraded"`
}

type incidentPayload struct {
	ID                string                  `json:"id"`
	Name              string                  `json:"name"`
	Status            string                  `json:"status"`
	CreatedAt         time.Time               `json:"created_at"`
	UpdatedAt         time.Time               `json:"updated_at"`
	MonitoringAt      *time.Time              `json:"monitoring_at"`
	ResolvedAt        *time.Time              `json:"resolved_at"`
	Impact            string                  `json:"impact"`
	Shortlink         string                  `json:"shortlink"`
	StartedAt         time.Time               `json:"started_at"`
	PageID            string                  `json:"page_id"`
	IncidentUpdates   []incidentUpdatePayload `json:"incident_updates"`
	Components        []componentPayload      `json:"components"`
	ReminderIntervals json.RawMessage         `json:"reminder_intervals"`
	ScheduledFor      *time.Time              `json:"scheduled_for"`
	ScheduledUntil    *time.Time              `json:"scheduled_until"`
}

type incidentUpdatePayload struct {
	ID                   string                     `json:"id"`
	Status               string                     `json:"status"`
	Body                 string                     `json:"body"`
	IncidentID           string                     `json:"incident_id"`
	CreatedAt            time.Time                  `json:"created_at"`
	UpdatedAt            time.Time                  `json:"updated_at"`
	DisplayAt            time.Time                  `json:"display_at"`
	AffectedComponents   []affectedComponentPayload `json:"affected_components"`
	DeliverNotifications bool                       `json:"deliver_notifications"`
	CustomTweet          *string                    `json:"custom_tweet"`
	TweetID              *string                    `json:"tweet_id"`
}

type affectedComponentPayload struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	OldStatus string `json:"old_status"`
	NewStatus string `json:"new_status"`
}

func validateSummary(summary summaryPayload) error {
	if err := validatePage(summary.Page); err != nil {
		return err
	}
	if !validStatusIndicator(summary.Status.Indicator) || strings.TrimSpace(summary.Status.Description) == "" {
		return errors.New("summary status is invalid")
	}
	for _, component := range summary.Components {
		if err := validateComponent(component, summary.Page.ID); err != nil {
			return err
		}
	}
	for _, incident := range append(append([]incidentPayload(nil), summary.Incidents...), summary.ScheduledMaintenances...) {
		if err := validateIncident(incident, summary.Page.ID, false); err != nil {
			return err
		}
	}
	return nil
}

func validateUnresolved(unresolved unresolvedPayload) error {
	if err := validatePage(unresolved.Page); err != nil {
		return err
	}
	for _, incident := range unresolved.Incidents {
		if err := validateIncident(incident, unresolved.Page.ID, true); err != nil {
			return err
		}
	}
	return nil
}

func validatePage(page pagePayload) error {
	if strings.TrimSpace(page.ID) == "" || strings.TrimSpace(page.Name) == "" || strings.TrimSpace(page.TimeZone) == "" || page.UpdatedAt.IsZero() || !validHTTPURL(page.URL) {
		return errors.New("page metadata is invalid")
	}
	return nil
}

func validateComponent(component componentPayload, pageID string) error {
	if strings.TrimSpace(component.ID) == "" || strings.TrimSpace(component.Name) == "" || component.PageID != pageID || component.CreatedAt.IsZero() || component.UpdatedAt.IsZero() || !validComponentStatus(component.Status) {
		return fmt.Errorf("component %q is invalid", component.Name)
	}
	return nil
}

func validateIncident(incident incidentPayload, pageID string, unresolved bool) error {
	if strings.TrimSpace(incident.ID) == "" || strings.TrimSpace(incident.Name) == "" || incident.PageID != pageID || incident.CreatedAt.IsZero() || incident.UpdatedAt.IsZero() || incident.StartedAt.IsZero() || !validIncidentStatus(incident.Status) || !validImpact(incident.Impact) || !validHTTPURL(incident.Shortlink) {
		return fmt.Errorf("incident %q is invalid", incident.Name)
	}
	if unresolved && (incident.Status == "resolved" || incident.Status == "completed") {
		return fmt.Errorf("incident %q is resolved in unresolved feed", incident.Name)
	}
	for _, component := range incident.Components {
		if err := validateComponent(component, pageID); err != nil {
			return err
		}
	}
	for _, update := range incident.IncidentUpdates {
		if strings.TrimSpace(update.ID) == "" || update.IncidentID != incident.ID || strings.TrimSpace(update.Status) == "" || update.CreatedAt.IsZero() || update.UpdatedAt.IsZero() || update.DisplayAt.IsZero() {
			return fmt.Errorf("incident %q update is invalid", incident.Name)
		}
		for _, component := range update.AffectedComponents {
			if strings.TrimSpace(component.Code) == "" || strings.TrimSpace(component.Name) == "" || !validComponentStatus(component.OldStatus) || !validComponentStatus(component.NewStatus) {
				return fmt.Errorf("incident %q affected component is invalid", incident.Name)
			}
		}
	}
	return nil
}

func validHTTPURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	return err == nil && parsed != nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func validStatusIndicator(value string) bool {
	switch strings.TrimSpace(value) {
	case "none", "minor", "major", "critical", "maintenance":
		return true
	default:
		return false
	}
}

func validComponentStatus(value string) bool {
	switch strings.TrimSpace(value) {
	case "operational", "degraded_performance", "partial_outage", "major_outage", "under_maintenance":
		return true
	default:
		return false
	}
}

func validIncidentStatus(value string) bool {
	switch strings.TrimSpace(value) {
	case "investigating", "identified", "monitoring", "resolved", "scheduled", "in_progress", "verifying", "completed":
		return true
	default:
		return false
	}
}

func validImpact(value string) bool {
	switch strings.TrimSpace(value) {
	case "none", "minor", "major", "critical", "maintenance":
		return true
	default:
		return false
	}
}
