package releaseaudit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureFingerprint = "source.go:change:missing-ownership-check"
const fixtureCoverage = "route::owner::api::access-control"

type memoryRecords struct {
	latest         map[string]Snapshot
	loaded         int
	publications   int
	failStatus     string
	failCandidates bool
}

func (m *memoryRecords) Load(_ context.Context, project string) ([]Snapshot, error) {
	var snapshots []Snapshot
	for _, snapshot := range m.latest {
		if snapshot.Identity.ProjectID == project {
			snapshots = append(snapshots, snapshot)
		}
	}
	m.loaded += len(snapshots)
	return snapshots, nil
}

func (m *memoryRecords) Publish(_ context.Context, state Snapshot) error {
	if state.Status == m.failStatus || m.failCandidates && state.Status == "incomplete" && len(state.Candidates) > 0 {
		return errors.New("Hub publication unavailable")
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	var clone Snapshot
	if err := json.Unmarshal(raw, &clone); err != nil {
		return err
	}
	if m.latest == nil {
		m.latest = map[string]Snapshot{}
	}
	m.latest[state.Identity.ProjectID+state.Identity.RunID] = clone
	m.publications++
	return nil
}

type recordedRepairs struct {
	keys           []string
	infrastructure int
	records        *memoryRecords
}

func (r *recordedRepairs) Confirmed(_ context.Context, identity Identity, key string, record json.RawMessage) error {
	if len(r.records.latest[identity.ProjectID+identity.RunID].Findings) != 1 {
		return errors.New("repair preceded evidence publication")
	}
	r.keys = append(r.keys, key)
	return nil
}

func (r *recordedRepairs) Infrastructure(context.Context, Identity, string) error {
	r.infrastructure++
	return nil
}

type fixtureSandbox struct{}

func (fixtureSandbox) Execute(context.Context, []string, string) (string, error) {
	return "The other owner's object changed.", nil
}

type fixtureProvider struct {
	malformedLedger bool
	repeatMalformed bool
	corrections     int
	t               *testing.T
	root            string
	verdict         string
	usage           Usage
	fail            string
	priorSeen       bool
	session         int
}

func (p *fixtureProvider) Invoke(ctx context.Context, invocation Invocation, observe func(Usage) error) (Response, error) {
	p.t.Helper()
	if !strings.Contains(invocation.Prompt, UntrustedContentRule) || !strings.Contains(invocation.Prompt, BundleCommit) {
		p.t.Fatal("provider prompt omitted trust boundary or pinned skill")
	}
	relative, err := filepath.Rel(p.root, invocation.Scratch)
	if err != nil || !filepath.IsLocal(relative) {
		p.t.Fatalf("scratch escaped job: %s", invocation.Scratch)
	}
	if invocation.Source.Commit() == "" {
		p.t.Fatal("source is not pinned")
	}
	source, err := invocation.Source.Read(ctx, "source.go")
	if err != nil || string(source) != "package example\n" {
		p.t.Fatalf("pinned source changed: %q %v", source, err)
	}
	if invocation.Phase == "recon-prior-evidence" && strings.Contains(invocation.Prompt, `"run_id":"first"`) {
		p.priorSeen = true
	}
	p.session++
	response := Response{SessionID: fmt.Sprintf("session-%d", p.session), Output: json.RawMessage(`{}`)}
	if err := observe(p.usage); err != nil {
		return response, err
	}
	if err := observe(p.usage); err != nil {
		return response, err
	}
	if invocation.Phase == p.fail {
		return response, errors.New("provider unavailable")
	}
	switch invocation.Phase {
	case "recon-ledger":
		response.Output = fixtureLedger("planned", p.verdict != "")
		if p.malformedLedger {
			response.Output = json.RawMessage(strings.Replace(string(response.Output), `"starting_paths":["source.go"]`, `"starting_paths":[]`, 1))
		}
	case "ledger-correction":
		p.corrections++
		if !strings.Contains(string(invocation.Input), "starting_paths") || !strings.Contains(string(invocation.Input), "must not be empty") {
			p.t.Fatal("correction omitted validator diagnostics")
		}
		response.Output = fixtureLedger("planned", p.verdict != "")
		if p.repeatMalformed {
			response.Output = json.RawMessage(strings.Replace(string(response.Output), `"starting_paths":["source.go"]`, `"starting_paths":[]`, 1))
		}
	case "hunt":
		status := "covered"
		candidates := []Candidate{}
		if p.verdict != "" {
			status = "candidate"
			candidates = append(candidates, Candidate{Fingerprint: fixtureFingerprint, CoverageID: fixtureCoverage, Record: fixtureFinding(p.verdict)})
		}
		response.Output, err = json.Marshal(struct {
			Ledger     json.RawMessage `json:"ledger"`
			Candidates []Candidate     `json:"candidates"`
		}{fixtureLedger(status, p.verdict != ""), candidates})
	case "coverage-critic", "final-coverage-critic":
		status := "covered"
		if p.verdict != "" {
			status = "candidate"
		}
		response.Output = fixtureLedger(status, p.verdict != "")
	case "candidate-disprover", "independent-record-verifier", "replacement-record-verifier":
		if invocation.Phase == "candidate-disprover" && invocation.Sandbox != nil {
			if _, err := invocation.Sandbox.Execute(ctx, []string{"fixture"}, invocation.Scratch); err != nil {
				return response, err
			}
		}
		response.Output = fixtureFinding(p.verdict)
	}
	return response, err
}

func fixtureLedger(status string, candidate bool) json.RawMessage {
	owner, checks, paths, fingerprints := "null", "[]", "[]", "[]"
	if status != "planned" {
		owner = `"agent-0006"`
		paths = `["source.go"]`
		checks = `[{"agent_id":"agent-0006","reviewed_paths":["source.go"],"invariant":"Only the owner changes an object.","method":"source","result":"The ownership check is absent.","artifact":null}]`
		if candidate {
			fingerprints = `["` + fixtureFingerprint + `"]`
		}
	}
	return json.RawMessage(fmt.Sprintf(`[{"coverage_id":"%s","canonical_refs":{"surface":"route","boundary":"owner","subsystem":"api","attack_class":"access-control"},"surface":"Route","boundary":"Object ownership","subsystem":"API","attack_class":"Access control","starting_paths":["source.go"],"ordinary_attack_class_block":"ATTACK-CLASSES.md#Access control","selected_companion_blocks":[],"excluded_blocks":[],"prior_status":"new","attempts":[],"wave":1,"status":"%s","agent_id":%s,"reviewed_paths":%s,"local_checks":%s,"result_fingerprints":%s,"unresolved":[]}]`, fixtureCoverage, status, owner, paths, checks, fingerprints))
}

func fixtureFinding(verdict string) json.RawMessage {
	base := `"fingerprint":"` + fixtureFingerprint + `","title":"Missing ownership check","description":"A user changes another owner's object.","trace":[{"kind":"entrypoint","file":"source.go","line":1,"scope":"change","description":"A user selects an object."},{"kind":"sink","file":"source.go","line":1,"scope":"change","description":"The object changes without checking its owner."}],"evidence":[{"file":"source.go","line":1,"description":"The operation omits the ownership check."}]`
	switch verdict {
	case "confirmed":
		return json.RawMessage(`{"verdict":"confirmed",` + base + `,"root_cause":"change omits an ownership check.","intended_behavior":"Only an object's owner changes it.","conditions":[],"execution":{"attacker_perspective":"An unprivileged user.","payloads":["Another owner's object ID."],"instructions":["Submit the ID through change."],"observed_result":"The other owner's object changed."},"remediation":{"strategy":"Require ownership before changing the object."},"severity":{"likelihood":{"score":"medium","reason":"The operation is reachable."},"impact":{"score":"medium","reason":"A protected object changes."},"overall_severity":"medium"},"confidence":{"score":"high","reason":"The bounded fixture reproduced the result."}}`)
	case "rejected":
		return json.RawMessage(`{"verdict":"rejected",` + base + `,"claimed_root_cause":"The operation was claimed to omit ownership.","reason":"The caller checks ownership before reaching the operation."}`)
	default:
		return json.RawMessage(`{"verdict":"needs_validation",` + base + `,"claimed_root_cause":"The operation may omit ownership.","blockers":["The bounded target execution is unavailable."],"validation_plan":{"local":"Run the isolated ownership fixture."}}`)
	}
}

func pinnedFixture(t *testing.T) *GitSource {
	t.Helper()
	repository := t.TempDir()
	git := func(arguments ...string) string {
		t.Helper()
		command := exec.Command("git", arguments...)
		command.Dir = repository
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + repository, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git fixture: %v: %s", err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(repository, "source.go"), []byte("package example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.go")
	git("-c", "user.name=Example", "-c", "user.email=example@example.test", "commit", "--no-gpg-sign", "-m", "fixture")
	commit := git("rev-parse", "HEAD")
	source, err := NewGitSource(t.Context(), repository, commit, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "source.go"), []byte("untrusted worktree edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	return source
}

func TestStageBoundaries(t *testing.T) {
	t.Parallel()
	source := pinnedFixture(t)
	for _, test := range []struct {
		malformedLedger, repeatMalformed bool
		corrections                      int
		name, verdict, want, fail        string
		baseCommit                       string
		failPublication                  string
		failCandidatePublication         bool
		ledgerStatus                     string
		retainedCandidates               bool
		sandbox                          bool
		invocations                      int
		usage                            Usage
		allowance                        Usage
		repairs, infrastructure          int
	}{
		{name: "confirmed", verdict: "confirmed", want: "findings", sandbox: true, repairs: 2},
		{name: "sandbox unavailable", verdict: "confirmed", want: "needs_validation"},
		{name: "needs validation", verdict: "needs_validation", want: "needs_validation"},
		{name: "rejected", verdict: "rejected", want: "clean"},
		{name: "clean", want: "clean"},
		{name: "ledger corrected", malformedLedger: true, corrections: 1, want: "clean"},
		{name: "invalid ledger remains rejected", malformedLedger: true, repeatMalformed: true, corrections: 2, want: "failed", infrastructure: 2},
		{name: "correction respects allowance", malformedLedger: true, corrections: 1, invocations: 8, want: "incomplete", ledgerStatus: "planned"},
		{name: "incremental identity retained", want: "clean", baseCommit: strings.Repeat("b", 40)},
		{name: "invocation allowance", verdict: "confirmed", sandbox: true, invocations: 8, want: "incomplete", ledgerStatus: "planned"},
		{name: "allowance after confirmed record", verdict: "confirmed", sandbox: true, invocations: 9, want: "incomplete", repairs: 2},
		{name: "allowance during verification", verdict: "confirmed", sandbox: true, allowance: Usage{Input: 25, Output: 100, Cached: 25}, want: "incomplete", ledgerStatus: "candidate", retainedCandidates: true},
		{name: "final publication failure", want: "failed", failPublication: "clean", infrastructure: 2},
		{name: "candidate publication failure", verdict: "confirmed", sandbox: true, want: "failed", failCandidatePublication: true, infrastructure: 2, retainedCandidates: true},
		{name: "usage allowance", usage: Usage{Input: 100, Output: 1}, allowance: Usage{Input: 550, Output: 50}, want: "incomplete", ledgerStatus: "planned"},
		{name: "provider failure", fail: "recon-surfaces", want: "failed", infrastructure: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			provider := &fixtureProvider{t: t, root: root, verdict: test.verdict, fail: test.fail, usage: test.usage, malformedLedger: test.malformedLedger, repeatMalformed: test.repeatMalformed}
			if provider.usage == (Usage{}) {
				provider.usage = Usage{Input: 3, Output: 2, Cached: 1}
			}
			records := &memoryRecords{failStatus: test.failPublication, failCandidates: test.failCandidatePublication}
			reporter := &recordedRepairs{records: records}
			var sandbox Sandbox
			if test.sandbox {
				sandbox = fixtureSandbox{}
			}
			stage, err := New(provider, records, reporter, sandbox)
			if err != nil {
				t.Fatal(err)
			}
			allowance := Allowance{Invocations: 30, Usage: Usage{Input: 10000, Output: 10000, Cached: 10000}}
			if test.invocations != 0 {
				allowance.Invocations = test.invocations
			}
			if test.allowance != (Usage{}) {
				allowance.Usage = test.allowance
			}
			for _, run := range []string{"first", "second"} {
				identity := Identity{ProjectID: "prj_example", Repository: "example-org/example-project", Commit: source.Commit(), ReleaseSeries: "release/v1.0", RunID: run, Attempt: "1", JobID: "audit", RunURL: "https://example.test/runs/" + run}
				provider.corrections = 0
				result, err := stage.Run(t.Context(), Request{BaseCommit: test.baseCommit, Identity: identity, Source: source, Scratch: root, Allowance: allowance})
				if provider.corrections != test.corrections {
					t.Fatalf("corrections=%d want=%d", provider.corrections, test.corrections)
				}
				if result.BaseCommit != test.baseCommit {
					t.Fatalf("scope base lost: %q", result.BaseCommit)
				}
				if result.Status != test.want {
					t.Fatalf("status=%s want=%s err=%v", result.Status, test.want, err)
				}
				if test.want == "incomplete" && !errors.Is(err, ErrAllowance) {
					t.Fatalf("allowance result: %v", err)
				}
				if test.want == "failed" && err == nil {
					t.Fatal("failed audit returned success to its release workflow")
				}
				if test.want != "incomplete" && test.want != "failed" && err != nil {
					t.Fatal(err)
				}
				if result.BundleCommit != "c1c8a8c1471069fb0e188eeaff69b8e8db6564a8" || result.BundleDigest != "9c550a639c12bacbb1e59b5893c7d86cd03145205b769ac5ab2c45ed7c794c73" {
					t.Fatal("stage did not use the reviewed bundle pin")
				}
				if result.Usage.Input != int64(result.Invocations)*provider.usage.Input {
					t.Fatalf("duplicate cumulative usage counted: %+v", result.Usage)
				}
				if result.Status == "failed" && (len(result.Findings) != 0 || len(result.Infrastructure) == 0) {
					t.Fatal("infrastructure failure became a finding")
				}
				for _, snapshot := range []Snapshot{result, records.latest[identity.ProjectID+run]} {
					if test.retainedCandidates && (len(snapshot.Candidates) != 1 || snapshot.Candidates[0].Fingerprint != fixtureFingerprint || snapshot.Candidates[0].CoverageID != fixtureCoverage) {
						t.Fatal("allowance exhaustion dropped the incomplete candidate")
					}
					if test.ledgerStatus != "" {
						var ledger []struct {
							ID           string   `json:"coverage_id"`
							Status       string   `json:"status"`
							Fingerprints []string `json:"result_fingerprints"`
						}
						if err := json.Unmarshal(snapshot.Ledger, &ledger); err != nil {
							t.Fatal(err)
						}
						if len(ledger) != 1 || ledger[0].ID != fixtureCoverage || ledger[0].Status != test.ledgerStatus || len(snapshot.Findings) != 0 {
							t.Fatalf("allowance exhaustion lost unfinished coverage: ledger=%s findings=%s", snapshot.Ledger, snapshot.Findings)
						}
						if test.retainedCandidates && (len(ledger[0].Fingerprints) != 1 || ledger[0].Fingerprints[0] != fixtureFingerprint) {
							t.Fatal("allowance exhaustion lost unresolved coverage fingerprint")
						}
					}
				}
				if test.failPublication == "" && records.latest[identity.ProjectID+run].Status != test.want {
					t.Fatal("final state was not published")
				}
			}
			if records.loaded != 1 || records.publications < 4 || test.want != "incomplete" && test.want != "failed" && !provider.priorSeen {
				t.Fatalf("second run did not hydrate project history: loaded=%d publications=%d prior=%v", records.loaded, records.publications, provider.priorSeen)
			}
			if len(reporter.keys) != test.repairs || reporter.infrastructure != test.infrastructure {
				t.Fatalf("reporting=%v infrastructure=%d", reporter.keys, reporter.infrastructure)
			}
			if len(reporter.keys) == 2 && reporter.keys[0] != reporter.keys[1] {
				t.Fatal("root cause key changed across occurrences")
			}
		})
	}
}

func TestConfirmationUsesCurrentCandidateExecution(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		executions []Observation
		want       string
	}{
		{name: "another candidate executed", want: "needs_validation"},
		{name: "current execution failed", executions: []Observation{{Succeeded: false}}, want: "needs_validation"},
		{name: "current execution succeeded", executions: []Observation{{Succeeded: true}}, want: "confirmed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			e := &execution{stage: &Stage{sandbox: fixtureSandbox{}}, state: Snapshot{Evidence: []Evidence{{Phase: "candidate-disprover", Executions: []Observation{{Succeeded: true}}}}}}
			raw, err := e.normalizeFinding(fixtureFinding("confirmed"), test.executions)
			if err != nil {
				t.Fatal(err)
			}
			var record struct {
				Verdict string `json:"verdict"`
			}
			if err := json.Unmarshal(raw, &record); err != nil {
				t.Fatal(err)
			}
			if record.Verdict != test.want {
				t.Fatalf("verdict=%s want=%s", record.Verdict, test.want)
			}
		})
	}
}

func TestLedgerCanonicalOrderPreservesUnitsAndRejectsDuplicates(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "bundle"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := prepareBundle(filepath.Join(directory, "bundle")); err != nil {
		t.Fatal(err)
	}
	e := &execution{directory: directory, stage: &Stage{records: &memoryRecords{}}, state: Snapshot{Status: "incomplete", Ledger: json.RawMessage("[]")}}
	var first []json.RawMessage
	if err := json.Unmarshal(fixtureLedger("planned", false), &first); err != nil {
		t.Fatal(err)
	}
	second := json.RawMessage(strings.ReplaceAll(strings.ReplaceAll(string(first[0]), "route", "zroute"), "Route", "Zroute"))
	raw, err := json.Marshal([]json.RawMessage{second, first[0]})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.updateLedger(t.Context(), raw); err != nil {
		t.Fatal(err)
	}
	var actual []map[string]any
	if err := json.Unmarshal(e.state.Ledger, &actual); err != nil {
		t.Fatal(err)
	}
	if len(actual) != 2 || actual[0]["coverage_id"] != fixtureCoverage || actual[1]["coverage_id"] != "zroute::owner::api::access-control" {
		t.Fatalf("ledger order or units changed: %s", e.state.Ledger)
	}
	for _, unit := range actual {
		if unit["status"] != "planned" || len(unit["local_checks"].([]any)) != 0 {
			t.Fatalf("canonical ordering changed review claims: %v", unit)
		}
	}
	duplicate, err := json.Marshal([]json.RawMessage{first[0], first[0]})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.updateLedger(t.Context(), duplicate); err == nil {
		t.Fatal("duplicate coverage identity accepted")
	}
	if err := e.updateLedger(t.Context(), fixtureLedger("planned", false)); err == nil {
		t.Fatal("canonical serialization dropped prior coverage")
	}
}

func TestScopedCoverageRequiresCompleteInScopeEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, base, ledger string
		want               bool
	}{
		{"full review cannot exclude", "", `[{"status":"covered"},{"status":"out_of_scope"}]`, false},
		{"incremental review retains exclusions", strings.Repeat("b", 40), `[{"status":"covered"},{"status":"out_of_scope"}]`, true},
		{"cannot exclude everything", strings.Repeat("b", 40), `[{"status":"out_of_scope"}]`, false},
		{"planned unit incomplete", strings.Repeat("b", 40), `[{"status":"planned"},{"status":"out_of_scope"}]`, false},
		{"unresolved excluded finding", strings.Repeat("b", 40), `[{"status":"covered"},{"status":"out_of_scope","result_fingerprints":["unresolved"]}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &execution{request: Request{BaseCommit: tc.base}, state: Snapshot{Ledger: json.RawMessage(tc.ledger)}}
			if got := e.coverageComplete(); got != tc.want {
				t.Fatalf("coverageComplete()=%v want %v", got, tc.want)
			}
		})
	}
}
