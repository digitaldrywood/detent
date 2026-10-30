package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatorfailure"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func BenchmarkHotSQL(b *testing.B) {
	f := benchmarkAnalyticsFixture(b, 0)
	q := f.service.database.reader
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	now := f.service.config.now()
	at := formatHubTime(now.Add(-time.Minute))
	var item string
	var issue int64
	if err := q.QueryRowContext(b.Context(), "SELECT id,native_id FROM issues WHERE project_id=?", scope.project).Scan(&issue, &item); err != nil {
		b.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(b.Context(), `INSERT INTO machines(id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES('bench-machine','example-runner',1,'test',?,?,?)`, at, at, at); err != nil {
		b.Fatal(err)
	}
	tx, err := f.service.database.db.BeginTx(b.Context(), nil)
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	payload := `{"runtime":{"local_attempt_id":123,"heartbeat_at":"` + at + `"},"terminal_failure":{"error":"protocol error","stage":"pre_provider","attribution":"instance"},"output":"` + strings.Repeat("x", 4096) + `"}`
	for i := range 10000 {
		id := fmt.Sprintf("bench-%05d", i)
		result, err := tx.ExecContext(b.Context(), `INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at) VALUES(?,?,'bench-machine',?,?, ?,?,?,?,?)`, id, issue, id, at, at, at, at, at, at)
		if err != nil {
			b.Fatal(err)
		}
		fence, err := result.LastInsertId()
		if err != nil {
			b.Fatal(err)
		}
		if _, err := tx.ExecContext(b.Context(), `INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,started_at,updated_at,hot_projection_version,local_attempt_id,heartbeat_at,health_failure_signature,health_instance_failure) VALUES(?,?,?,?,?,?,?,1,'failed',?,?,?,1,?,?,?,1)`, id, scope.organization, scope.project, item, id, fence, id, strings.Replace(payload, `"local_attempt_id":123`, `"local_attempt_id":`+strconv.Itoa(i+1), 1), at, at, i+1, at, "protocol error"); err != nil {
			b.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(b.Context(), `UPDATE native_attempts SET health_updated_day=julianday(updated_at)`); err != nil {
		b.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	var token string
	if err := q.QueryRowContext(b.Context(), "SELECT id FROM api_tokens LIMIT 1").Scan(&token); err != nil {
		b.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(b.Context(), `INSERT INTO runner_enrollments(id,organization_id,runner_id,machine_id,token_hash,operations_json,created_at,expires_at,created_by) VALUES('bench-enrollment',?,'bench-runner','bench-machine',?,'[]',?,?,?)`, scope.organization, strings.Repeat("0", 64), at, formatHubTime(now.Add(time.Hour)), token); err != nil {
		b.Fatal(err)
	}
	view := runnerauth.ProjectConfiguration{ProjectID: string(scope.project), ObservedAt: now, Diagnostics: &runnerauth.ProjectDiagnostics{ObservedAt: now, RuntimeObservedAt: now, Records: []runnerauth.DiagnosticAttempt{{Key: "local:9999", IssueID: item, LocalAttemptID: 9999, Generation: 1, Stage: "implementing", Membership: []string{"running"}, LeaseRenewedAt: now, LatestError: strings.Repeat("x", 7000)}}}}
	projects := map[string]runnerauth.ProjectConfiguration{string(scope.project): view}
	for i := range 99 {
		other := view
		other.ProjectID = fmt.Sprintf("project-%d", i)
		projects[other.ProjectID] = other
	}
	document, err := json.Marshal(projects)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(b.Context(), `INSERT INTO runner_identities(id,organization_id,machine_id,token_id,enrollment_id,operations_json,created_at,capacity_limit,reported_capacity,last_heartbeat_at,project_configuration_json) VALUES('bench-runner',?,'bench-machine',?,'bench-enrollment','[]',?,1,1,?,?)`, scope.organization, token, at, at, string(document)); err != nil {
		b.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(b.Context(), `INSERT INTO token_grants(token_id,organization_id,project_id) VALUES(?,?,?)`, token, scope.organization, scope.project); err != nil {
		b.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(b.Context(), `UPDATE leases SET released_at=NULL,acquired_at=?,renewed_at=?,expires_at=? WHERE lease_id='bench-09999'`, at, at, formatHubTime(now.Add(time.Hour))); err != nil {
		b.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(b.Context(), `INSERT INTO lease_runners(lease_id,runner_id) VALUES('bench-09999','bench-runner')`); err != nil {
		b.Fatal(err)
	}
	tx, err = f.service.database.db.BeginTx(b.Context(), nil)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := tx.ExecContext(b.Context(), `INSERT INTO runner_project_observations(organization_id,runner_id,project_id,observed_at,diagnostics_observed_at,runtime_observed_at,snapshot_digest) VALUES(?,'bench-runner',?,?,?,?,'bench-snapshot')`, scope.organization, scope.project, formatHubTime(now), formatHubTime(now), formatHubTime(now)); err != nil {
		b.Fatal(err)
	}
	if err := storeRunnerBoardProjection(b.Context(), tx, scope, "bench-runner", view); err != nil {
		b.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	benchmarkHotSQLMutations(b, f, scope, item, view, now)
	for _, typed := range []bool{false, true} {
		b.Run(fmt.Sprintf("configuration/typed=%t", typed), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var raw string
				if typed {
					entry, err := readProjectConfigurationEntry(b.Context(), q, scope, "bench-runner")
					if err != nil {
						b.Fatal(err)
					}
					raw = entry.String
				} else {
					if err := q.QueryRowContext(b.Context(), `SELECT json_extract(project_configuration_json,?) FROM runner_identities WHERE organization_id=? AND id='bench-runner'`, "$."+jsonPathKey(string(scope.project)), scope.organization).Scan(&raw); err != nil {
						b.Fatal(err)
					}
				}
				var result runnerauth.ProjectConfiguration
				if err := json.Unmarshal([]byte(raw), &result); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("board/typed=%t", typed), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var err error
				if typed {
					_, err = readNativeBoardExecution(b.Context(), q, scope, item, nil, now)
				} else {
					_, err = readJSONNativeBoardExecution(b.Context(), q, scope, item, nil, now)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	window := operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now}
	want, err := readJSONHealthRateAttempts(b.Context(), q, scope, window)
	if err != nil {
		b.Fatal(err)
	}
	got, err := readHealthRateAttempts(b.Context(), q, scope, window)
	if err != nil {
		b.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		b.Fatal("health benchmark projections differ")
	}
	for _, path := range []struct {
		name string
		read func(context.Context, nativeQueryer, nativeScope, operatortool.AnalyticsWindow) ([]healthRateAttempt, error)
	}{{"json", readJSONHealthRateAttempts}, {"typed", readHealthRateAttempts}} {
		b.Run("health/"+path.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := path.read(b.Context(), q, scope, window); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	b.Run("health/productivity", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := readHealthProductivityAttempts(b.Context(), q, scope, got); err != nil {
				b.Fatal(err)
			}
			healthProductivityFindings(now, healthRateProject{ID: string(scope.project), Attempts: got})
		}
	})
	for _, typed := range []bool{false, true} {
		b.Run(fmt.Sprintf("runtime/typed=%t", typed), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if typed {
					if _, err := readLocalAttemptMatches(b.Context(), q, scope, item, 9999); err != nil {
						b.Fatal(err)
					}
				} else {
					if _, err := readJSONLocalAttemptMatches(b.Context(), q, scope, item, 9999); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
func readJSONHealthRateAttempts(ctx context.Context, q nativeQueryer, scope nativeScope, window operatortool.AnalyticsWindow) ([]healthRateAttempt, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.id,a.work_item_id,a.status,a.updated_at,
 COALESCE((SELECT lr.runner_id FROM lease_runners lr WHERE lr.lease_id=a.lease_id),''),
 json_object('terminal_failure',json_extract(a.data_json,'$.terminal_failure'),'finalization',json_extract(a.data_json,'$.finalization'),'runtime',json_object('heartbeat_at',json_extract(a.data_json,'$.runtime.heartbeat_at'),'landing',json_extract(a.data_json,'$.runtime.landing'))),
 (SELECT coalesce(sum(u.cost_estimate),0) FROM attempt_usage u WHERE u.organization_id=a.organization_id AND u.project_id=a.project_id AND u.attempt_id=a.id AND u.currency='USD' AND julianday(u.updated_at)<=julianday(?))
 FROM native_attempts a JOIN projects p ON p.id=a.project_id
 WHERE a.organization_id=? AND a.project_id=? AND p.profile='native'
 AND (julianday(a.updated_at)>=julianday(?) OR julianday(json_extract(a.data_json,'$.runtime.landing.observed_at'))>=julianday(?))
 AND julianday(a.started_at)<julianday(?) ORDER BY a.fencing_token LIMIT ?`, formatHubTime(window.To), scope.organization, scope.project, formatHubTime(window.From), formatHubTime(window.From), formatHubTime(window.To), healthReadLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []healthRateAttempt{}
	landed := map[string]bool{}
	for rows.Next() {
		if len(out) == healthReadLimit {
			return nil, errHealthReadLimit
		}
		var a healthRateAttempt
		var at, raw string
		if err := rows.Scan(&a.ID, &a.Item, &a.Status, &at, &a.Runner, &raw, &a.Cost); err != nil {
			return nil, err
		}
		if a.At, err = parseTimeValue(at); err != nil {
			return nil, err
		}
		var data tracker.NativeRunData
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			return nil, err
		}
		if f := data.TerminalFailure; a.Status == "failed" && f != nil && f.Stage == "pre_provider" && f.Attribution == "instance" {
			a.InstanceSetupFailure = true
		}
		text, _ := operatorfailure.FromAttempt(data, a.Status)
		a.Recovery = healthRecoveryReason(text)
		if text != "" && a.Recovery == "" {
			a.Signature = operatorfailure.Normalize(text)
		}
		if data.Runtime != nil {
			a.Heartbeat = data.Runtime.HeartbeatAt
			if l := data.Runtime.Landing; l != nil && (l.Landed || l.RefusalKind != "") {
				a.Landing, a.Landed, a.Conflict = true, l.Landed, !l.Landed && l.RefusalKind == "conflict"
				a.LandingAt = l.ObservedAt
				if a.LandingAt.IsZero() {
					a.LandingAt = a.At
				}
				if l.Landed && l.ChangeID != "" {
					key := l.ChangeID + "\x00" + l.VersionID + "\x00" + l.MergeSHA
					a.LandingKey = key
					if landed[key] {
						a.Landing, a.Landed = false, false
					}
					landed[key] = true
				}
			}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func readJSONNativeBoardExecution(ctx context.Context, q nativeQueryer, scope nativeScope, item string, latest *tracker.NativeAttempt, now time.Time) (*tracker.NativeBoardExecution, error) {
	var lease, acquired, expires, runner, configuration string
	var fence uint64
	err := q.QueryRowContext(ctx, `SELECT l.lease_id,l.fencing_token,l.acquired_at,l.expires_at,coalesce(r.id,''),coalesce(json_extract(r.project_configuration_json,?), '{}')
FROM issues i JOIN leases l ON l.issue_id=i.id
LEFT JOIN lease_runners lr ON lr.lease_id=l.lease_id
LEFT JOIN runner_identities r ON r.id=lr.runner_id AND r.organization_id=i.organization_id
AND EXISTS(SELECT 1 FROM token_grants g WHERE g.token_id=r.token_id AND g.project_id=i.project_id)
WHERE i.organization_id=? AND i.project_id=? AND i.native_id=? AND l.released_at IS NULL
AND julianday(l.renewed_at)<=julianday(?) AND julianday(l.expires_at)>julianday(?)`, "$."+jsonPathKey(string(scope.project)), scope.organization, scope.project, item, formatHubTime(now), formatHubTime(now)).Scan(&lease, &fence, &acquired, &expires, &runner, &configuration)
	if err != nil {
		return nil, err
	}
	started, err := parseTimeValue(acquired)
	if err != nil {
		return nil, err
	}
	expiry, err := parseTimeValue(expires)
	if err != nil {
		return nil, err
	}
	execution := &tracker.NativeBoardExecution{ID: lease, RunnerID: runner, ExpiresAt: expiry}
	terminal := false
	if latest != nil && string(latest.LeaseID) == lease {
		execution.ID, execution.Running, execution.Identity, execution.StartedAt = latest.AttemptID, latest.Status == "running", nativeAttemptDisplayIdentity(*latest), &latest.StartedAt
		terminal = latest.Status != "running"
		if latest.Runtime != nil {
			execution.Stage = latest.Runtime.Phase
			if execution.Stage == "completed" {
				execution.Running, execution.Stage, terminal = false, "", true
			}
		}
		if terminal {
			execution.Running, execution.Stage = false, ""
		}
	}
	var view runnerauth.ProjectConfiguration
	if err := json.Unmarshal([]byte(configuration), &view); err != nil {
		return nil, err
	}
	if err := overlayProjectConfigurationObservation(ctx, q, scope, runner, &view); err != nil {
		return nil, err
	}
	d := view.Diagnostics
	if d == nil || view.ProjectID != string(scope.project) || d.ObservedAt.Before(started) || d.ObservedAt.After(now) {
		return execution, nil
	}
	asOf := d.RuntimeObservedAt
	if asOf.IsZero() {
		asOf = d.ObservedAt
	}
	fresh := !asOf.Before(started) && !asOf.After(now) && now.Sub(asOf) <= runnerauth.HeartbeatTimeout && now.Sub(d.ObservedAt) <= runnerauth.HeartbeatTimeout
	var local *runnerauth.DiagnosticAttempt
	var claim *runnerauth.DiagnosticAttempt
	count := 0
	for i := range d.Records {
		record := &d.Records[i]
		if record.IssueID != item || record.Fence != 0 && record.Fence != fence {
			continue
		}
		if record.Fence == fence && slices.Contains(record.Membership, "native_claim") {
			claim = record
		}
		if record.LocalAttemptID > 0 && record.Generation > 0 && slices.Contains(record.Membership, "running") && !record.LeaseRenewedAt.Before(started) && (latest == nil || string(latest.LeaseID) != lease || record.NativeAttemptID == "" || record.NativeAttemptID == latest.AttemptID) {
			local = record
			count++
		}
	}
	if count == 1 && local != nil && !terminal && fresh {
		execution.ID = runner + "/" + strconv.FormatInt(local.LocalAttemptID, 10) + "/" + strconv.FormatUint(local.Generation, 10)
		if local.NativeAttemptID != "" {
			execution.ID = local.NativeAttemptID
		} else if claim != nil && claim.NativeAttemptID != "" {
			execution.ID = claim.NativeAttemptID
		}
		execution.Running, execution.Stage = true, local.Stage
		execution.ExpiresAt = minTime(execution.ExpiresAt, asOf.Add(runnerauth.HeartbeatTimeout))
	}
	if count != 1 {
		local = nil
	}
	completed := local != nil && !local.ProviderCompletedAt.Before(started) && !local.ProviderCompletedAt.After(now) || claim != nil && !claim.ProviderCompletedAt.Before(started) && !claim.ProviderCompletedAt.After(now)
	if completed {
		execution.Running, execution.Stage = false, ""
		if fresh && (claim != nil && claim.OperationPending && claim.HostOperation == "workspace.finalize_native_work" || local != nil && local.OperationPending && local.HostOperation == "workspace.finalize_native_work") {
			execution.Stage = "finalizing"
		}
	}
	return execution, nil
}

func readJSONLocalAttemptMatches(ctx context.Context, q nativeQueryer, scope nativeScope, item string, local int64) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND json_extract(data_json,'$.runtime.local_attempt_id')=? UNION SELECT subject_id FROM native_archive_sections WHERE organization_id=? AND project_id=? AND work_item_id=? AND kind='run' AND local_attempt_id=? LIMIT 2`, scope.organization, scope.project, item, local, scope.organization, scope.project, item, local)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var matches []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		matches = append(matches, id)
	}
	return matches, rows.Err()
}
