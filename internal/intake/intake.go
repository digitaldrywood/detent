package intake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

var (
	ErrMissingStore       = errors.New("intake issue store is required")
	ErrSourceNotFound     = errors.New("intake source not found")
	ErrSourceNotWebhook   = errors.New("intake source is not a webhook")
	ErrSourceNotScheduled = errors.New("intake source is not scheduled")
	ErrMissingFingerprint = errors.New("intake event fingerprint is required")
	ErrInvalidPayload     = errors.New("intake payload is invalid")
	ErrUnknownAdapter     = errors.New("intake webhook adapter is not registered")
	ErrUnknownScanner     = errors.New("intake scanner is not registered")
)

type Event struct {
	Source      string
	Kind        string
	Summary     string
	Details     string
	Fingerprint string
	Fields      map[string]string
}

type Issue struct {
	ID         string `json:"id,omitempty"`
	Identifier string `json:"identifier,omitempty"`
	Number     int    `json:"number,omitempty"`
	URL        string `json:"url,omitempty"`
	Body       string `json:"-"`
	Closed     bool   `json:"-"`
}

type IssueDraft struct {
	Title  string
	Body   string
	Labels []string
}

type IssueStore interface {
	FindIntakeIssue(context.Context, string) (Issue, bool, error)
	CreateIntakeIssue(context.Context, IssueDraft) (Issue, error)
	UpdateIntakeIssue(context.Context, string, IssueDraft) (Issue, error)
	SetIntakeIssueState(context.Context, string, string) error
}

type WebhookAdapter interface {
	Decode([]byte) (Event, error)
}

type WebhookFactory interface {
	New(string) (WebhookAdapter, error)
}

type Scanner interface {
	Scan(context.Context) ([]Event, error)
}

type ScannerFactory interface {
	New(string, string) (Scanner, error)
}

type Dependencies struct {
	Root           string
	WebhookFactory WebhookFactory
	ScannerFactory ScannerFactory
	Logger         *slog.Logger
	Now            func() time.Time
}

type Result struct {
	Issue   Issue  `json:"issue,omitempty"`
	Created bool   `json:"created"`
	Matched bool   `json:"matched"`
	Source  string `json:"source"`
}

type runtimeSource struct {
	config   Source
	adapter  WebhookAdapter
	scanner  Scanner
	schedule cron.Schedule
}

type Prepared struct {
	config  Config
	sources map[string]runtimeSource
	store   IssueStore
	root    string
}

type Manager struct {
	mu             sync.RWMutex
	processMu      sync.Mutex
	config         Config
	sources        map[string]runtimeSource
	store          IssueStore
	root           string
	webhookFactory WebhookFactory
	scannerFactory ScannerFactory
	logger         *slog.Logger
	now            func() time.Time
	issues         map[string]Issue
	updates        chan struct{}
}

const pendingStateMarker = "<!-- detent-intake-state:pending -->"

func New(cfg Config, store IssueStore, deps Dependencies) (*Manager, error) {
	webhookFactory := deps.WebhookFactory
	if webhookFactory == nil {
		webhookFactory = DefaultWebhookFactory()
	}
	scannerFactory := deps.ScannerFactory
	if scannerFactory == nil {
		scannerFactory = DefaultScannerFactory()
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	manager := &Manager{
		root:           strings.TrimSpace(deps.Root),
		webhookFactory: webhookFactory,
		scannerFactory: scannerFactory,
		logger:         logger,
		now:            now,
		issues:         map[string]Issue{},
		updates:        make(chan struct{}, 1),
	}
	if err := manager.Update(cfg, store, deps.Root); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *Manager) Enabled() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config.Enabled()
}

func (m *Manager) Update(cfg Config, store IssueStore, root string) error {
	prepared, err := m.Prepare(cfg, store, root)
	if err != nil {
		return err
	}
	m.Apply(prepared)
	return nil
}

func (m *Manager) Prepare(cfg Config, store IssueStore, root string) (*Prepared, error) {
	if m == nil {
		return nil, ErrMissingStore
	}
	cfg.Normalize()
	if problems := cfg.Validate("intake", nil); len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	if cfg.Enabled() && store == nil {
		return nil, ErrMissingStore
	}

	root = strings.TrimSpace(root)
	sources := make(map[string]runtimeSource, len(cfg.Sources))
	for _, source := range cfg.Sources {
		runtime := runtimeSource{config: source}
		var err error
		if source.Kind == KindSchedule {
			runtime.schedule, err = cron.ParseStandard(source.Cron)
			if err == nil {
				runtime.scanner, err = m.scannerFactory.New(source.Scan, root)
			}
		} else {
			runtime.adapter, err = m.webhookFactory.New(source.Kind)
		}
		if err != nil {
			return nil, fmt.Errorf("configure intake source %s: %w", source.Name, err)
		}
		sources[source.Name] = runtime
	}

	return &Prepared{
		config:  cloneConfig(cfg),
		sources: sources,
		store:   store,
		root:    root,
	}, nil
}

func (m *Manager) Apply(prepared *Prepared) {
	if m == nil || prepared == nil {
		return
	}
	m.mu.Lock()
	m.config = prepared.config
	m.sources = prepared.sources
	m.store = prepared.store
	m.root = prepared.root
	m.mu.Unlock()
	m.signalUpdate()
}

func (m *Manager) Source(name string) (Source, bool) {
	if m == nil {
		return Source{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	runtime, ok := m.sources[strings.ToLower(strings.TrimSpace(name))]
	return runtime.config, ok
}

func (m *Manager) IngestWebhook(ctx context.Context, name string, payload []byte) (Result, error) {
	runtime, store, ok := m.runtimeSource(name)
	if !ok {
		return Result{}, ErrSourceNotFound
	}
	if runtime.config.Kind == KindSchedule || runtime.adapter == nil {
		return Result{}, ErrSourceNotWebhook
	}
	event, err := runtime.adapter.Decode(payload)
	if err != nil {
		return Result{}, fmt.Errorf("decode intake source %s: %w", runtime.config.Name, err)
	}
	return m.process(ctx, runtime.config, store, event)
}

func (m *Manager) RunScheduled(ctx context.Context, name string) ([]Result, error) {
	runtime, store, ok := m.runtimeSource(name)
	if !ok {
		return nil, ErrSourceNotFound
	}
	if runtime.config.Kind != KindSchedule {
		return nil, ErrSourceNotScheduled
	}
	if runtime.scanner == nil {
		return nil, fmt.Errorf("create intake scanner %s: %w", runtime.config.Scan, ErrUnknownScanner)
	}
	events, err := runtime.scanner.Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("scan intake source %s: %w", runtime.config.Name, err)
	}
	results := make([]Result, 0, len(events))
	var resultErr error
	for _, event := range events {
		result, err := m.process(ctx, runtime.config, store, event)
		if err != nil {
			resultErr = errors.Join(resultErr, err)
			continue
		}
		results = append(results, result)
	}
	return results, resultErr
}

func (m *Manager) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		due, ok := m.nextScheduled(m.now())
		if !ok {
			select {
			case <-ctx.Done():
				return nil
			case <-m.updates:
				continue
			}
		}

		delay := due.at.Sub(m.now())
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-m.updates:
			if !timer.Stop() {
				<-timer.C
			}
			continue
		case <-timer.C:
		}
		for _, name := range due.names {
			if _, err := m.RunScheduled(ctx, name); err != nil && ctx.Err() == nil {
				m.logger.ErrorContext(ctx, "scheduled intake failed", "source", name, "error", err)
			}
		}
	}
}

type scheduledDue struct {
	at    time.Time
	names []string
}

func (m *Manager) nextScheduled(now time.Time) (scheduledDue, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var due scheduledDue
	for name, source := range m.sources {
		if source.schedule == nil {
			continue
		}
		next := source.schedule.Next(now)
		if due.at.IsZero() || next.Before(due.at) {
			due = scheduledDue{at: next, names: []string{name}}
			continue
		}
		if next.Equal(due.at) {
			due.names = append(due.names, name)
		}
	}
	if due.at.IsZero() {
		return scheduledDue{}, false
	}
	sort.Strings(due.names)
	return due, true
}

func (m *Manager) runtimeSource(name string) (runtimeSource, IssueStore, bool) {
	if m == nil {
		return runtimeSource{}, nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	runtime, ok := m.sources[strings.ToLower(strings.TrimSpace(name))]
	return runtime, m.store, ok
}

func (m *Manager) process(ctx context.Context, source Source, store IssueStore, event Event) (Result, error) {
	event = normalizeEvent(source, event)
	if event.Summary == "" {
		return Result{Source: source.Name}, fmt.Errorf("%w: summary is required", ErrInvalidPayload)
	}
	result := Result{Matched: matches(source.Match, event.Fields), Source: source.Name}
	if !result.Matched {
		return result, nil
	}
	fingerprint := fieldValue(event.Fields, source.DedupeBy)
	if fingerprint == "" && strings.EqualFold(source.DedupeBy, "fingerprint") {
		fingerprint = strings.TrimSpace(event.Fingerprint)
	}
	if fingerprint == "" {
		return result, fmt.Errorf("%w: source %s field %s", ErrMissingFingerprint, source.Name, source.DedupeBy)
	}
	marker := fingerprintMarker(source.Name, fingerprint)
	fields := cloneFields(event.Fields)
	fields["source"] = source.Name
	fields["kind"] = source.Kind
	fields["summary"] = event.Summary
	fields["details"] = event.Details
	fields["fingerprint"] = fingerprint
	draft := IssueDraft{
		Title:  render(source.Creates.Title, fields),
		Body:   appendMarker(render(source.Creates.Body, fields), marker),
		Labels: append([]string(nil), source.Creates.Labels...),
	}
	pendingDraft := draft
	pendingDraft.Body = appendMarker(draft.Body, pendingStateMarker)

	m.processMu.Lock()
	defer m.processMu.Unlock()
	if cached, ok := m.issues[marker]; ok {
		return m.updateExisting(ctx, source, store, marker, cached, draft, pendingDraft, result)
	}
	issue, found, err := store.FindIntakeIssue(ctx, marker)
	if err != nil {
		return result, fmt.Errorf("find intake issue: %w", err)
	}
	if found {
		return m.updateExisting(ctx, source, store, marker, issue, draft, pendingDraft, result)
	}
	issue, err = store.CreateIntakeIssue(ctx, pendingDraft)
	if err != nil {
		return result, fmt.Errorf("create intake issue: %w", err)
	}
	m.issues[marker] = issue
	result.Issue = issue
	result.Created = true
	if err := store.SetIntakeIssueState(ctx, issue.ID, source.Creates.Status); err != nil {
		return result, fmt.Errorf("set intake issue %s state: %w", issue.Identifier, err)
	}
	issue, err = store.UpdateIntakeIssue(ctx, issue.ID, draft)
	if err != nil {
		return result, fmt.Errorf("complete intake issue %s state handoff: %w", issue.Identifier, err)
	}
	m.issues[marker] = issue
	result.Issue = issue
	return result, nil
}

func (m *Manager) updateExisting(
	ctx context.Context,
	source Source,
	store IssueStore,
	marker string,
	issue Issue,
	draft IssueDraft,
	pendingDraft IssueDraft,
	result Result,
) (Result, error) {
	if strings.Contains(issue.Body, pendingStateMarker) {
		updated, err := store.UpdateIntakeIssue(ctx, issue.ID, pendingDraft)
		if err != nil {
			return result, fmt.Errorf("update pending intake issue %s: %w", issue.Identifier, err)
		}
		m.issues[marker] = updated
		result.Issue = updated
		if err := store.SetIntakeIssueState(ctx, issue.ID, source.Creates.Status); err != nil {
			return result, fmt.Errorf("set intake issue %s state: %w", issue.Identifier, err)
		}
	}
	updated, err := store.UpdateIntakeIssue(ctx, issue.ID, draft)
	if err != nil {
		return result, fmt.Errorf("update intake issue %s: %w", issue.Identifier, err)
	}
	m.issues[marker] = updated
	result.Issue = updated
	return result, nil
}

func normalizeEvent(source Source, event Event) Event {
	event.Source = source.Name
	event.Kind = source.Kind
	event.Summary = strings.TrimSpace(event.Summary)
	event.Details = strings.TrimSpace(event.Details)
	if event.Details == "" {
		event.Details = event.Summary
	}
	if event.Fields == nil {
		event.Fields = map[string]string{}
	}
	event.Fields["source"] = source.Name
	event.Fields["kind"] = source.Kind
	event.Fields["summary"] = event.Summary
	event.Fields["details"] = event.Details
	if event.Fingerprint != "" {
		event.Fields["fingerprint"] = event.Fingerprint
	}
	return event
}

func matches(expression string, fields map[string]string) bool {
	expression = strings.TrimSpace(expression)
	if expression == "" {
		return true
	}
	key, want, ok := strings.Cut(expression, ":")
	if !ok {
		return false
	}
	return strings.EqualFold(fieldValue(fields, key), strings.TrimSpace(want))
}

func fieldValue(fields map[string]string, key string) string {
	key = strings.TrimSpace(key)
	if value := strings.TrimSpace(fields[key]); value != "" {
		return value
	}
	for candidate, value := range fields {
		if strings.EqualFold(strings.TrimSpace(candidate), key) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func fingerprintMarker(source string, fingerprint string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(source)) + "\x00" + strings.TrimSpace(fingerprint)))
	return "<!-- detent-intake:" + hex.EncodeToString(digest[:]) + " -->"
}

func appendMarker(body string, marker string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return marker
	}
	return body + "\n\n" + marker
}

func render(pattern string, fields map[string]string) string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i int, j int) bool {
		return len(keys[i]) > len(keys[j])
	})
	pairs := make([]string, 0, len(keys)*2)
	for _, key := range keys {
		pairs = append(pairs, "{"+key+"}", fields[key])
	}
	return strings.TrimSpace(strings.NewReplacer(pairs...).Replace(pattern))
}

func cloneFields(fields map[string]string) map[string]string {
	out := make(map[string]string, len(fields)+5)
	for key, value := range fields {
		out[key] = value
	}
	return out
}

func (m *Manager) signalUpdate() {
	select {
	case m.updates <- struct{}{}:
	default:
	}
}
