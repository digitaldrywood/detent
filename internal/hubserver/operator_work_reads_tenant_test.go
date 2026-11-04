package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresOperatorItemReferencesUseCurrentAccessWithoutLegacyDatabase(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	details := &memoryRunDetails{}
	service := &Service{database: &database{}, config: Config{TenantRepository: repository, AccessRepository: repository, RunDetails: details, SecretKeys: secretTestKeys(t, "1", "1"), Logger: discardLogger(), now: func() time.Time { return now }}}
	var previousRecoveryCursor string
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		service.localOrganization = string(org)
		credential := apiCredential{ID: "reader", Hash: apikey.HashToken(string(org) + "_reader"), Scope: apiScopeOperator}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example organization", Slug: string(org), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: credential.ID, Hash: credential.Hash, Name: "Example reader", Scope: "operator", KeyScope: "read", Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			for _, project := range []tracker.ProjectID{"example", "hidden"} {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "Example " + string(project), Slug: string(project), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return err
				}
			}
			return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: credential.ID, ProjectID: "example"})
		}); err != nil {
			t.Fatal(err)
		}
		key, err := service.config.SecretKeys.Seal([]byte(strings.Repeat("k", 32)), accessstore.SecretAAD(org, "", accessstore.NativeCursorKeyKind))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Envelope: key, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: key.Version, RecordedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		for _, project := range []tracker.ProjectID{"example", "hidden"} {
			for number := 1; number <= 2; number++ {
				id := tracker.NativeWorkItemID(fmt.Sprintf("wi_%s_%d", project, number))
				actor := "reader"
				if number == 2 {
					actor = "other"
				}
				issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: id, Number: number, Revision: 1}, Title: string(org) + " example", Body: "Example body", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: actor}, CreatedAt: now, UpdatedAt: now}
				raw, err := json.Marshal(issue)
				if err != nil {
					t.Fatal(err)
				}
				version, err := eventstore.NewContent(details).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: id, RecordID: string(id), Revision: 1}, raw)
				if err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: id, Number: int64(number), Title: issue.Title, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, WorkflowStateName: &issue.State}); err != nil {
						return err
					}
					_, err := w.CreateCollaborationVersion(t.Context(), version)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
		change := tracker.ChangeRequest{OrganizationID: org, ProjectID: "example", WorkItemID: "wi_example_1", ID: "change_example", Title: string(org) + " retained change", Revision: 1, CreatedAt: now, UpdatedAt: now}
		changeRaw, err := json.Marshal(change)
		if err != nil {
			t.Fatal(err)
		}
		changeRow, err := eventstore.NewContent(details).PrepareChange(t.Context(), org, changeRaw)
		if err != nil {
			t.Fatal(err)
		}
		originalPolicy := policy.Descriptor{ID: "policy_original", SourceRevision: string(org) + " original policy"}
		policyRow, err := eventstore.NewContent(details).PrepareProjectPolicy(t.Context(), org, originalPolicy)
		if err != nil {
			t.Fatal(err)
		}
		policyRow.ProjectID, policyRow.ApprovedBy, policyRow.ApprovedAt = "example", "reader", now
		originalVersion := tracker.ChangeVersion{ID: "version_original", ChangeID: change.ID, Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("a", 40), PolicyID: originalPolicy.ID}, Actor: tracker.Actor{Kind: "human", PrincipalID: "reader"}, CreatedAt: now}
		versionRaw, err := json.Marshal(originalVersion)
		if err != nil {
			t.Fatal(err)
		}
		versionRow, err := eventstore.NewContent(details).PrepareVersion(t.Context(), org, versionRaw)
		if err != nil {
			t.Fatal(err)
		}
		review := tracker.ChangeReview{ID: "review_example", VersionID: originalVersion.ID, Validator: &gate.ValidatorResult{VersionID: originalVersion.ID, Commands: []gate.CommandResult{{Command: "example-check", Output: string(org) + " retained command output", OutputTruncated: true}}}, Decision: "approve", Body: string(org) + strings.Repeat(" retained review text", 5000), Actor: tracker.Actor{Kind: "human", PrincipalID: "reader"}, CreatedAt: now}
		reviewRaw, err := json.Marshal(review)
		if err != nil {
			t.Fatal(err)
		}
		reviewRow, err := eventstore.NewContent(details).PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: change.ID, Kind: "review"}, reviewRaw)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateChange(t.Context(), changeRow); err != nil {
				return err
			}
			if err := w.ImportProjectPolicyRevision(t.Context(), policyRow, false); err != nil {
				return err
			}
			if _, err := w.CreateVersion(t.Context(), versionRow); err != nil {
				return err
			}
			if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: "example", WorkItemID: "wi_example_1", ChangeID: change.ID, CreatedAt: now}); err != nil {
				return err
			}
			_, err := w.AppendEvidence(t.Context(), reviewRow)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		scope := nativeScope{organization: org, project: "example", credential: credential}
		for _, test := range []struct {
			name, reference, role string
			want                  error
		}{
			{"number", "1", "", nil},
			{"project number", "example#1", "", nil},
			{"native id", "wi_example_1", "", nil},
			{"readable path", "/" + string(org) + "/example/issues/1", "", nil},
			{"reporter own", "1", "reporter", nil},
			{"reporter private", "2", "reporter", explain.ErrNotFound},
			{"missing", "99", "", explain.ErrNotFound},
			{"invalid", "invalid", "", explain.ErrNotFound},
			{"wrong prefix", "hidden#1", "", explain.ErrNotFound},
			{"unauthorized project", "wi_hidden_1", "", operatortool.ErrProjectScopeRequired},
			{"reporter hidden", "wi_hidden_1", "reporter", explain.ErrNotFound},
		} {
			t.Run(string(org)+"/"+test.name, func(t *testing.T) {
				defer func() {
					if value := recover(); value != nil {
						t.Fatalf("operator read panicked with nil legacy database: %v", value)
					}
				}()
				current := scope
				current.credential.HostedRole = test.role
				item, err := service.resolveOperatorNativeReadItem(t.Context(), nil, current, test.reference)
				if err != nil {
					err = safeWorkReadError(err)
				}
				if test.want != nil {
					if !errors.Is(err, test.want) {
						t.Fatalf("error=%v, want %v", err, test.want)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if item.OrganizationID != org || item.ProjectID != "example" || item.WorkItemID != "wi_example_1" || item.Body != "Example body" || item.Title != string(org)+" example" || item.Revision != 1 {
					t.Fatalf("wrong current item identity or content")
				}
			})
		}
		identity := operatorIdentity(credential, string(org))
		ctx := operatortool.WithConnection(t.Context(), operatortool.Connection{Identity: identity, Resolve: func(context.Context) (operatortool.Authority, error) {
			return operatortool.Authority{Identity: identity, Check: func(ctx context.Context, req operatortool.Requirement) error {
				return repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error { return service.authorizeAccessScope(ctx, r, scope, false) })
			}}, nil
		}})
		for _, tool := range []string{operatortool.WorkItem, operatortool.WorkExport} {
			t.Run(string(org)+"/"+tool, func(t *testing.T) {
				defer func() {
					if value := recover(); value != nil {
						t.Fatalf("operator tool panicked: %v", value)
					}
				}()
				result, err := (operatorWorkReads{service: service, scope: scope}).ReadWork(ctx, tool, operatortool.WorkReadRequest{ProjectID: "example", Reference: strconv.Itoa(1), Limit: 10})
				if err != nil {
					t.Fatal(err)
				}
				var response operatortool.WorkReadResult[operatortool.NativeItem]
				if err := json.Unmarshal(result.Content, &response); err != nil {
					t.Fatal(err)
				}
				if response.ProjectID != "example" || response.Reference != "wi_example_1" || response.Freshness != explain.SourceAvailable || response.Data.OrganizationID != org || response.Data.ProjectID != "example" || response.Data.WorkItemID != "wi_example_1" || response.Data.Number != 1 || response.Data.Revision != 1 || response.Data.Title != string(org)+" example" || response.Data.Body != "Example body" {
					t.Fatal("operator response lost current item identity or body")
				}
				if tool == operatortool.WorkItem && response.Data.Identifier != "example#1" {
					t.Fatal("work item identifier is missing")
				}
			})
		}

		var recoveryCursor string
		for _, role := range []string{"", "reporter"} {
			t.Run(string(org)+"/retained recovery/"+role, func(t *testing.T) {
				scope := scope
				scope.credential.HostedRole = role
				defer func() {
					if value := recover(); value != nil {
						t.Fatalf("operator recovery panicked with nil legacy database: %v", value)
					}
				}()
				request := operatortool.WorkReadRequest{ProjectID: "example", Reference: "1", View: "recovery", Limit: 1}
				var assembled []byte
				var section string
				seen := map[string]bool{}
				for {
					result, err := (operatorWorkReads{service: service, scope: scope}).ReadWork(ctx, operatortool.WorkItem, request)
					if err != nil {
						t.Fatal(err)
					}
					var response operatortool.WorkReadResult[tracker.NativeRecoveryPage]
					if err := json.Unmarshal(result.Content, &response); err != nil {
						t.Fatal(err)
					}
					page := response.Data
					if page.OrganizationID != org || page.ProjectID != "example" || page.WorkItemID != "wi_example_1" || page.Revision != 1 {
						t.Fatal("retained fragment lost tenant or source identity")
					}
					if page.Section == "complete" {
						if page.NextCursor != "" || len(assembled) != 0 || !seen["reviews"] || !seen["change"] || !seen["issue_body"] || !seen["versions"] {
							t.Fatal("recovery claimed completion before retained records")
						}
						break
					}
					if len(assembled) == 0 {
						section = page.Section
					}
					if page.Section != section || page.Offset != int64(len(assembled)) || page.NextCursor == "" {
						t.Fatal("fragment identity or continuation changed")
					}
					assembled = append(assembled, page.Data...)
					if int64(len(assembled)) == page.TotalBytes {
						digest := sha256.Sum256(assembled)
						if page.SHA256 != hex.EncodeToString(digest[:]) {
							t.Fatal("retained record digest mismatch")
						}
						switch page.Section {
						case "reviews":
							var value tracker.ChangeReview
							if json.Unmarshal(assembled, &value) != nil || value.Body != review.Body || value.Validator == nil || !reflect.DeepEqual(value.Validator.Commands, review.Validator.Commands) || value.ID != review.ID || page.RecordID != "1" || page.VersionID != review.VersionID || page.Offset == 0 {
								t.Fatal("retained review content or fragmentation lost")
							}
						case "versions":
							var value tracker.ChangeVersion
							if json.Unmarshal(assembled, &value) != nil || value.ID != originalVersion.ID || page.RecordID != value.ID || page.VersionID != value.ID || value.HeadSHA != originalVersion.HeadSHA || value.Policy.SourceRevision != originalPolicy.SourceRevision || value.Policy.ID != originalPolicy.ID {
								t.Fatal("retained version source or original policy lost")
							}
						case "change":
							var value tracker.ChangeRequest
							if json.Unmarshal(assembled, &value) != nil || value.Title != change.Title || value.ID != change.ID || page.RecordID != change.ID {
								t.Fatal("retained Change content lost")
							}
							if !reflect.DeepEqual(value.LinkedIssues, []tracker.NativeWorkItemID{"wi_example_1"}) {
								t.Fatalf("visible linked issues=%v, want owned primary", value.LinkedIssues)
							}

						case "issue_body":
							var value string
							if json.Unmarshal(assembled, &value) != nil || value != "Example body" || page.RecordID != "wi_example_1" {
								t.Fatal("retained body content lost")
							}
						default:
							t.Fatalf("unexpected retained section %s", page.Section)
						}
						seen[page.Section] = true
						assembled = nil
					}
					request.Cursor = page.NextCursor
					if recoveryCursor == "" {
						recoveryCursor = request.Cursor
					}
				}
				if role == "" && previousRecoveryCursor != "" {
					request.Cursor = previousRecoveryCursor
					if _, err := (operatorWorkReads{service: service, scope: scope}).ReadWork(ctx, operatortool.WorkItem, request); err == nil {
						t.Fatal("accepted other organization cursor")
					}
				}
				if role == "" {
					previousRecoveryCursor = recoveryCursor
				}
				request.Cursor = recoveryCursor
				request.Reference = "2"
				if _, err := (operatorWorkReads{service: service, scope: scope}).ReadWork(ctx, operatortool.WorkItem, request); err == nil {
					t.Fatal("accepted foreign source cursor")
				}
				request.ProjectID, request.Reference = "hidden", "1"
				if _, err := (operatorWorkReads{service: service, scope: scope}).ReadWork(ctx, operatortool.WorkItem, request); err == nil {
					t.Fatal("accepted unauthorized project cursor")
				}
			})

		}
		t.Run(string(org)+"/reporter hidden primary owned linked", func(t *testing.T) {
			hidden := change
			hidden.ID, hidden.WorkItemID = "change_hidden_primary", "wi_example_2"
			raw, err := json.Marshal(hidden)
			if err != nil {
				t.Fatal(err)
			}
			row, err := eventstore.NewContent(details).PrepareChange(t.Context(), org, raw)
			if err != nil {
				t.Fatal(err)
			}
			hiddenVersion := originalVersion
			hiddenVersion.ID, hiddenVersion.ChangeID = "version_hidden", hidden.ID
			raw, err = json.Marshal(hiddenVersion)
			if err != nil {
				t.Fatal(err)
			}
			hiddenVersionRow, err := eventstore.NewContent(details).PrepareVersion(t.Context(), org, raw)
			if err != nil {
				t.Fatal(err)
			}
			hiddenReview := review
			hiddenReview.VersionID = hiddenVersion.ID
			raw, err = json.Marshal(hiddenReview)
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := eventstore.NewContent(details).PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: hidden.ID, Kind: "review"}, raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateChange(t.Context(), row); err != nil {
					return err
				}
				if _, err := w.CreateVersion(t.Context(), hiddenVersionRow); err != nil {
					return err
				}
				for _, item := range []tracker.NativeWorkItemID{"wi_example_1", "wi_example_2"} {
					if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: "example", WorkItemID: item, ChangeID: hidden.ID, CreatedAt: now}); err != nil {
						return err
					}
				}
				_, err := w.AppendEvidence(t.Context(), evidence)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			reporter := scope
			reporter.credential.HostedRole = "reporter"
			result, err := (operatorWorkReads{service: service, scope: reporter}).ReadWork(ctx, operatortool.WorkItem, operatortool.WorkReadRequest{ProjectID: "example", Reference: "1", View: "recovery", Limit: 1})
			if err == nil {
				var response operatortool.WorkReadResult[tracker.NativeRecoveryPage]
				if err := json.Unmarshal(result.Content, &response); err != nil {
					t.Fatal(err)
				}
				t.Fatalf("reporter leaked hidden primary first fragment: successful MCP response section=%s bytes=%d sha256=%s cursor=%t", response.Data.Section, len(response.Data.Data), response.Data.SHA256, response.Data.NextCursor != "")
			}
			if !errors.Is(err, explain.ErrNotFound) {
				t.Fatalf("hidden primary error=%v, want not found", err)
			}
		})

		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: credential.ID, ProjectID: "hidden"})
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := (operatorWorkReads{service: service, scope: scope}).ReadWork(ctx, operatortool.WorkItem, operatortool.WorkReadRequest{ProjectID: "example", Reference: "wi_hidden_1", View: "recovery", Limit: 1}); err == nil {
			t.Fatal("recovery accepted wrong authorized project")
		}
		if _, err := service.resolveOperatorNativeReadItem(t.Context(), nil, scope, "wi_hidden_1"); !isNativeNotFound(err) {
			t.Fatalf("wrong authorized project error=%v", err)
		}
		updated := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "example", WorkItemID: "wi_example_1", Number: 1, Revision: 2}, Title: "Current title", Body: "Current body", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "reader"}, CreatedAt: now, UpdatedAt: now}
		raw, err := json.Marshal(updated)
		if err != nil {
			t.Fatal(err)
		}
		version, err := eventstore.NewContent(details).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "example", WorkItemID: updated.WorkItemID, RecordID: string(updated.WorkItemID), Revision: 2}, raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			row, err := w.WorkItem(t.Context(), "example", updated.WorkItemID)
			if err != nil {
				return err
			}
			row.Title = updated.Title
			if _, err := w.UpdateWorkItem(t.Context(), row); err != nil {
				return err
			}
			_, err = w.CreateCollaborationVersion(t.Context(), version)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := (operatorWorkReads{service: service, scope: scope}).ReadWork(ctx, operatortool.WorkItem, operatortool.WorkReadRequest{ProjectID: "example", Reference: "1", View: "recovery", Limit: 1, Cursor: recoveryCursor}); err == nil {
			t.Fatal("recovery accepted stale source revision")
		}
		current, err := service.resolveOperatorNativeReadItem(t.Context(), nil, scope, "1")
		if err != nil {
			t.Fatal(err)
		}
		if current.Revision != 2 || current.Title != "Current title" || current.Body != "Current body" {
			t.Fatal("operator resolver read stale revision")
		}
		fresh, err := (operatorWorkReads{service: service, scope: scope}).ReadWork(ctx, operatortool.WorkItem, operatortool.WorkReadRequest{ProjectID: "example", Reference: "1", View: "recovery", Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		var freshPage operatortool.WorkReadResult[tracker.NativeRecoveryPage]
		if err := json.Unmarshal(fresh.Content, &freshPage); err != nil {
			t.Fatal(err)
		}
		recoveryCursor = freshPage.Data.NextCursor
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.DeleteAccessTokenGrant(t.Context(), credential.ID, "example")
		}); err != nil {
			t.Fatal(err)
		}
		t.Run(string(org)+"/revoked grant", func(t *testing.T) {
			defer func() {
				if value := recover(); value != nil {
					t.Fatalf("revoked read panicked: %v", value)
				}
			}()
			if _, err := (operatorWorkReads{service: service, scope: scope}).ReadWork(ctx, operatortool.WorkItem, operatortool.WorkReadRequest{ProjectID: "example", Reference: "1", View: "recovery", Limit: 1, Cursor: recoveryCursor}); err == nil {
				t.Fatal("recovery accepted revoked grant")
			}
			if _, err := service.resolveOperatorNativeReadItem(t.Context(), nil, scope, "1"); err == nil {
				t.Fatal("revoked project grant still reads item")
			}
		})
	}
}
