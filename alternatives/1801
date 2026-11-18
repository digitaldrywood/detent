package coreimporttest

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func WorkItems(t *testing.T, repository fs.FS, org tracker.OrganizationID, project tracker.ProjectID, malformedQueue bool, body string) *neutralimporttest.Fixture {
	t.Helper()
	fixture := neutralimporttest.New(t, repository, org, "issues", "workflow_states", "repositories", "queue_entries", "native_comments", "collaboration_versions", "collaboration_events", "issue_dependencies", "change_content_objects", "attachments")
	const created = "2026-10-01T00:00:00Z"
	const updated = "2026-10-02T00:00:00Z"
	fixture.Add("workflow_states", map[string]any{"id": 1, "project_id": project, "detent_state": "Todo"})
	fixture.Add("repositories", map[string]any{"id": 1, "github_owner": "example-org", "github_name": "example-project"})
	actor := tracker.Actor{Kind: "human", PrincipalID: "example-owner"}
	actorJSON, err := json.Marshal(actor)
	if err != nil {
		t.Fatal(err)
	}
	for index, suffix := range []string{"", "_second"} {
		base := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: tracker.NativeWorkItemID("wi_core_import" + suffix), Number: 1 + index*2, Revision: 1, Profile: "native"}, Title: "Initial title", Body: body, State: "Todo", Labels: []string{"needs-review"}, Assignees: []string{"example-assignee"}, Actor: actor, CreatedAt: parseTime(t, created), UpdatedAt: parseTime(t, created), LastActivityAt: parseTime(t, created)}
		baseRaw, err := json.Marshal(base)
		if err != nil {
			t.Fatal(err)
		}
		baseKeys, baseFields, err := neutralimport.VersionObject(baseRaw)
		if err != nil {
			t.Fatal(err)
		}
		baseRaw, err = neutralimport.VersionObjectBytes(baseKeys, baseFields)
		if err != nil {
			t.Fatal(err)
		}
		current := base
		current.Revision = 2
		current.Title = "Updated title"
		current.UpdatedAt = parseTime(t, updated)
		current.LastActivityAt = parseTime(t, updated)
		currentRaw, err := json.Marshal(current)
		if err != nil {
			t.Fatal(err)
		}
		currentKeys, currentFields, err := neutralimport.VersionObject(currentRaw)
		if err != nil {
			t.Fatal(err)
		}
		currentRaw, err = neutralimport.VersionObjectBytes(currentKeys, currentFields)
		if err != nil {
			t.Fatal(err)
		}
		patch, encoding, err := neutralimport.EncodeVersion(baseRaw, "snapshot", currentRaw, 2)
		if err != nil || body != "" && encoding != "patch" {
			t.Fatalf("encode source history encoding=%q error=%v", encoding, err)
		}
		for _, version := range []struct {
			revision int
			encoding string
			raw      []byte
		}{{1, "snapshot", baseRaw}, {2, encoding, patch}} {
			fixture.Add("collaboration_versions", map[string]any{"organization_id": org, "project_id": project, "work_item_id": "wi_core_import" + suffix, "record_id": "wi_core_import" + suffix, "revision": version.revision, "encoding": version.encoding, "record_json": string(version.raw)})
		}
		commentActor, err := json.Marshal(tracker.Actor{Kind: "human", PrincipalID: "example-commenter"})
		if err != nil {
			t.Fatal(err)
		}
		comment := tracker.NativeComment{ID: "comment_core_import" + suffix, OrganizationID: org, ProjectID: project, WorkItemID: tracker.NativeWorkItemID("wi_core_import" + suffix), Revision: 1, Sequence: 1, Body: "Example comment", Actor: tracker.Actor{Kind: "human", PrincipalID: "example-commenter"}, CreatedAt: parseTime(t, created), UpdatedAt: parseTime(t, created)}
		commentRaw, err := json.Marshal(comment)
		if err != nil {
			t.Fatal(err)
		}
		fixture.Add("native_comments", map[string]any{"id": comment.ID, "revision": 1, "sequence": 1, "body": "Example comment", "actor_json": string(commentActor), "edited_by_json": nil, "provenance_json": nil, "source_key": nil, "deleted": 0, "created_at": created, "updated_at": created, "organization_id": org, "project_id": project, "work_item_id": "wi_core_import" + suffix})
		fixture.Add("collaboration_versions", map[string]any{"organization_id": org, "project_id": project, "work_item_id": "wi_core_import" + suffix, "record_id": comment.ID, "revision": 1, "encoding": "snapshot", "record_json": string(commentRaw)})
		queueState := "Todo"
		if malformedQueue {
			queueState = ""
		}
		fixture.Add("queue_entries", map[string]any{"id": 41 + index*2, "issue_id": 41 + index*2, "scope": "native", "state": queueState, "rank": "010", "priority_override": 1, "created_at": created, "updated_at": updated})
		for sequence, name := range []string{"issue.created", "issue.edited"} {
			data, err := json.Marshal(tracker.CollaborationData{Revision: tracker.Revision(sequence + 1)})
			if err != nil {
				t.Fatal(err)
			}
			fixture.Add("collaboration_events", map[string]any{"id": "event_core_import_" + string(rune('1'+sequence)) + suffix, "sequence": sequence + 1, "type": name, "schema_version": 1, "actor_json": string(actorJSON), "data_json": string(data), "event_revision": sequence + 1, "recorded_at": []string{created, updated}[sequence], "organization_id": org, "project_id": project, "work_item_id": "wi_core_import" + suffix})
		}
		fixture.Add("issues", map[string]any{"id": 41 + index*2, "native_id": "wi_core_import" + suffix, "project_id": project, "number": 1 + index*2, "revision": 2, "event_sequence": 2, "native_source_key": nil, "title": "Updated title", "body": body, "issue_contract_json": nil, "report_context_json": nil, "actor_json": string(actorJSON), "provenance_json": nil, "archived": 0, "created_at": created, "updated_at": updated, "native_created_at": created, "native_updated_at": updated, "last_activity_at": updated, "workflow_state_id": 1, "labels_json": "[\"needs-review\"]", "assignees_json": "[\"example-assignee\"]", "author_login": "example-author", "repository_id": 1, "dispatch_wait_since": nil, "organization_id": org})
	}
	return fixture
}

func WorkItemBody() string {
	return "Example issue body " + strings.Repeat("x", 8192)
}

func SeedHistoricalTarget(t *testing.T, target *postgres.Repository, organization tracker.OrganizationID, project tracker.ProjectID, profile string) {
	t.Helper()
	if err := target.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: string(organization), Slug: string(organization), CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "provider_" + string(organization)}); err != nil {
			return err
		}
		record, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: string(project), Profile: profile}, Settings: projectstore.Settings{OrganizationID: organization, ProjectID: project}, States: hubtest.States()})
		if err != nil {
			return err
		}
		record.Settings.RepositoryEnabled, record.Settings.Repository = true, "example-org/example-project"
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func Execution(t *testing.T, repository fs.FS, organization tracker.OrganizationID) *neutralimporttest.Fixture {
	t.Helper()
	fixture := neutralimporttest.New(t, repository, organization, "issues", "machines", "leases", "native_attempts", "native_attempt_events", "change_content_objects", "attachments", "api_tokens", "runner_enrollments", "runner_enrollment_projects", "runner_identities", "runner_project_capacity_limits", "runner_project_ranks", "runner_identity_events", "lease_runners", "lease_policies", "work_events", "attempt_usage", "attempt_diffs", "attempt_diff_files", "attempt_diff_file_archives")
	const acquired = "2026-10-01T00:00:00Z"
	const updated = "2026-10-02T00:00:00Z"
	const expired = "2026-10-01T01:00:00Z"
	for index, suffix := range []string{"", "_second"} {
		id := func(value string) string { return value + suffix }
		fixture.Add("issues", map[string]any{"id": 41 + index, "organization_id": organization, "project_id": "prj_execution_import", "native_id": id("wi_execution_import")})
		fixture.Add("machines", map[string]any{"id": id("machine_execution_import"), "organization_id": organization, "hostname": "example-host", "display_name": "Example host", "capacity": 2, "version": "v0.173.4", "sprite_name": "", "last_heartbeat_at": acquired, "registered_at": acquired, "updated_at": updated})
		var expiry any = "2027-10-01T00:00:00Z"
		if index == 1 {
			expiry = nil
		}
		fixture.Add("api_tokens", map[string]any{"id": id("runner_token_execution_import"), "expires_at": expiry})
		fixture.Add("runner_enrollments", map[string]any{"id": id("enrollment_execution_import"), "organization_id": organization, "runner_id": id("runner_execution_import"), "machine_id": id("machine_execution_import"), "token_hash": Hash(byte('3' + index)), "scope": "projects", "shared_machine": 0, "operations_json": "[\"read\",\"claim\"]", "created_at": acquired, "expires_at": "2027-10-01T00:00:00Z", "created_by": "owner_token_execution_import", "redeemed_at": acquired, "revoked_at": nil})
		fixture.Add("runner_enrollment_projects", map[string]any{"enrollment_id": id("enrollment_execution_import"), "project_id": "prj_execution_import"})
		fixture.Add("runner_identities", map[string]any{"id": id("runner_execution_import"), "organization_id": organization, "machine_id": id("machine_execution_import"), "token_id": id("runner_token_execution_import"), "enrollment_id": id("enrollment_execution_import"), "scope": "projects", "state": "active", "revision": 1, "tags_json": "[]", "operations_json": "[\"read\",\"claim\"]", "display_name": "Example runner", "capacity_limit": 2, "reported_capacity": 2, "os": "darwin", "architecture": "arm64", "created_at": acquired, "last_heartbeat_at": updated, "removed_at": nil, "routing_settings_json": "{}", "capacity_configuration_json": "null"})
		fixture.Add("runner_identity_events", map[string]any{"id": 9 + index, "runner_id": id("runner_execution_import"), "actor_id": "owner_token_execution_import", "kind": "enrolled", "occurred_at": acquired})
		fixture.Add("leases", map[string]any{"fencing_token": 40 + index, "lease_id": id("lease_execution_import"), "issue_id": 41 + index, "machine_id": id("machine_execution_import"), "session_id": id("session_execution_import"), "work_item_revision": 0, "landing_capacity": 0, "acquired_at": acquired, "renewed_at": acquired, "expires_at": expired, "released_at": updated})
		fixture.Add("lease_runners", map[string]any{"lease_id": id("lease_execution_import"), "runner_id": id("runner_execution_import")})
		fixture.Add("lease_policies", map[string]any{"lease_id": id("lease_execution_import"), "scope": string(organization) + "/prj_execution_import", "policy_id": "policy_example"})
		run := tracker.NativeRunData{AttemptID: id("attempt_execution_import"), LeaseID: tracker.LeaseID(id("lease_execution_import")), FencingToken: tracker.FencingToken(40 + index), RunID: id("run_execution_import"), Sequence: 1, PolicyID: "policy_example", Runtime: &tracker.NativeRuntimeObservation{LocalAttemptID: 42}}
		runData, err := json.Marshal(run)
		if err != nil {
			t.Fatal(err)
		}
		fixture.Add("native_attempts", map[string]any{"id": id("attempt_execution_import"), "organization_id": organization, "project_id": "prj_execution_import", "work_item_id": id("wi_execution_import"), "lease_id": id("lease_execution_import"), "fencing_token": 40 + index, "run_id": id("run_execution_import"), "sequence": 1, "status": "failed", "work_item_revision": 0, "dispatch_generation": 0, "data_json": string(runData), "checkpoint_json": nil, "artifact_ids_json": "[]", "started_at": updated, "updated_at": updated, "payload_object_key": "", "checkpoint_object_key": ""})
		requestHash := hex.EncodeToString(make([]byte, 32))
		fixture.Add("native_attempt_events", map[string]any{"attempt_id": id("attempt_execution_import"), "sequence": 1, "request_hash": requestHash})
		fixture.Add("attempt_usage", map[string]any{"attempt_id": id("attempt_execution_import"), "project_id": "prj_execution_import", "period": acquired, "provider": "codex", "model": "example-model", "input": 100, "cached_input": 20, "output": 30, "cost_estimate": 0.25, "currency": "USD", "updated_at": updated})

		fixture.Add("work_events", map[string]any{"id": 7 + index, "issue_id": 41 + index, "fencing_token": 40 + index, "machine_id": id("machine_execution_import"), "session_id": id("session_execution_import"), "run_id": id("run_execution_import"), "kind": "progress", "payload_json": "{\"step\":\"compile\"}", "occurred_at": updated, "recorded_at": updated})
	}
	return fixture
}

func Hash(char byte) string {
	return strings.Repeat(string(char), 64)
}

func SeedExecutionTarget(t *testing.T, target *postgres.Repository, admin *sql.DB, organization tracker.OrganizationID) {
	t.Helper()
	project := tracker.ProjectID("prj_execution_import")
	if err := target.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example organization", Slug: string(organization), CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
			return fmt.Errorf("create import test organization: %w", err)
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "provider_execution_import"}); err != nil {
			return fmt.Errorf("create import test tenant: %w", err)
		}
		for _, credential := range []accessstore.Credential{
			{OrganizationID: organization, ID: "owner_token_execution_import", Name: "owner token", Hash: Hash('1'), Fingerprint: "owner", Scope: "admin", ProjectAccess: "selected", CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Kind: "service"},
			{OrganizationID: organization, ID: "runner_token_execution_import", Name: "runner token", Hash: Hash('2'), Fingerprint: "runner", Scope: "worker", ProjectAccess: "selected", NativeOnly: true, CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), ExpiresAt: new(time.Date(2027, 10, 1, 0, 0, 0, 0, time.UTC)), Kind: "service"},
			{OrganizationID: organization, ID: "runner_token_execution_import_second", Name: "second runner token", Hash: Hash('5'), Fingerprint: "runner-second", Scope: "worker", ProjectAccess: "selected", NativeOnly: true, CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), ExpiresAt: new(time.Date(2027, 10, 1, 0, 0, 0, 0, time.UTC)), Kind: "service"},
		} {
			if err := w.CreateAccessCredential(t.Context(), credential); err != nil {
				return fmt.Errorf("create import test credential %s: %w", credential.ID, err)
			}
		}
		if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{
			Core:     corestore.Project{OrganizationID: organization, ID: project, Name: "Example project", Slug: "example-project", Profile: "native"},
			Settings: projectstore.Settings{OrganizationID: organization, ProjectID: project},
			States:   hubtest.States(),
		}); err != nil {
			return fmt.Errorf("create import test project: %w", err)
		}
		for index, suffix := range []string{"", "_second"} {
			_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{
				OrganizationID: organization, ID: int64(41 + index), NativeID: tracker.NativeWorkItemID("wi_execution_import" + suffix), ProjectID: project, Number: int64(1 + index),
				Revision: 1, EventSequence: 1, Title: "Example issue", ActorKind: new("human"), ActorPrincipalID: new("example-owner"),
				CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
				NativeCreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), NativeUpdatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
				LastActivityAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), WorkflowStateName: new("Todo"),
			})
			if err != nil {
				return fmt.Errorf("create import test work item: %w", err)
			}
		}
		return nil
	}); err != nil {
		var postgresErr *pgconn.PgError
		if errors.As(err, &postgresErr) {
			t.Fatalf("initialize import fixture: constraint %s: %s", postgresErr.ConstraintName, postgresErr.Message)
		}
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `INSERT INTO policy_revisions (organization_id,project_id,policy_id,approved_by,approved_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) VALUES ($1,$2,'policy_example','owner_token_execution_import',$3,$4,$5,1,1)`, organization, project, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), "orgs/"+string(organization)+"/blobs/sha256/aa/"+Hash('a'), Hash('a')); err != nil {
		t.Fatalf("seed import test policy: %v", err)
	}
}

func Changes(t *testing.T, repository fs.FS, organization tracker.OrganizationID, project tracker.ProjectID, createdAt time.Time, descriptor policy.Descriptor, policyJSON []byte) *neutralimporttest.Fixture {
	t.Helper()
	fixture := neutralimporttest.New(t, repository, organization, "change_requests", "change_issue_links", "change_versions", "change_evidence", "change_sources", "change_landing_receipts", "quality_landings", "change_viewed_files", "policy_revisions", "change_command_outputs", "change_finding_attributions", "change_content_objects", "attachments", "native_pr_publications", "release_deliveries")
	fixture.Add("policy_revisions", map[string]any{"scope": string(organization) + "/" + string(project), "policy_id": descriptor.ID, "metadata_json": string(policyJSON)})
	for index, suffix := range []string{"a", "b"} {
		id := "change_import_" + suffix
		item := "wi_change_import_" + suffix
		change := tracker.ChangeRequest{ID: id, OrganizationID: organization, ProjectID: project, WorkItemID: tracker.NativeWorkItemID(item), LinkedIssues: []tracker.NativeWorkItemID{tracker.NativeWorkItemID(item)}, Title: "Change " + id, Body: "Preserved discussion", CurrentVersion: "version_" + suffix, Revision: 2, CreatedAt: createdAt, UpdatedAt: createdAt.Add(time.Minute)}
		changeJSON, err := json.Marshal(change)
		if err != nil {
			t.Fatal(err)
		}
		fixture.Add("change_requests", map[string]any{"id": id, "organization_id": organization, "project_id": project, "work_item_id": item, "record_json": string(changeJSON)})
		fixture.Add("change_issue_links", map[string]any{"change_id": id, "organization_id": organization, "project_id": project, "work_item_id": item})
		validation := &gate.CommandResult{HeadSHA: strings.Repeat("b", 40), ExitCode: 0, DurationNS: int64(time.Second), Command: "go test", TreeSHA: strings.Repeat("c", 40), Output: "validated output"}
		baseSHA, headSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
		bundle := ChangeBundle(baseSHA, headSHA)
		source := &tracker.ChangeSource{Format: "git-bundle", BaseSHA: baseSHA, HeadSHA: headSHA, BundleSHA256: tracker.ChangeSourceDigest(bundle), DiffSHA256: strings.Repeat("e", 64), Bytes: int64(len(bundle))}
		externalID := strconv.Itoa(201 + index)
		version := tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: baseSHA, HeadSHA: headSHA, MergeBaseSHA: baseSHA, Repository: "https://github.com/example-org/example-project", AttemptID: "source_" + suffix, PolicyID: descriptor.ID, Code: tracker.ChangeArtifact{Kind: "code", URI: "bundle://example"}, Validation: validation, External: &tracker.ChangeExternalReference{Provider: "github", ID: externalID, URL: "https://github.com/example-org/example-project/pull/" + externalID}, Source: source}, ID: "version_" + suffix, ChangeID: id, Number: int64(index + 1), Policy: descriptor, Actor: tracker.Actor{Kind: "human", PrincipalID: "example-owner"}, CreatedAt: createdAt}
		versionJSON, err := json.Marshal(version)
		if err != nil {
			t.Fatal(err)
		}
		fixture.Add("change_versions", map[string]any{"id": version.ID, "change_id": id, "number": version.Number, "record_json": string(versionJSON), "policy_revision_id": descriptor.ID, "payload_object_key": "", "repository": version.Repository, "created_at": createdAt.Format(time.RFC3339Nano)})
		fixture.Add("change_sources", map[string]any{"version_id": version.ID, "bundle": bundle, "object_key": "", "object_sha256": "", "object_bytes": 0, "content_bytes": 0})
		fixture.Add("change_viewed_files", map[string]any{"version_id": version.ID, "principal_id": "example-owner", "manifest_sha256": strings.Repeat("d", 64), "file_sha256": strings.Repeat("f", 64), "viewed": 1})
		fixture.Add("native_pr_publications", map[string]any{"version_id": version.ID, "change_id": id, "repository": version.Repository, "base_ref": "develop", "branch": "change/" + version.ID, "head_sha": headSHA, "policy_id": descriptor.ID, "source_attempt_id": version.AttemptID, "recorder_attempt_id": "recorder_" + version.ID, "provider": "github", "external_id": externalID, "external_url": version.Repository + "/pull/" + externalID})
		frozen := tracker.ChangeLanding{VersionID: version.ID, HeadSHA: headSHA, MergeSHA: strings.Repeat("c", 40), BaseRef: "develop", Method: "merge", Actor: version.Actor, LandedAt: createdAt.Add(time.Minute + 123*time.Nanosecond), Quality: &tracker.LandingQualitySnapshot{IssueRevision: 1, IssueBody: "Frozen contract " + suffix, Criteria: []string{"Preserved criterion"}, Missing: []string{}, Evidence: []gate.CriterionEvidence{}}}
		frozenJSON, err := json.Marshal(frozen)
		if err != nil {
			t.Fatal(err)
		}
		fixture.Add("quality_landings", map[string]any{"version_id": version.ID, "record_json": string(frozenJSON), "landed_at": hubapi.FormatHubTime(frozen.LandedAt)})
		landing := tracker.NativeLandingReceipt{VersionID: version.ID, HeadSHA: headSHA, MergeSHA: strings.Repeat("c", 40), BaseRef: "develop", BaseSHA: baseSHA, Method: "merge", Landed: true, ObservedAt: createdAt.Add(time.Minute + 123*time.Nanosecond)}
		landingJSON, err := json.Marshal(landing)
		if err != nil {
			t.Fatal(err)
		}
		fixture.Add("change_landing_receipts", map[string]any{"attempt_id": version.AttemptID, "version_id": version.ID, "record_json": string(landingJSON)})
		fixture.Add("release_deliveries", map[string]any{"work_item_id": item, "organization_id": organization, "project_id": project, "source_issue_id": item, "source_change_id": id, "source_merge_sha": strings.Repeat("c", 40), "repository_id": 47, "repository": version.Repository, "pr_number": 300 + index + 1, "head_ref": "release-pick/" + version.ID, "head_sha": headSHA, "base_ref": "release/v1.0", "base_sha": strings.Repeat("d", 40), "frozen_sha": strings.Repeat("e", 40), "change_id": id, "version_id": version.ID, "principal_id": "example-owner", "actor_kind": "human", "via_key": "example-release-key", "associated_at": createdAt.Add(2*time.Minute + 123*time.Nanosecond).Format(time.RFC3339Nano)})
		discussion := tracker.ChangeDiscussion{ID: "discussion_" + suffix, VersionID: version.ID, Body: "Preserved discussion", Actor: tracker.Actor{Kind: "human", PrincipalID: "example-owner"}, CreatedAt: createdAt.Add(2 * time.Minute), Escape: &tracker.QualityEscape{OccurrenceID: "occurrence_" + suffix, Kind: "scheduled_failure", Cause: "validator_miss", ObservedAt: createdAt.Add(90 * time.Second), HumanOverride: true}}
		discussionJSON, err := json.Marshal(discussion)
		if err != nil {
			t.Fatal(err)
		}
		fixture.Add("change_evidence", map[string]any{"sequence": 1, "change_id": id, "version_id": version.ID, "kind": "discussion", "source_key": nil, "record_json": string(discussionJSON), "payload_object_key": ""})
	}
	return fixture
}

func ChangeBundle(base, head string) []byte {
	return []byte(fmt.Sprintf("# v2 git bundle\n%s refs/heads/main\n-%s base\n\nbundle payload", head, base))
}

func parseTime(t *testing.T, raw string) time.Time {
	t.Helper()
	value, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func ExecutionWithCheckpoints(t *testing.T, repository fs.FS, organization tracker.OrganizationID, objects attachment.DetailStore) string {
	t.Helper()
	fixture := Execution(t, repository, organization)
	for index, suffix := range []string{"", "_second"} {
		id := "attempt_execution_import" + suffix
		checkpoint := &tracker.NativeCheckpoint{Resume: "fresh_checkout", WorktreeState: "clean", ExternalEffect: "none", EffectState: "none"}
		if index == 0 {
			checkpoint.WorktreeState = "unpushed"
		}
		var value any = checkpoint
		if organization == "org_execution_import" && index == 0 {
			value = hubrecords.CheckpointPayload{Checkpoint: checkpoint}
		}
		if organization == "org_execution_import_other" {
			run := tracker.NativeRunData{AttemptID: id, LeaseID: tracker.LeaseID("lease_execution_import" + suffix), FencingToken: tracker.FencingToken(40 + index), RunID: "run_execution_import" + suffix, Sequence: 1, Handoff: checkpoint}
			var data any = run
			if index == 1 {
				data = map[string]tracker.NativeRunData{hubrecords.RunDetailEntry(run): run}
			}
			raw, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := objects.StoreDetail(t.Context(), string(organization), raw)
			if err != nil {
				t.Fatal(err)
			}
			fixture.Add("change_content_objects", map[string]any{"object_key": ref.Key, "object_sha256": ref.SHA256, "object_bytes": ref.CompressedSize, "content_bytes": ref.UncompressedSize, "pending_content": raw})
			detail := hubrecords.NewRunDetailReference(ref, string(organization), hubrecords.RunDetailEntry(run))
			if index == 0 {
				detail.ObjectKey = ref.Key
			} else {
				fixture.Add("attachments", map[string]any{"id": detail.AttachmentID, "project_id": "prj_execution_import", "kind": "archive", "object_key": ref.Key, "sha256": ref.SHA256, "size": ref.CompressedSize, "uncompressed_size": ref.UncompressedSize, "codec": ref.Codec, "schema_version": ref.SchemaVersion, "retention_class": ref.Retention, "storage_binding_id": "", "customer_owned": 0, "uploader": "runner_token_execution_import_second", "name": "archive.gz", "content_type": "application/gzip", "created_at": "2026-10-01T00:00:00Z"})
			}
			value = hubrecords.StoredRunCheckpoint{Detail: detail}
		}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		fixture.Set("native_attempts", map[string]string{"id": id}, map[string]any{"checkpoint_json": string(raw)})
	}
	return fixture.Write()
}
