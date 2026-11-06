package hubserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// attemptDiffFixture is a claimed issue with a running attempt, which is the
// only producer the hub honours today.
type attemptDiffFixture struct {
	nativeFixture
	worker   string
	runnerID string
	issue    tracker.NativeIssue
	policy   string
	lease    tracker.NativeLease
	attempt  string
	run      string
}

func newAttemptDiffFixture(t *testing.T) *attemptDiffFixture {
	t.Helper()
	fixture := newClaimedAttemptDiffFixture(t)
	fixture.start(t)
	return fixture
}

// newClaimedAttemptDiffFixture claims the issue and stops before run.started,
// so a test can change the item between the claim and the start.
func newClaimedAttemptDiffFixture(t *testing.T) *attemptDiffFixture {
	t.Helper()
	return claimedAttemptDiffFixture(t, false, Config{})
}

func claimedAttemptDiffFixture(t *testing.T, enrolled bool, cfg Config) *attemptDiffFixture {
	t.Helper()
	cfg.DatabasePath = filepath.Join(t.TempDir(), "hub.db")
	service := openTestService(t, cfg)
	f := newNativeFixture(t, service, "", "attempt-diff")
	fixture := &attemptDiffFixture{nativeFixture: f}
	policy := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", policy)
	fixture.policy = policy.ID
	fixture.issue = f.create(t, "diff-work")
	machine := tracker.MachineID("diff-machine")
	if enrolled {
		r := prepareRunner(t, f, runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
		r.enroll(t)
		fixture.worker, machine, fixture.runnerID = r.redemption.Credential, r.binding.MachineID, r.binding.RunnerID
	} else {
		fixture.worker = f.worker(t, "attempt-diff-worker")
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", fixture.worker,
			map[string]any{"id": machine, "hostname": "fixture", "display_name": "Fixture", "version": "test", "capacity": 1}), http.StatusOK)
	}
	capabilities := []string{"native_issues", "scoped_collaboration"}
	if enrolled {
		capabilities = append(capabilities, tracker.NativeExecutionCapability)
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", fixture.worker,
		tracker.NativeClaim{PolicyID: fixture.policy, WorkItemID: fixture.issue.WorkItemID, MachineID: machine, SessionID: newNativeID("session"),
			TTLSeconds: 600, ProtocolMajor: 2, Capabilities: capabilities})
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &fixture.lease)
	fixture.attempt = newNativeID("attempt")
	fixture.run = newNativeID("run")
	return fixture
}

// start records run.started for the fixture's attempt.
func (f *attemptDiffFixture) start(t *testing.T) {
	t.Helper()
	f.event(t, "run.started", 1, "")
}

// event appends one run event for the fixture's attempt.
func (f *attemptDiffFixture) event(t *testing.T, kind string, sequence int64, outcome string) {
	t.Helper()
	event := tracker.NativeRunEvent{
		Mutation: tracker.Mutation{IdempotencyKey: newNativeID("event")}, Type: kind, SchemaVersion: 1,
		Data: tracker.NativeRunData{Sequence: sequence, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"},
			LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: outcome},
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost,
		f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, event), http.StatusOK)
}

func (f *attemptDiffFixture) producer() tracker.DiffProducer {
	return tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: f.attempt, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}
}

func (f *attemptDiffFixture) request(seq int64, files ...tracker.AttemptDiffFile) tracker.AttemptDiffRequest {
	return tracker.AttemptDiffRequest{
		Producer:   f.producer(),
		Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: seq},
		BaseSHA:    strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Files: files,
	}
}

func (f *attemptDiffFixture) post(t *testing.T, request tracker.AttemptDiffRequest) *httptest.ResponseRecorder {
	t.Helper()
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/attempts/"+f.attempt+"/diff", f.worker, request)
}

func (f *attemptDiffFixture) get(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	return performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/attempts/"+f.attempt+"/diff"+query, f.token, nil)
}

func requireNativeCode(t *testing.T, response *httptest.ResponseRecorder, status int, code string) nativeError {
	t.Helper()
	requireNativeStatus(t, response, status)
	var failure nativeError
	decodeHubResponse(t, response, &failure)
	if failure.Code != code {
		t.Fatalf("code = %q, want %q: %s", failure.Code, code, response.Body.String())
	}
	return failure
}

// The diff route's body allowance must hold a diff at the contract's limit at
// the worst case of JSON string escaping, or a diff the contract accepts is
// refused by the transport instead. Each byte class the encoder escapes costs
// at most six bytes, which is the factor the allowance is sized by.
func TestAttemptDiffBodyAllowanceCoversEscaping(t *testing.T) {
	t.Parallel()
	const sample = 1 << 10
	for _, test := range []struct {
		name string
		char string
	}{
		{name: "less than", char: "<"},
		{name: "greater than", char: ">"},
		{name: "ampersand", char: "&"},
		{name: "escape", char: "\x1b"},
		{name: "quote", char: "\""},
		{name: "newline", char: "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal(strings.Repeat(test.char, sample))
			if err != nil {
				t.Fatal(err)
			}
			if perByte := (len(encoded) - 2) / sample; perByte > 6 {
				t.Fatalf("%q encodes to %d bytes per byte, above the allowance factor", test.char, perByte)
			}
		})
	}
	if want := int64(6*tracker.MaxDiffBytes + (1 << 20)); maxAttemptDiffRequestBytes != want {
		t.Fatalf("allowance = %d, want %d", maxAttemptDiffRequestBytes, want)
	}
}
