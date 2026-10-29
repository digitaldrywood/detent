package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type changeWriterMeasurementKey struct{}

type changeWriterMeasurement struct {
	statements int
	content    int
	active     bool
}

func TestChangeMutationsRejectStalePreparation(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"publish", "review"} {
		for _, changed := range []string{"change", "issue", "repository policy", "review policy", "content policy", "evidence"} {
			t.Run(operation+"/"+changed, func(t *testing.T) {
				f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")}))
				version := f.publish(t, "initial", "")
				change := f.detail(t).Change
				path := f.path + "/versions"
				var payload any = tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "stale-preparation"}, ExpectedVersionID: version.ID, ChangeVersionInput: changeTestInput()}
				if operation == "review" {
					path += "/" + version.ID + "/reviews"
					payload = tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "stale-preparation"}, ExpectedRevision: change.Revision, ExpectedVersionID: version.ID, Decision: "approved"}
				}
				injected := false
				ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, _ []driver.NamedValue) {
					if injected || !strings.Contains(query, "FROM change_requests c JOIN issues i") {
						return
					}
					injected = true
					var err error
					switch changed {
					case "change":
						change.Revision++
						raw, marshalErr := json.Marshal(change)
						if marshalErr != nil {
							t.Fatal(marshalErr)
						}
						_, err = f.service.database.db.ExecContext(t.Context(), "UPDATE change_requests SET record_json=? WHERE id=?", string(raw), change.ID)
					case "issue":
						_, err = f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET revision=revision+1 WHERE native_id=?", f.issue.WorkItemID)
					case "repository policy":
						_, err = f.service.database.db.ExecContext(t.Context(), "DELETE FROM project_policies WHERE scope=?", string(f.project.OrganizationID)+"/"+string(f.project.ID))
					case "content policy":
						_, err = f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET require_visual_attachments=NOT require_visual_attachments, content_settings_revision=content_settings_revision+1 WHERE organization_id=? AND id=?", f.project.OrganizationID, f.project.ID)
					case "review policy":
						rules := f.rules
						rules.RequireReview = false
						rules.ID = changerequest.PolicyID(rules)
						raw, marshalErr := json.Marshal(rules)
						if marshalErr != nil {
							t.Fatal(marshalErr)
						}
						_, err = f.service.database.db.ExecContext(t.Context(), "UPDATE change_review_policies SET policy_json=? WHERE organization_id=? AND project_id=?", string(raw), f.project.OrganizationID, f.project.ID)
					case "evidence":
						review := tracker.ChangeReview{ID: newNativeID("review"), VersionID: version.ID, Decision: "changes_requested", Actor: tracker.Actor{Kind: "human", PrincipalID: f.rules.RequiredChecks[0].PrincipalID}}
						err = f.service.hubTransact(t.Context(), func(tx *sql.Tx, _ time.Time) error {
							return insertChangeEvidence(t.Context(), tx, change.ID, version.ID, "review", "", review)
						})
					}
					if err != nil {
						t.Fatal(err)
					}
				}})
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, newHubAPIRequest(t, http.MethodPost, path, f.token, payload).WithContext(ctx))
				requireNativeStatus(t, response, http.StatusConflict)
				if !injected {
					t.Fatal("concurrent mutation was not injected")
				}
				var receipts, versions int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_commands WHERE command_key='stale-preparation'").Scan(&receipts); err != nil {
					t.Fatal(err)
				}
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM change_versions WHERE change_id=?", change.ID).Scan(&versions); err != nil {
					t.Fatal(err)
				}
				if receipts != 0 || versions != 1 {
					t.Fatalf("stale mutation committed: receipts=%d versions=%d", receipts, versions)
				}
			})
		}
	}
}

func TestChangeMutationsBoundWriterWork(t *testing.T) {
	t.Parallel()
	for _, files := range []int{1, 128} {
		t.Run(fmt.Sprintf("files_%d", files), func(t *testing.T) {
			objects := &changeObjectFixture{}
			service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), Content: objects.call})
			f := newChangeFixture(t, service)
			input := changeTestInput()
			bundle := changeMutationSource(t, &input, files)
			input.Validation = &gate.CommandResult{Command: "go test ./internal/example", HeadSHA: input.HeadSHA, TreeSHA: strings.Repeat("d", 40), DurationNS: 1, Output: strings.Repeat("command output\n", 2000)}
			expected := ""
			versions := 1
			if files > 1 {
				versions = 12
			}
			var version tracker.ChangeVersion
			for i := range versions {
				request := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("bounded-%d", i)}, ExpectedVersionID: expected, ChangeVersionInput: input, SourceBundle: bundle}
				response := performHubAPIRequest(t, service, http.MethodPost, f.path+"/versions", f.token, request)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &version)
				expected = version.ID
			}
			for _, operation := range []string{"publish", "review"} {
				t.Run(operation, func(t *testing.T) {
					path := f.path + "/versions"
					var payload any = tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "measured-version"}, ExpectedVersionID: expected, ChangeVersionInput: input, SourceBundle: bundle}
					if operation == "review" {
						path += "/" + version.ID + "/reviews"
						payload = tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "measured-review"}, ExpectedVersionID: version.ID, Decision: "approved", Body: "Reviewed source"}
					}
					measurement := &changeWriterMeasurement{}
					ctx := context.WithValue(t.Context(), changeWriterMeasurementKey{}, measurement)
					ctx = withRequestTiming(ctx, &requestTiming{writerTrace: func(_ string, _ []driver.NamedValue) {
						measurement.statements++
					}, writerTransaction: func(active bool) { measurement.active = active }})
					request := newHubAPIRequest(t, http.MethodPost, path, f.token, payload).WithContext(ctx)
					response := httptest.NewRecorder()
					service.Handler().ServeHTTP(response, request)
					requireNativeStatus(t, response, http.StatusOK)
					if operation == "publish" {
						decodeHubResponse(t, response, &version)
					}
					if measurement.content != 0 || measurement.statements > 30 {
						t.Fatalf("%s writer statements=%d (bound 30), content operations=%d (want 0)", operation, measurement.statements, measurement.content)
					}
					t.Logf("%s: files=%d, historical versions=%d, writer statements=%d, content operations=%d", operation, files, versions, measurement.statements, measurement.content)
				})
			}
		})
	}
}

func changeMutationSource(t *testing.T, input *tracker.ChangeVersionInput, files int) []byte {
	t.Helper()
	bundle, directory := changeTestSourceCheckout(t, input)
	if files == 1 {
		return bundle
	}
	git := func(args ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", append([]string{"-C", directory, "-c", "user.name=Source Fixture", "-c", "user.email=source@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("source fixture git %v: %s: %v", args, output, err)
		}
		return string(output)
	}
	for i := 1; i < files; i++ {
		if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("file-%03d.txt", i)), []byte(strings.Repeat(fmt.Sprintf("source %d\n", i), 1000)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "-m", "multi-file source")
	input.HeadSHA = strings.TrimSpace(git("rev-parse", "HEAD"))
	bundlePath := filepath.Join(directory, "multi.bundle")
	git("bundle", "create", bundlePath, "HEAD", "^"+input.BaseSHA)
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	diff := git("diff", "--raw", "--no-abbrev", "--no-renames", "--no-ext-diff", "--no-textconv", "-z", input.BaseSHA, input.HeadSHA, "--")
	input.Source.HeadSHA, input.Source.BundleSHA256, input.Source.DiffSHA256, input.Source.Bytes = input.HeadSHA, tracker.ChangeSourceDigest(bundle), tracker.ChangeSourceDigest([]byte(diff)), int64(len(bundle))
	return bundle
}

func TestQualityEscapePreparesHistoryOutsideWriter(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"legacy inline", "legacy archive", "concurrent assessment", "changed landing", "human override"} {
		t.Run(scenario, func(t *testing.T) {
			objects := &changeObjectFixture{}
			f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), Content: objects.call}))
			version := f.publish(t, "initial", "")
			now := f.service.config.now()
			landing := tracker.ChangeLanding{VersionID: version.ID, MergeSHA: strings.Repeat("d", 40), LandedAt: now.Add(-time.Hour), Quality: &tracker.LandingQualitySnapshot{IssueBody: "Accept valid input", Criteria: []string{"Accept valid input"}}}
			raw, err := json.Marshal(landing)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO quality_landings(version_id,record_json) VALUES(?,?)", version.ID, string(raw)); err != nil {
				t.Fatal(err)
			}
			escape := tracker.QualityEscape{OccurrenceID: strings.Repeat("e", 40), Kind: "revert", ObservedAt: now.Add(-time.Minute), Cause: "missing_criterion", EvidenceReference: "revert_commit", EvidenceQuote: "This reverts commit " + landing.MergeSHA, BasisQuote: "Accept valid input"}
			prior := escape
			prior.HumanOverride = scenario == "human override"
			discussion := tracker.ChangeDiscussion{ID: "prior", VersionID: version.ID, Escape: &prior}
			data, err := json.Marshal(discussion)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(storageJSONPayload{Organization: string(f.project.OrganizationID), Project: string(f.project.ID), Family: "change_evidence", Row: 1, Data: data})
			if err != nil {
				t.Fatal(err)
			}
			ref, err := prepareChangeContent(f.service.contentContext(t.Context()), string(f.project.OrganizationID), payload)
			if err != nil {
				t.Fatal(err)
			}
			envelope, err := json.Marshal(map[string]any{"_storage_archive": storageJSONReference{Organization: string(f.project.OrganizationID), Project: string(f.project.ID), Family: "change_evidence", Row: 1, Reference: ref}})
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "legacy archive" && scenario != "human override" {
				envelope = data
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(t.Context(), "INSERT INTO change_evidence(change_id,version_id,kind,record_json) VALUES(?,?,'discussion',?)", f.change.ID, version.ID, string(envelope)); err != nil {
				t.Fatal(err)
			}
			for range claimEvidenceLimit + 1 {
				if _, err := tx.ExecContext(t.Context(), "INSERT INTO change_evidence(change_id,version_id,kind,record_json) VALUES(?,?,'discussion','{}')", f.change.ID, version.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			measurement := &changeWriterMeasurement{}
			injected, historyInWriter := false, false
			ctx := context.WithValue(t.Context(), changeWriterMeasurementKey{}, measurement)
			ctx = withRequestTiming(ctx, &requestTiming{
				writerTransaction: func(active bool) { measurement.active = active },
				writerTrace: func(query string, _ []driver.NamedValue) {
					if strings.Contains(query, "escape_occurrence_id") && strings.Contains(query, "SELECT record_json") {
						historyInWriter = true
					}
				},
				trace: func(query string, _ []driver.NamedValue) {
					if (scenario != "concurrent assessment" && scenario != "changed landing") || injected || !strings.Contains(query, "FROM change_requests c JOIN issues i") {
						return
					}
					injected = true
					if scenario == "changed landing" {
						if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE quality_landings SET record_json='{}' WHERE version_id=?", version.ID); err != nil {
							t.Fatal(err)
						}
						return
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO change_evidence(change_id,version_id,kind,record_json) VALUES(?,?,'discussion',?)", f.change.ID, version.ID, string(data)); err != nil {
						t.Fatal(err)
					}
				},
			})
			request := tracker.DiscussChange{Mutation: tracker.Mutation{IdempotencyKey: "prepared-escape"}, VersionID: version.ID, Body: "Assessment", Escape: &escape}
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, newHubAPIRequest(t, http.MethodPost, f.path+"/discussion", f.token, request).WithContext(ctx))
			want := http.StatusOK
			if scenario == "concurrent assessment" || scenario == "changed landing" {
				want = http.StatusConflict
			}
			if scenario == "human override" {
				want = http.StatusUnprocessableEntity
			}
			requireNativeStatus(t, response, want)
			var receipts int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_commands WHERE command_key='prepared-escape'").Scan(&receipts); err != nil {
				t.Fatal(err)
			}
			if want != http.StatusOK && receipts != 0 {
				t.Fatalf("rejected assessment saved %d receipts", receipts)
			}
			if measurement.content != 0 || historyInWriter {
				t.Fatalf("writer scanned history=%v or accessed %d objects", historyInWriter, measurement.content)
			}
			if (scenario == "concurrent assessment" || scenario == "changed landing") && !injected {
				t.Fatal("concurrent assessment was not injected")
			}
		})
	}
}
