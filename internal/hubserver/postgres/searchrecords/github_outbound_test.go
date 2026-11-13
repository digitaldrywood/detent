package searchrecords_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestGitHubOutboundFencesExpiredClaimsAcrossTenants(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"first", "second"} {
		key, err := attachment.ArchiveKey(string(org), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		if err != nil {
			t.Fatal(err)
		}
		payload := eventstore.Receipt{OrganizationID: org, PayloadObjectKey: key, PayloadSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PayloadBytes: 1, PayloadContentBytes: 1}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), CreatedAt: now}); err != nil {
				return err
			}
			for _, project := range []tracker.ProjectID{"one", "two"} {
				if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: project, Name: string(project), Profile: "native"}); err != nil {
					return err
				}
				out := projectstore.GitHubOutbound{ProjectID: project, WorkItemID: "wi_example", Key: string(project), Repository: "example-org/example-project", IssueNumber: 1, Kind: "workpad", CreatedAt: now, Payload: payload}
				for range 2 {
					if err := w.(projectstore.GitHubOutboxWriter).EnqueueGitHubOutbound(t.Context(), out); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var expired projectstore.GitHubOutbound
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			owner := w.(projectstore.GitHubOutboxWriter)
			var err error
			expired, err = owner.ClaimGitHubOutbound(t.Context(), now, now.Add(time.Minute), "old")
			if err != nil {
				return err
			}
			if expired.Key != "one" || expired.Payload.OrganizationID != org {
				t.Fatalf("first claim=%+v", expired)
			}
			second, err := owner.ClaimGitHubOutbound(t.Context(), now, now.Add(time.Minute), "second")
			if err != nil {
				return err
			}
			if second.Key != "two" {
				t.Fatalf("unexpired claim was reclaimed: %+v", second)
			}
			second.Status = "completed"
			second.Outcome = "committed"
			return owner.FinishGitHubOutbound(t.Context(), second, now)
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			owner := w.(projectstore.GitHubOutboxWriter)
			if err := owner.RenewGitHubOutbound(t.Context(), expired.ID, expired.LeaseID, now.Add(30*time.Second), now.Add(2*time.Minute), true); err != nil {
				return err
			}
			if _, err := owner.ClaimGitHubOutbound(t.Context(), now.Add(time.Minute), now.Add(3*time.Minute), "early"); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("renewed claim was reclaimed: %v", err)
			}
			current, err := owner.ClaimGitHubOutbound(t.Context(), now.Add(2*time.Minute), now.Add(3*time.Minute), "current")
			if err != nil {
				return err
			}
			if current.ID != expired.ID || current.Attempts != 2 || current.Outcome != "uncertain" {
				t.Fatalf("recovered=%+v", current)
			}
			expired.Status = "completed"
			expired.Outcome = "committed"
			if err := owner.FinishGitHubOutbound(t.Context(), expired, now.Add(2*time.Minute)); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("expired claim completed: %v", err)
			}
			current.Status = "completed"
			current.Outcome = "committed"
			if err := owner.RenewGitHubOutbound(t.Context(), expired.ID, expired.LeaseID, now.Add(2*time.Minute), now.Add(4*time.Minute), false); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("old claim renewed: %v", err)
			}
			return owner.FinishGitHubOutbound(t.Context(), current, now.Add(2*time.Minute))
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.(projectstore.GitHubOutboxWriter).ClaimGitHubOutbound(t.Context(), now.Add(3*time.Minute), now.Add(4*time.Minute), "none")
			if !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("completed command was reclaimed: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			owner := w.(projectstore.GitHubOutboxWriter)
			for _, row := range []projectstore.GitHubOutbound{{ProjectID: "one", WorkItemID: "wi_example", Key: "one-new", Repository: "example-org/example-project", IssueNumber: 1, Kind: "workpad", CreatedAt: now.Add(4 * time.Minute), Payload: payload}, {ProjectID: "one", WorkItemID: "wi_other", Key: "other-item", Repository: "example-org/example-project", IssueNumber: 2, Kind: "workpad", CreatedAt: now.Add(4 * time.Minute), Payload: payload}} {
				if err := owner.EnqueueGitHubOutbound(t.Context(), row); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			project tracker.ProjectID
			item    tracker.NativeWorkItemID
			key     string
		}{{"one", "wi_example", "one-new"}, {"one", "wi_other", "other-item"}, {"two", "wi_example", "two"}} {
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.(projectstore.GitHubWorkpadReader).LatestGitHubWorkpad(t.Context(), test.project, test.item)
				if err == nil && (row.Key != test.key || row.ProjectID != test.project || row.WorkItemID != test.item || row.Payload.OrganizationID != org) {
					return fmt.Errorf("workpad scope/latest mismatch: %+v", row)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.(projectstore.GitHubOutboxWriter).SupersedeGitHubCompatibilityWrites(t.Context(), "one", true, now.Add(5*time.Minute))
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			row, err := r.(projectstore.GitHubWorkpadReader).LatestGitHubWorkpad(t.Context(), "one", "wi_example")
			if err == nil && row.Key != "one" {
				return fmt.Errorf("superseded workpad hid retained completed intent: %+v", row)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("latest item workpad seeks through retained noise", func(t *testing.T) {
		if err := repository.Close(); err != nil {
			t.Fatal(err)
		}
		config := pool.Config()
		trace := &workpadQueryTrace{}
		config.ConnConfig.Tracer = trace
		tracedPool, err := pgxpool.NewWithConfig(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(tracedPool.Close)
		reader, err := postgres.NewRepository(t.Context(), tracedPool, pgstoretest.SmallPool())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := reader.Close(); err != nil {
				t.Error(err)
			}
		})
		for _, org := range []tracker.OrganizationID{"first", "second"} {
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO github_outbound(organization_id,project_id,work_item_id,command_key,repository,issue_number,kind,status,next_attempt_at,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,outcome)
SELECT $1,'one',CASE WHEN n<=1000 THEN 'wi_example' ELSE 'wi_noise_'||n END,'workpad-noise-'||n,'example-org/example-project',1,'workpad',CASE WHEN n<=1000 THEN 'superseded' ELSE 'pending' END,$2,$2,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,'unattempted'
FROM generate_series(1,2000)n CROSS JOIN LATERAL(SELECT payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM github_outbound WHERE organization_id=$1 AND command_key='one')source`, org, now); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := admin.ExecContext(t.Context(), "ANALYZE github_outbound"); err != nil {
			t.Fatal(err)
		}
		for _, org := range []tracker.OrganizationID{"first", "second"} {
			if err := reader.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.(projectstore.GitHubWorkpadReader).LatestGitHubWorkpad(t.Context(), "one", "wi_example")
				if err == nil && row.Key != "one" {
					return fmt.Errorf("retained noise changed selected workpad: %+v", row)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		trace.mu.Lock()
		samples := append([]queueListQuerySample(nil), trace.samples...)
		trace.mu.Unlock()
		if len(samples) != 2 {
			t.Fatalf("actual workpad requests=%d", len(samples))
		}
		for _, sample := range samples {
			if err := withPlanSeedTransaction(t.Context(), admin, func(tx pgx.Tx) error {
				if _, err := tx.Exec(t.Context(), "SET LOCAL ROLE detent_runtime_v2"); err != nil {
					return err
				}
				if _, err := tx.Exec(t.Context(), "SELECT set_config('detent.organization_id',$1,true)", sample.args[0]); err != nil {
					return err
				}
				var raw []byte
				if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sample.sql, sample.args...).Scan(&raw); err != nil {
					return err
				}
				var plans []struct{ Plan queueListPlan }
				if err := json.Unmarshal(raw, &plans); err != nil {
					return err
				}
				if len(plans) != 1 {
					return fmt.Errorf("workpad plan count=%d", len(plans))
				}
				visits := float64(0)
				keyed := false
				var inspect func(queueListPlan, string)
				inspect = func(node queueListPlan, relation string) {
					if node.Relation != "" {
						relation = node.Relation
						if relation == "github_outbound" {
							visits += (node.Rows + node.Removed + node.Rechecked) * node.Loops
						}
					}
					if relation == "github_outbound" && strings.Contains(node.Condition, "organization_id") && strings.Contains(node.Condition, "project_id") && strings.Contains(node.Condition, "work_item_id") {
						keyed = true
					}
					for _, child := range node.Plans {
						inspect(child, relation)
					}
				}
				inspect(plans[0].Plan, "")
				if !keyed || visits > 1 {
					return fmt.Errorf("workpad selected-item seek visits=%f keyed=%t plan=%s", visits, keyed, raw)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	})

}

type workpadQueryTrace struct {
	mu      sync.Mutex
	samples []queueListQuerySample
}

func (trace *workpadQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "o.kind='workpad'") && strings.Contains(data.SQL, "ORDER BY o.id DESC LIMIT 1") {
		trace.mu.Lock()
		trace.samples = append(trace.samples, queueListQuerySample{sql: data.SQL, args: append([]any(nil), data.Args...)})
		trace.mu.Unlock()
	}
	return ctx
}
func (*workpadQueryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
