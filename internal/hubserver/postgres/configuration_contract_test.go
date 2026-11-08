package postgres_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestConfigurationContractFencesProjectionPolicyAndCurrentGrants(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	now := historyAt()
	objects := newHistoryObjects()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
		for _, id := range []string{"first", "second"} {
			seedRunnerIdentity(t, repository, org, id, "machine_"+id, now, true)
		}
		for _, id := range []string{"first", "second"} {
			project := tracker.ProjectID("prj_" + id)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.ApproveProjectPolicy(t.Context(), policyTestRevision(org, project, "policy_"+id, now), "")
			}); err != nil {
				t.Fatal(err)
			}
		}
		defaults, err := objects.StoreDetail(t.Context(), string(org), []byte("example defaults"))
		if err != nil {
			t.Fatal(err)
		}
		for _, runner := range []string{"first", "second"} {
			source := artifact.Digest([]byte(string(org) + runner + "initial"))
			authority := postgres.ConfigurationAuthority{RunnerID: runner, EnrollmentID: "enrollment_" + runner, Revision: 1, At: now}
			if err := repository.WriteConfiguration(t.Context(), org, corestore.Serving, func(w postgres.ConfigurationWriter) error {
				header := postgres.ConfigurationHeader{OrganizationID: org, RunnerID: runner, SourceRevision: source, DefaultsObjectKey: new(defaults.Key), DefaultsSha256: defaults.SHA256, DefaultsBytes: defaults.CompressedSize, DefaultsContentBytes: defaults.UncompressedSize, CreatedAt: now}
				created, err := w.InitializeConfiguration(t.Context(), header, authority)
				if err != nil {
					return err
				}
				if !created {
					t.Fatal("configuration not initialized")
				}
				created, err = w.InitializeConfiguration(t.Context(), header, authority)
				if err != nil {
					return err
				}
				if created {
					t.Fatal("configuration initialized twice")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			change := postgres.ConfigurationChange{OrganizationID: org, Authority: authority, ExpectedRevision: 1, ExpectedSourceRevision: source, SourceRevision: artifact.Digest([]byte(string(org) + runner + "applied"))}
			for _, id := range []string{"first", "second"} {
				payload, err := objects.StoreDetail(t.Context(), string(org), []byte("example configuration "+id))
				if err != nil {
					t.Fatal(err)
				}
				change.Projects = append(change.Projects, postgres.ConfigurationProject{OrganizationID: org, RunnerID: runner, ProjectID: tracker.ProjectID("prj_" + id), ConfigRevision: payload.SHA256, PolicyID: "policy_" + id, State: "attached", LocalIntakeEnabled: true, PayloadObjectKey: payload.Key, PayloadSHA256: payload.SHA256, PayloadBytes: payload.CompressedSize, PayloadContentBytes: payload.UncompressedSize})
			}
			for _, test := range []struct {
				name   string
				mutate func(*postgres.ConfigurationChange)
				want   error
			}{
				{"foreign tenant", func(c *postgres.ConfigurationChange) { c.OrganizationID = "foreign" }, corestore.ErrTenantMismatch},
				{"stale enrollment", func(c *postgres.ConfigurationChange) { c.Authority.EnrollmentID = "replaced" }, corestore.ErrRevisionConflict},
				{"stale runner", func(c *postgres.ConfigurationChange) { c.Authority.Revision = 9 }, corestore.ErrRevisionConflict},
				{"stale source", func(c *postgres.ConfigurationChange) { c.ExpectedSourceRevision = artifact.Digest([]byte("stale")) }, corestore.ErrRevisionConflict},
				{"stale policy", func(c *postgres.ConfigurationChange) { c.Projects[1].PolicyID = "stale" }, corestore.ErrRevisionConflict},
				{"omitted policy", func(c *postgres.ConfigurationChange) { c.Projects[1].PolicyID = "" }, corestore.ErrRevisionConflict},
			} {
				t.Run(fmt.Sprint(org, runner, test.name), func(t *testing.T) {
					input := change
					input.Projects = append([]postgres.ConfigurationProject(nil), change.Projects...)
					test.mutate(&input)
					err := repository.WriteConfiguration(t.Context(), org, corestore.Serving, func(w postgres.ConfigurationWriter) error {
						_, err := w.ApplyConfiguration(t.Context(), input)
						return err
					})
					if !errors.Is(err, test.want) {
						t.Fatalf("apply error=%v want=%v", err, test.want)
					}
				})
			}
			if err := repository.WriteConfiguration(t.Context(), org, corestore.Serving, func(w postgres.ConfigurationWriter) error {
				applied, err := w.ApplyConfiguration(t.Context(), change)
				if err != nil {
					return err
				}
				if applied.Revision != 2 || applied.SourceRevision != change.SourceRevision {
					t.Fatalf("applied=%+v", applied)
				}
				for _, project := range change.Projects {
					if _, err := w.ObserveConfiguration(t.Context(), postgres.ConfigurationObservation{OrganizationID: org, RunnerID: runner, ProjectID: project.ProjectID, ConfigRevision: project.ConfigRevision, PolicyID: project.PolicyID, Registered: true, ObservedAt: new(now.Add(time.Minute))}, authority); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := repository.WriteConfiguration(t.Context(), org, corestore.Serving, func(w postgres.ConfigurationWriter) error {
			return w.SetOrganizationProjectRanks(t.Context(), 1, []tracker.ProjectID{"prj_second", "prj_first"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, _ = repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.ReadConfiguration(t.Context(), org, corestore.Serving, func(r postgres.ConfigurationReader) error {
			ranks, err := r.OrganizationRankProjects(t.Context(), 3)
			if err != nil {
				return err
			}
			if len(ranks) != 2 || ranks[0].ID != "prj_second" || ranks[1].ID != "prj_first" {
				t.Fatalf("rank=%+v", ranks)
			}
			revision, err := r.OrganizationSchedulingRevision(t.Context())
			if err != nil {
				return err
			}
			if revision != 2 {
				t.Fatalf("rank revision=%d", revision)
			}
			for _, runner := range []string{"first", "second"} {
				header, err := r.ConfigurationHeader(t.Context(), runner)
				if err != nil {
					return err
				}
				if header.OrganizationID != org || header.Revision != 2 || header.DefaultsObjectKey == nil {
					t.Fatalf("header=%+v", header)
				}
				rows, err := r.ConfigurationProjects(t.Context(), runner, now, "", 2)
				if err != nil {
					return err
				}
				if len(rows) != 2 {
					t.Fatalf("projects=%+v", rows)
				}
				for _, row := range rows {
					if row.OrganizationID != org || row.RunnerID != runner || !row.ObservedRegistered || row.ObservedConfigRevision != row.ConfigRevision || row.ObservedPolicyID != row.PolicyID {
						t.Fatalf("project=%+v", row)
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `UPDATE runner_identities SET scope='projects' WHERE organization_id=$1 AND runner_id='first'`, org); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: "first", ProjectID: "prj_first"})
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadConfiguration(t.Context(), org, corestore.Serving, func(r postgres.ConfigurationReader) error {
			rows, err := r.ConfigurationProjects(t.Context(), "first", now, "", 2)
			if err == nil && (len(rows) != 1 || rows[0].ProjectID != "prj_first") {
				t.Fatalf("current grants escaped=%+v", rows)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		deniedPayload, err := objects.StoreDetail(t.Context(), string(org), []byte("denied configuration"))
		if err != nil {
			t.Fatal(err)
		}
		deniedChange := postgres.ConfigurationChange{OrganizationID: org, Authority: postgres.ConfigurationAuthority{RunnerID: "first", EnrollmentID: "enrollment_first", Revision: 1, At: now}, ExpectedRevision: 2, ExpectedSourceRevision: artifact.Digest([]byte(string(org) + "firstapplied")), SourceRevision: artifact.Digest([]byte("denied")), Projects: []postgres.ConfigurationProject{{OrganizationID: org, RunnerID: "first", ProjectID: "prj_second", ConfigRevision: deniedPayload.SHA256, PolicyID: "policy_second", State: "attached", PayloadObjectKey: deniedPayload.Key, PayloadSHA256: deniedPayload.SHA256, PayloadBytes: deniedPayload.CompressedSize, PayloadContentBytes: deniedPayload.UncompressedSize}}}
		if err := repository.WriteConfiguration(t.Context(), org, corestore.Serving, func(w postgres.ConfigurationWriter) error {
			_, err := w.ApplyConfiguration(t.Context(), deniedChange)
			return err
		}); !errors.Is(err, accessstore.ErrDenied) {
			t.Fatalf("grant-revoked write=%v", err)
		}

	}
}
