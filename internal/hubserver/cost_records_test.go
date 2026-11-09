package hubserver

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSpriteCostRecordsUsePostgresAndReplayImport(t *testing.T) {
	var logs bytes.Buffer
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})
	repository := projectRecordPostgres(t)
	service := openPostgresHubTestService(t, repository, Config{RunDetails: &memoryRunDetails{}, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	for _, name := range []string{"First example", "Second example"} {
		org, err := service.createNativeOrganizationFor(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		seedPostgresHubAdmin(t, repository, service, org)
		base := "/api/v2/organizations/" + string(org.ID) + "/projects"
		created := performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, map[string]any{"idempotency_key": "create", "name": "Example project", "grant_access": true, "states": nativeFixtureStates()})
		requireNativeStatus(t, created, http.StatusCreated)
		var project tracker.NativeProject
		decodeHubResponse(t, created, &project)
		base += "/" + string(project.ID) + "/usage/sprites"
		from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		observation := costTestObservation("cpu", from, from.Add(time.Hour))
		request := spriteUsageRequest{Mutation: tracker.Mutation{IdempotencyKey: "import"}, Observations: []costObservation{observation}}
		response := performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, request)
		requireNativeStatus(t, response, http.StatusOK)
		var result struct {
			Recorded int `json:"recorded"`
		}
		decodeHubResponse(t, response, &result)
		if result.Recorded != 1 {
			t.Fatalf("recorded=%d", result.Recorded)
		}
		replay := performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, request)
		requireNativeStatus(t, replay, http.StatusOK)
		if replay.Body.String() != response.Body.String() {
			t.Fatal("import replay changed response")
		}
		request.IdempotencyKey = "another-command"
		duplicate := performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, request)
		requireNativeStatus(t, duplicate, http.StatusOK)
		decodeHubResponse(t, duplicate, &result)
		if result.Recorded != 0 {
			t.Fatal("source replay counted twice")
		}
		request.IdempotencyKey = "conflicting-source"
		quantity := 3.0
		request.Observations[0].Quantity = &quantity
		conflict := performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, request)
		requireNativeStatus(t, conflict, http.StatusConflict)
		if err := repository.ReadTenant(t.Context(), org.ID, corestore.Serving, func(r tenantstore.Reader) error {
			spend, err := r.BillingMonthlySpend(t.Context(), project.ID, from)
			if err == nil && spend.SpriteMicros != 20000 {
				t.Errorf("sprite spend=%d", spend.SpriteMicros)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunnerCostCalculationUsesCanonicalAttribution(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reported := int64(1000000)
	facts := runnerUsageFacts{WorkItemID: "item_example", RunnerID: "runner_example", MachineID: "machine_example", Placement: "sprite", StartedAt: from, EndedAt: from.Add(time.Hour)}
	request := tracker.NativeRunEvent{Type: "run.finished", Data: tracker.NativeRunData{AttemptID: "attempt_example", Sequence: 2, MachineID: "untrusted_machine", RunnerID: "untrusted_runner", Usage: []tracker.NativeUsage{
		{Provider: "openai", Model: "first-model", BillingMode: "metered", CostEstimate: 2, ReportedCostMicros: &reported, CostSource: "backend_result", Currency: "USD", Input: 10, Output: 2},
		{Provider: "openai", Model: "second-model", BillingMode: "subscription", CostEstimate: 2, Input: 20, Output: 3},
	}}}
	observations, err := buildRunnerUsageObservations(request, UsageConfig{Currency: "USD"}, facts)
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 {
		t.Fatalf("observations=%d", len(observations))
	}
	for _, entry := range observations {
		o := entry.observation
		if o.RunnerID != facts.RunnerID || o.MachineID != facts.MachineID || o.WorkItemID != facts.WorkItemID || o.Placement != "sprite" || !o.From.Equal(from) || !o.To.Equal(from.Add(time.Hour)) || o.Revision != 2 {
			t.Fatalf("wrong canonical attribution: %+v", o)
		}
	}
	first, second := observations[0].observation, observations[1].observation
	if first.AmountMicros == nil || *first.AmountMicros != 1000000 || first.Basis != "runner_reported" || first.SourceID != "attempt_example:first-model" {
		t.Fatalf("metered cost lost reported evidence: %+v", first)
	}
	if second.AmountMicros != nil || second.EstimatedAmountMicros == nil || *second.EstimatedAmountMicros != 2000000 || second.SourceID != "attempt_example:second-model" {
		t.Fatalf("subscription estimate became metered spend: %+v", second)
	}
}

func TestRunnerCostPreparationPreservesArchivedEvidence(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := from.Add(time.Hour)
	objects := &memoryRunDetails{}
	service := &Service{config: Config{RunDetails: objects}}
	scope := nativeScope{organization: "org_example", project: "project_example"}
	amount := int64(42)
	request := tracker.NativeRunEvent{Type: "run.finished", Data: tracker.NativeRunData{AttemptID: "attempt_example", Sequence: 2, Usage: []tracker.NativeUsage{{Provider: "openai", Model: "example-model", Currency: "USD", BillingMode: "metered", ReportedCostMicros: &amount, CostSource: "backend_result", Input: 10}}}}
	facts := runnerUsageFacts{WorkItemID: "item_example", RunnerID: "runner_example", MachineID: "machine_example", Placement: "local", StartedAt: from, EndedAt: now}
	records, err := service.prepareRunnerCostRecords(t.Context(), scope, request, facts, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records=%d", len(records))
	}
	record := records[0]
	raw, err := objects.ReadDetail(t.Context(), string(scope.organization), record.Reference.ArchiveReference())
	if err != nil {
		t.Fatal(err)
	}
	var archived costObservation
	if err := json.Unmarshal(raw, &archived); err != nil {
		t.Fatal(err)
	}
	if archived.AmountMicros == nil || *archived.AmountMicros != 42 || archived.RunnerID != facts.RunnerID || archived.Basis != "runner_reported" || record.ProjectID != scope.project || !record.ReceivedAt.Equal(now) {
		t.Fatalf("archived cost lost attribution or evidence: %+v", archived)
	}
	objects.fail = true
	if _, err := service.prepareRunnerCostRecords(t.Context(), scope, request, facts, now); err == nil {
		t.Fatal("unavailable archive accepted")
	}
	objects.fail = false
	for _, test := range []struct {
		name   string
		mutate func(map[string][]byte, string)
	}{
		{name: "missing", mutate: func(data map[string][]byte, key string) { delete(data, key) }},
		{name: "corrupt", mutate: func(data map[string][]byte, key string) { data[key] = []byte("corrupt") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects.afterStore = func(ref attachment.ArchiveReference) {
				objects.mu.Lock()
				defer objects.mu.Unlock()
				test.mutate(objects.data, ref.Key)
			}
			if _, err := service.prepareRunnerCostRecords(t.Context(), scope, request, facts, now); err == nil {
				t.Fatal("invalid archived evidence accepted")
			}
			objects.afterStore = nil
		})
	}
	request.Type = "run.observed"
	if records, err := service.prepareRunnerCostRecords(t.Context(), scope, request, facts, now); err != nil || len(records) != 0 {
		t.Fatalf("unfinished run prepared costs: %d %v", len(records), err)
	}
}
