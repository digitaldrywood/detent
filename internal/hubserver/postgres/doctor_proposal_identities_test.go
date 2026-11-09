package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type doctorProposalQueryTrace struct {
	query string
	args  []any
}

func (trace *doctorProposalQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	query := strings.ToLower(data.SQL)
	if strings.Contains(query, "doctor_proposal_identities") && strings.Contains(query, "select") {
		trace.query, trace.args = data.SQL, append([]any(nil), data.Args...)
	}
	return ctx
}

func (*doctorProposalQueryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func assertDoctorProposalIdentitySeek(t *testing.T, admin *sql.DB, org tracker.OrganizationID, trace *doctorProposalQueryTrace) {
	t.Helper()
	if trace.query == "" {
		t.Fatal("doctor identity query was not captured")
	}
	tx, err := admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Error(err)
		}
	}()
	if _, err := tx.ExecContext(t.Context(), `SET LOCAL ROLE detent_runtime_v2`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), `SELECT set_config('detent.organization_id',$1,true)`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), `SET LOCAL enable_seqscan=off`); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := tx.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE,FORMAT JSON) "+trace.query, trace.args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	type plan struct {
		NodeType string  `json:"Node Type"`
		Index    string  `json:"Index Name"`
		Cond     string  `json:"Index Cond"`
		Rows     float64 `json:"Actual Rows"`
		Plans    []plan
	}
	var plans []struct{ Plan plan }
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatal(err)
	}
	found := false
	var visit func(plan, bool)
	visit = func(node plan, bounded bool) {
		if node.NodeType == "Limit" {
			bounded = true
		}
		if node.NodeType == "Sort" {
			bounded = false
		}
		if node.Index != "" && strings.Contains(node.Cond, "identity_hash") {
			found = true
			if !bounded || node.Rows > 1 || !strings.Contains(node.Cond, "has_state") || !strings.Contains(node.Cond, "state_name") || node.NodeType != "Index Scan" && node.NodeType != "Index Only Scan" {
				t.Errorf("identity lookup did not seek one ordered row per state: type=%s bounded=%v rows=%v condition=%s", node.NodeType, bounded, node.Rows, node.Cond)
			}
		}
		for _, child := range node.Plans {
			visit(child, bounded)
		}
	}
	for _, node := range plans {
		visit(node.Plan, false)
	}
	if !found {
		t.Fatal("doctor identity lookup did not use an identity seek")
	}
}

func TestDoctorProposalIdentitiesPreserveFullBodyAndFirstOrigin(t *testing.T) {
	first := "<!-- detent:self-improvement proposal_id=example-first -->"
	second := "<!-- detent:self-improvement proposal_id=example-second -->"
	for _, test := range []struct {
		name, body string
		want       []postgres.DoctorProposalIdentity
	}{
		{"empty", "Example body", nil},
		{"full body and duplicates", first + strings.Repeat("x", 70000) + "\n```\n" + second + "\n```\n" + first + "\n```detent-origin\norigin_kind: doctor\nfingerprint: first-origin\n```\n```detent-origin\norigin_kind: doctor\nfingerprint: second-origin\n```", []postgres.DoctorProposalIdentity{{Kind: "fingerprint", Value: "first-origin"}, {Kind: "marker", Value: first}, {Kind: "marker", Value: second}}},
		{"binary fingerprint", "```detent-origin\norigin_kind: doctor\nfingerprint: \"example\\0fingerprint\"\n```", []postgres.DoctorProposalIdentity{{Kind: "fingerprint", Value: "example\x00fingerprint"}}},
		{"unsupported marker spacing", "<!-- detent:self-improvement proposal_id =example -->", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := postgres.DeriveDoctorProposalIdentities(test.body); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("identities = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestDoctorProposalIdentityCoverageFencesCurrentSelection(t *testing.T) {
	admin := template.Database(t)
	config := smallPool()
	config.ServingConnections = 2
	repository, pool := repositoryFor(t, admin, config)
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	trace := &doctorProposalQueryTrace{}
	repository, err := postgres.NewRepository(t.Context(), tracedFeaturePool(t, pool, trace), config)
	if err != nil {
		t.Fatal(err)
	}
	initial := repository
	t.Cleanup(func() {
		if err := initial.Close(); err != nil {
			t.Error(err)
		}
	})
	content := eventstore.NewContent(newHistoryObjects())
	marker := "<!-- detent:self-improvement proposal_id=example-first -->"
	otherMarker := "<!-- detent:self-improvement proposal_id=" + strings.Repeat("y", 7000) + " -->"
	fingerprint := "example\x00fingerprint"
	body := strings.Repeat("x", 70000) + "\n" + marker + "\n" + otherMarker + "\n```detent-origin\norigin_kind: doctor\nfingerprint: \"example\\0fingerprint\"\n```"
	lookup := func(t *testing.T, org tracker.OrganizationID, selection postgres.DoctorProposalIdentitySelection) (postgres.DoctorProposalIdentityResult, error) {
		t.Helper()
		var result postgres.DoctorProposalIdentityResult
		err := repository.ReadDoctorProposalIdentities(t.Context(), org, corestore.Serving, func(r postgres.DoctorProposalIdentityReader) error {
			var err error
			result, err = r.LookupDoctorProposalIdentity(t.Context(), selection)
			return err
		})
		return result, err
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error { return seedCoreOrganization(t.Context(), w, org) }); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			for _, ignored := range []struct{ project, profile string }{{"prj_0_linked", "github_compatible"}, {"prj_0_retired", "native"}} {
				project := tracker.ProjectID(ignored.project)
				if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: project, Name: string(project), Profile: ignored.profile}); err != nil {
					return err
				}
				for number := int64(1); number <= 2; number++ {
					if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: tracker.NativeWorkItemID(fmt.Sprintf("ignored_%s_%d", project, number)), Number: number, Title: "Example historical item"}); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `UPDATE projects SET deleted_at=now() WHERE organization_id=$1 AND id='prj_0_retired'`, org); err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{"first", "second"} {
			t.Run(string(org)+"/"+suffix, func(t *testing.T) {
				project := tracker.ProjectID("prj_" + suffix)
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: string(project), Profile: "native"}, States: []tracker.NativeState{{Name: "Open"}, {Name: "Done", Terminal: true}}})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				var bindings []postgres.DoctorProposalIdentityBinding
				for number := int64(1); number <= 2; number++ {
					item := tracker.NativeWorkItemID(fmt.Sprintf("proposal_%s_%d", suffix, number))
					state := ""
					var workflow *string
					if number == 2 {
						state = "Done"
						workflow = new(state)
					}
					snapshot := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: item, Number: int(number), Revision: 1}, Title: "Example proposal", Body: body, State: state, Terminal: number == 2}
					version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: item, RecordID: string(item), Revision: 1}, historyJSON(t, snapshot))
					if err != nil {
						t.Fatal(err)
					}
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: item, Number: number, Title: snapshot.Title, WorkflowStateName: workflow}); err != nil {
							return err
						}
						_, err := w.CreateCollaborationVersion(t.Context(), version)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					bindings = append(bindings, postgres.DoctorProposalIdentityBinding{OrganizationID: org, ProjectID: project, WorkItemID: item, Revision: 1, VersionSHA256: version.PayloadSHA256, Identities: postgres.DeriveDoctorProposalIdentities(body)})
				}
				selection := postgres.DoctorProposalIdentitySelection{ProjectID: project, Identity: postgres.DoctorProposalIdentity{Kind: "marker", Value: marker}}
				for i, binding := range bindings {
					if _, err := lookup(t, org, selection); !errors.Is(err, postgres.ErrDoctorProposalIdentitiesPending) {
						t.Fatalf("partial identity coverage treated as absence: %v", err)
					}
					if err := repository.ReadDoctorProposalIdentities(t.Context(), org, corestore.Serving, func(r postgres.DoctorProposalIdentityReader) error {
						head, err := r.NextDoctorProposalIdentityHead(t.Context())
						if err == nil && (head.Item.NativeID != binding.WorkItemID || head.Version.PayloadSHA256 != binding.VersionSHA256) {
							return errors.New("unknown head seek lost current immutable version")
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
					for _, invalid := range []postgres.DoctorProposalIdentityBinding{
						{OrganizationID: org, ProjectID: project, WorkItemID: binding.WorkItemID, Revision: 2, VersionSHA256: binding.VersionSHA256},
						{OrganizationID: org, ProjectID: project, WorkItemID: binding.WorkItemID, Revision: 1, VersionSHA256: artifact.Digest([]byte("wrong head"))},
					} {
						if err := repository.WriteDoctorProposalIdentities(t.Context(), org, corestore.Serving, func(w postgres.DoctorProposalIdentityWriter) error {
							return w.BindDoctorProposalIdentities(t.Context(), invalid)
						}); !errors.Is(err, corestore.ErrRevisionConflict) {
							t.Fatalf("wrong immutable head accepted: %v", err)
						}
					}
					for range 2 {
						if err := repository.WriteDoctorProposalIdentities(t.Context(), org, corestore.Serving, func(w postgres.DoctorProposalIdentityWriter) error {
							return w.BindDoctorProposalIdentities(t.Context(), binding)
						}); err != nil {
							t.Fatal(err)
						}
					}
					conflict := binding
					conflict.Identities = nil
					if err := repository.WriteDoctorProposalIdentities(t.Context(), org, corestore.Serving, func(w postgres.DoctorProposalIdentityWriter) error {
						return w.BindDoctorProposalIdentities(t.Context(), conflict)
					}); !errors.Is(err, mutation.ErrConflict) {
						t.Fatalf("changed identities accepted for exact head %d: %v", i, err)
					}
				}
				for _, identity := range []postgres.DoctorProposalIdentity{{Kind: "marker", Value: marker}, {Kind: "marker", Value: otherMarker}, {Kind: "fingerprint", Value: fingerprint}} {
					selection.Identity = identity
					result, err := lookup(t, org, selection)
					if err != nil || result.WorkItemID != bindings[0].WorkItemID || result.Terminal || result.VersionSHA256 != bindings[0].VersionSHA256 {
						t.Fatalf("open full-body identity match = %+v, error %v", result, err)
					}
				}
				assertDoctorProposalIdentitySeek(t, admin, org, trace)
				result, err := lookup(t, org, selection)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), `UPDATE issues SET workflow_state_name='Open' WHERE organization_id=$1 AND project_id=$2 AND native_id=$3`, org, project, bindings[0].WorkItemID); err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteDoctorProposalIdentities(t.Context(), org, corestore.Serving, func(w postgres.DoctorProposalIdentityWriter) error {
					return w.ValidateDoctorProposalIdentitySelection(t.Context(), result)
				}); !errors.Is(err, corestore.ErrSerializationFailure) {
					t.Fatalf("changed item state did not fence selection: %v", err)
				}
				result, err = lookup(t, org, selection)
				if err != nil || result.WorkItemID != bindings[0].WorkItemID || result.Terminal {
					t.Fatalf("state membership refresh = %+v, error %v", result, err)
				}
				if _, err := admin.ExecContext(t.Context(), `UPDATE project_workflow_states SET terminal=true WHERE organization_id=$1 AND project_id=$2 AND name='Open'`, org, project); err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteDoctorProposalIdentities(t.Context(), org, corestore.Serving, func(w postgres.DoctorProposalIdentityWriter) error {
					return w.ValidateDoctorProposalIdentitySelection(t.Context(), result)
				}); !errors.Is(err, corestore.ErrSerializationFailure) {
					t.Fatalf("changed current workflow did not fence selection: %v", err)
				}
				result, err = lookup(t, org, selection)
				if err != nil || result.WorkItemID != bindings[1].WorkItemID || !result.Terminal {
					t.Fatalf("terminal match = %+v, error %v", result, err)
				}
				if _, err := admin.ExecContext(t.Context(), `UPDATE issues SET number=3 WHERE organization_id=$1 AND project_id=$2 AND native_id=$3`, org, project, bindings[0].WorkItemID); err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteDoctorProposalIdentities(t.Context(), org, corestore.Serving, func(w postgres.DoctorProposalIdentityWriter) error {
					return w.ValidateDoctorProposalIdentitySelection(t.Context(), result)
				}); !errors.Is(err, corestore.ErrSerializationFailure) {
					t.Fatalf("changed item number did not fence selection: %v", err)
				}
				result, err = lookup(t, org, selection)
				if err != nil || result.WorkItemID != bindings[0].WorkItemID || result.Number != 3 || !result.Terminal {
					t.Fatalf("terminal number refresh = %+v, error %v", result, err)
				}
				if _, err := admin.ExecContext(t.Context(), `UPDATE issues SET number=1 WHERE organization_id=$1 AND project_id=$2 AND native_id=$3`, org, project, bindings[0].WorkItemID); err != nil {
					t.Fatal(err)
				}
				selection.OpenOnly = true
				result, err = lookup(t, org, selection)
				if err != nil || result.WorkItemID != "" {
					t.Fatalf("open-only terminal match = %+v, error %v", result, err)
				}
				selection.OpenOnly = false
				for _, binding := range bindings {
					if _, err := admin.ExecContext(t.Context(), `UPDATE issues SET archived=true WHERE organization_id=$1 AND project_id=$2 AND native_id=$3`, org, project, binding.WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
				result, err = lookup(t, org, selection)
				if err != nil || result.WorkItemID != "" {
					t.Fatalf("archived match = %+v, error %v", result, err)
				}
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO issues(organization_id,project_id,native_id,number,title) VALUES($1,$2,$3,3,'Example')`, org, project, "unbound_"+suffix); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if _, err := admin.ExecContext(t.Context(), `DELETE FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3`, org, project, "unbound_"+suffix); err != nil {
						t.Error(err)
					}
				}()
				if err := repository.WriteDoctorProposalIdentities(t.Context(), org, corestore.Serving, func(w postgres.DoctorProposalIdentityWriter) error {
					return w.ValidateDoctorProposalIdentitySelection(t.Context(), result)
				}); !errors.Is(err, corestore.ErrSerializationFailure) {
					t.Fatalf("new source did not fence negative selection: %v", err)
				}
				if _, err := lookup(t, org, selection); !errors.Is(err, postgres.ErrDoctorProposalIdentitiesPending) {
					t.Fatalf("new unknown head treated as absence: %v", err)
				}
				if err := repository.ReadDoctorProposalIdentities(t.Context(), org, corestore.Serving, func(r postgres.DoctorProposalIdentityReader) error {
					_, err := r.NextDoctorProposalIdentityHead(t.Context())
					return err
				}); !errors.Is(err, eventstore.ErrInvalidRecord) {
					t.Fatalf("missing current version treated as no pending head: %v", err)
				}
				if _, err := admin.ExecContext(t.Context(), `DELETE FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3`, org, project, "unbound_"+suffix); err != nil {
					t.Fatal(err)
				}
				current := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: bindings[0].WorkItemID, Number: 1, Revision: 2}, Title: "Example proposal", Body: "Example resolved issue", State: "Open", Terminal: true}
				version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: current.WorkItemID, RecordID: string(current.WorkItemID), Revision: 2}, historyJSON(t, current))
				if err != nil {
					t.Fatal(err)
				}
				currentIdentities := postgres.DeriveDoctorProposalIdentities(current.Body)
				if err := repository.WriteDoctorProposalIdentities(t.Context(), org, corestore.Serving, func(w postgres.DoctorProposalIdentityWriter) error {
					row, err := w.WorkItem(t.Context(), project, current.WorkItemID)
					if err != nil {
						return err
					}
					row.Archived = false
					if _, err := w.UpdateWorkItem(t.Context(), row); err != nil {
						return err
					}
					if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
						return err
					}
					return w.BindDoctorProposalIdentities(t.Context(), postgres.DoctorProposalIdentityBinding{OrganizationID: org, ProjectID: project, WorkItemID: row.NativeID, Revision: 2, VersionSHA256: version.PayloadSHA256, Identities: currentIdentities})
				}); err != nil {
					t.Fatal(err)
				}
				result, err = lookup(t, org, selection)
				if err != nil || result.WorkItemID != "" {
					t.Fatalf("removed identities or known-empty head = %+v, error %v", result, err)
				}
				var tasks []*repositoryTask
				start := make(chan struct{})
				ids := []string{"racing_" + suffix + "_first", "racing_" + suffix + "_second"}
				var versions []eventstore.CollaborationVersion
				for i, id := range ids {
					item := tracker.NativeWorkItemID(id)
					snapshot := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: item, Number: 3 + i, Revision: 1}, Title: "Example racing proposal", Body: body, State: "Open", Terminal: true}
					version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: item, RecordID: id, Revision: 1}, historyJSON(t, snapshot))
					if err != nil {
						t.Fatal(err)
					}
					versions = append(versions, version)
				}
				identities := postgres.DeriveDoctorProposalIdentities(body)
				for i, version := range versions {
					tasks = append(tasks, startRepositoryTask(t, func(ctx context.Context) error {
						select {
						case <-start:
						case <-ctx.Done():
							return ctx.Err()
						}
						return repository.WriteDoctorProposalIdentities(ctx, org, corestore.Serving, func(w postgres.DoctorProposalIdentityWriter) error {
							if err := w.ValidateDoctorProposalIdentitySelection(ctx, result); err != nil {
								return err
							}
							if _, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: version.WorkItemID, Number: int64(3 + i), Title: "Example racing proposal", WorkflowStateName: new("Open")}); err != nil {
								return err
							}
							if _, err := w.CreateCollaborationVersion(ctx, version); err != nil {
								return err
							}
							return w.BindDoctorProposalIdentities(ctx, postgres.DoctorProposalIdentityBinding{OrganizationID: org, ProjectID: project, WorkItemID: version.WorkItemID, Revision: 1, VersionSHA256: version.PayloadSHA256, Identities: identities})
						})
					}))
				}
				close(start)
				created, fenced := 0, 0
				for _, task := range tasks {
					err := task.wait(t)
					if err == nil {
						created++
					} else if errors.Is(err, corestore.ErrSerializationFailure) {
						fenced++
					} else {
						t.Fatalf("competing proposal: %v", err)
					}
				}
				var count int
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=ANY($3::text[])`, org, project, ids).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if created != 1 || fenced != 1 || count != 1 {
					t.Fatalf("negative selection race created=%d fenced=%d rows=%d", created, fenced, count)
				}
				winner, err := lookup(t, org, selection)
				if err != nil || !slices.Contains(ids, string(winner.WorkItemID)) {
					t.Fatalf("replanned selection did not find committed proposal: %+v error=%v", winner, err)
				}
				if _, err := admin.ExecContext(t.Context(), `DELETE FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3`, org, project, winner.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteDoctorProposalIdentities(t.Context(), org, corestore.Serving, func(w postgres.DoctorProposalIdentityWriter) error {
					return w.ValidateDoctorProposalIdentitySelection(t.Context(), winner)
				}); !errors.Is(err, corestore.ErrSerializationFailure) {
					t.Fatalf("deleted source did not fence positive selection: %v", err)
				}
			})
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, _ = repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
			result, err := lookup(t, org, postgres.DoctorProposalIdentitySelection{ProjectID: project, Identity: postgres.DoctorProposalIdentity{Kind: "marker", Value: marker}})
			if err != nil || result.WorkItemID != "" {
				t.Fatalf("reopened archived identity = %+v, error %v", result, err)
			}
		}
		if err := repository.ReadDoctorProposalIdentities(t.Context(), org, corestore.Serving, func(r postgres.DoctorProposalIdentityReader) error {
			_, err := r.NextDoctorProposalIdentityHead(t.Context())
			return err
		}); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("inactive project heads entered native maintenance: %v", err)
		}
	}
}
