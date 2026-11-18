package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresMCPChangeTools(t *testing.T) {
	t.Parallel()
	service := newPostgresNativeService(t, Config{})
	for range 2 {
		organization := tracker.OrganizationID(artifact.NewID("org"))
		seedPostgresHubAdmin(t, service.config.TenantRepository, service, nativeOrganization{ID: organization, Name: "Example organization", Slug: strings.ReplaceAll(string(organization), "_", "-")})
		f := newChangeFixture(t, service, organization)
		t.Run(string(f.project.OrganizationID), func(t *testing.T) {
			version := publishReviewVersion(t, f, "mcp-review", "")
			bundle := seedReviewBundle(t, f, version, f.service.config.Clock().Add(time.Hour))
			file := tracker.ChangeViewedFile{ManifestSHA256: bundle.SHA256, FileSHA256: strings.Repeat("f", 64), Viewed: true}
			viewsPath := f.path + "/versions/" + version.ID + "/viewed-files"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, viewsPath, f.token, tracker.ViewChangeFile{Mutation: tracker.Mutation{IdempotencyKey: "seed-view"}, Bundle: bundle, FileSHA256: file.FileSHA256, Viewed: true}), http.StatusOK)
			diff := claimAttemptDiffFixture(t, false, f.nativeFixture)
			diff.start(t)
			request := diff.request(1, tracker.AttemptDiffFile{Path: "example.go", Status: "modified", Patch: "+example"}, tracker.AttemptDiffFile{Path: "second.go", Status: "added", Patch: "+second"})
			requireNativeStatus(t, diff.post(t, request), http.StatusAccepted)
			var wantDiff tracker.AttemptDiff
			response := diff.get(t, "")
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &wantDiff)
			var attempt tracker.NativeAttempt
			response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(diff.issue.WorkItemID)+"/attempts/"+diff.attempt, f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &attempt)
			operator := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
			admin := parityNativeContext(t, f.service, f.project.OrganizationID)
			other := newNativeFixture(t, f.service, f.project.OrganizationID, "Invisible project")
			invisible := changeOperatorContext(t, f.service, other.token, string(f.project.OrganizationID))
			worker := changeOperatorContext(t, f.service, diff.worker, string(f.project.OrganizationID))
			tests := []struct {
				name  string
				args  operatortool.ChangeArguments
				ctx   context.Context
				check func(*testing.T, operatortool.ChangeResult)
			}{
				{operatortool.GetAttemptDiff, operatortool.ChangeArguments{ItemID: string(diff.issue.WorkItemID), AttemptID: diff.attempt, Sequence: 1, Limit: 1}, operator, func(t *testing.T, got operatortool.ChangeResult) {
					want := wantDiff
					want.Files = want.Files[:1]
					if !reflect.DeepEqual(got.Diff, &want) || got.NextOffset == nil || *got.NextOffset != 1 {
						t.Fatalf("attempt diff payload: %+v", got)
					}
				}},
				{operatortool.GetWorkItemDiff, operatortool.ChangeArguments{ItemID: string(diff.issue.WorkItemID)}, operator, func(t *testing.T, got operatortool.ChangeResult) {
					if !reflect.DeepEqual(got.Diff, &wantDiff) {
						t.Fatalf("work item diff payload: %+v", got.Diff)
					}
				}},
				{operatortool.GetNativeRun, operatortool.ChangeArguments{ItemID: string(diff.issue.WorkItemID), AttemptID: diff.attempt}, operator, func(t *testing.T, got operatortool.ChangeResult) {
					if !reflect.DeepEqual(got.Attempt, &attempt) {
						t.Fatalf("native attempt payload: %+v", got.Attempt)
					}
				}},
				{operatortool.ViewChangeFile, operatortool.ChangeArguments{ItemID: string(f.issue.WorkItemID), ChangeID: f.change.ID, VersionID: version.ID, RequestID: "mcp-view", Bundle: &bundle, FileSHA256: file.FileSHA256, Viewed: true}, operator, func(t *testing.T, got operatortool.ChangeResult) {
					var receipt tracker.ChangeViewedFile
					if err := json.Unmarshal(got.Receipt, &receipt); err != nil || receipt != file {
						t.Fatalf("view receipt: %s error=%v", got.Receipt, err)
					}
				}},
				{operatortool.ChangeViewedFiles, operatortool.ChangeArguments{ItemID: string(f.issue.WorkItemID), ChangeID: f.change.ID, VersionID: version.ID}, operator, func(t *testing.T, got operatortool.ChangeResult) {
					if !reflect.DeepEqual(got.Viewed, []tracker.ChangeViewedFile{file}) {
						t.Fatalf("viewed files: %+v", got.Viewed)
					}
				}},
				{operatortool.ApproveChangeReviewPolicy, operatortool.ChangeArguments{RequestID: "mcp-policy", ExpectedPolicyID: f.rules.ID, Policy: &f.rules}, admin, func(t *testing.T, got operatortool.ChangeResult) {
					var receipt tracker.ChangeReviewPolicy
					if err := json.Unmarshal(got.Receipt, &receipt); err != nil || !reflect.DeepEqual(receipt, f.rules) {
						t.Fatalf("policy receipt: %s error=%v", got.Receipt, err)
					}
				}},
			}
			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					test.args.ProjectID = string(f.project.ID)
					call := parityToolCall(t, test.name, test.args)
					for _, caller := range []struct {
						name   string
						ctx    context.Context
						denied bool
					}{
						{"authorized", test.ctx, false}, {"not-visible", invisible, true}, {"insufficient-scope", worker, true},
					} {
						t.Run(caller.name, func(t *testing.T) {
							result, err := (hostedOperatorExecutor{f.service}).Execute(caller.ctx, call)
							if caller.denied {
								if !errors.Is(err, operatortool.ErrAccessDenied) || len(result.Content) != 0 {
									t.Fatalf("resource disclosure: %s error=%v", result.Content, err)
								}
								return
							}
							paritySuccess(t, result, err)
							var got operatortool.ChangeResult
							if err := json.Unmarshal(result.Content, &got); err != nil {
								t.Fatal(err)
							}
							if got.OrganizationID != string(f.project.OrganizationID) || got.ProjectID != string(f.project.ID) || got.WorkItemID != test.args.ItemID || got.Freshness != "live" || got.GeneratedAt.IsZero() || got.URL == "" {
								t.Fatalf("result envelope: %+v", got)
							}
							test.check(t, got)
							definitions, err := (hostedOperatorExecutor{f.service}).ListTools(admin)
							if err != nil || !entryKeyHasDefinition(definitions, test.name) {
								t.Fatalf("tool not advertised: %v", err)
							}
						})
					}
				})
			}
			t.Run("empty-work-item-diff", func(t *testing.T) {
				result, err := (hostedOperatorExecutor{f.service}).Execute(operator, parityToolCall(t, operatortool.GetWorkItemDiff, operatortool.ChangeArguments{ProjectID: string(f.project.ID), ItemID: string(f.issue.WorkItemID), Source: tracker.DiffSourceWorkspace}))
				paritySuccess(t, result, err)
				if !bytes.Contains(result.Content, []byte(`"diff":null`)) {
					t.Fatalf("missing null diff: %s", result.Content)
				}
			})
		})
	}
}

func TestPostgresMCPReviewPolicyAuthorization(t *testing.T) {
	t.Parallel()
	repository, _ := projectRecordPostgresWithAdmin(t, 4)
	details := &memoryRunDetails{}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(organization), func(t *testing.T) {
			f, owner := newChangePolicyPostgresTenantFixture(t, repository, organization, details)
			ctx := parityHostedContext(t, f, owner)
			rules := defaultChangeReviewPolicy(hubTestPolicy())
			rules.RequireReview = true
			setRole := func(role string) error {
				return repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					member, err := w.AccessMember(t.Context(), owner.identity.Subject)
					if err != nil {
						return err
					}
					member.Role = role
					return w.UpdateAccessMember(t.Context(), member)
				})
			}
			t.Run("revoked-mid-request", func(t *testing.T) {
				intercepted := &projectUpdateBeforeWriteRepository{Repository: repository, before: func() error { return setRole("member") }}
				f.service.config.TenantRepository = intercepted
				t.Cleanup(func() {
					f.service.config.TenantRepository = repository
					if err := setRole("owner"); err != nil {
						t.Error(err)
					}
				})
				call := parityToolCall(t, operatortool.ApproveChangeReviewPolicy, operatortool.ChangeArguments{ProjectID: string(f.project), RequestID: "mcp-mid-request", ExpectedPolicyID: defaultChangeReviewPolicy(hubTestPolicy()).ID, Policy: &rules})
				result, err := (hostedOperatorExecutor{f.service}).Execute(ctx, call)
				if !errors.Is(err, operatortool.ErrAccessDenied) || len(result.Content) != 0 {
					t.Fatalf("revoked mutation: %s error=%v", result.Content, err)
				}
				if intercepted.before != nil {
					t.Fatal("mutation did not reach tenant writer")
				}
				if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					_, err := r.ReviewPolicy(t.Context(), f.project)
					if !errors.Is(err, corestore.ErrNotFound) {
						t.Fatalf("revoked mutation stored policy: %v", err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
			f.service.config.TenantRepository = repository
			args := operatortool.ChangeArguments{ProjectID: string(f.project), RequestID: "mcp-policy-authorization", ExpectedPolicyID: defaultChangeReviewPolicy(hubTestPolicy()).ID, Policy: &rules}
			call := parityToolCall(t, operatortool.ApproveChangeReviewPolicy, args)
			result, err := (hostedOperatorExecutor{f.service}).Execute(ctx, call)
			paritySuccess(t, result, err)
			var original operatortool.ChangeResult
			if err := json.Unmarshal(result.Content, &original); err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name    string
				demote  bool
				request string
			}{
				{"authorized-replay", false, args.RequestID}, {"revoked-replay", true, args.RequestID}, {"revoked-new-request", true, "mcp-revoked"},
			} {
				t.Run(test.name, func(t *testing.T) {
					if test.demote {
						if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
							member, err := w.AccessMember(t.Context(), owner.identity.Subject)
							if err != nil {
								return err
							}
							member.Role = "member"
							return w.UpdateAccessMember(t.Context(), member)
						}); err != nil {
							t.Fatal(err)
						}
					}
					copy := args
					copy.RequestID = test.request
					result, err := (hostedOperatorExecutor{f.service}).Execute(ctx, parityToolCall(t, operatortool.ApproveChangeReviewPolicy, copy))
					if test.demote {
						if !errors.Is(err, operatortool.ErrAccessDenied) || len(result.Content) != 0 {
							t.Fatalf("demoted caller received receipt: %s error=%v", result.Content, err)
						}
						return
					}
					paritySuccess(t, result, err)
					var replay operatortool.ChangeResult
					if err := json.Unmarshal(result.Content, &replay); err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(replay.Receipt, original.Receipt) {
						t.Fatalf("replay receipt changed: %s", replay.Receipt)
					}
				})
			}
		})
	}
}

func TestPostgresMCPChangeToolRoles(t *testing.T) {
	t.Parallel()
	repository, _ := projectRecordPostgresWithAdmin(t, 4)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(organization), func(t *testing.T) {
			f, owner := newChangePolicyPostgresTenantFixture(t, repository, organization, &memoryRunDetails{})
			response := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "role-issue"}, Title: "Example private issue", Body: workItemRecordBody, State: "Todo"})
			requireNativeStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, response, &issue)
			reporter := f.user(t, "reporter", "reporter", "reporter@example.test", "", "")
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				member, err := w.AccessMember(t.Context(), reporter.identity.Subject)
				if err != nil {
					return err
				}
				if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: member.UserID, ProjectID: f.project, CanWrite: true}); err != nil {
					return err
				}
				return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: member.PrincipalID, ProjectID: f.project})
			}); err != nil {
				t.Fatal(err)
			}
			ctx := parityHostedContext(t, f, reporter)
			for _, name := range []string{operatortool.GetAttemptDiff, operatortool.GetWorkItemDiff, operatortool.GetNativeRun, operatortool.ChangeViewedFiles, operatortool.ViewChangeFile, operatortool.ApproveChangeReviewPolicy} {
				t.Run(name, func(t *testing.T) {
					rules := defaultChangeReviewPolicy(hubTestPolicy())
					bundle := tracker.ChangeReviewBundle{ArtifactID: artifact.NewID("artifact"), Revision: 1, SHA256: strings.Repeat("a", 64), HeadSHA: strings.Repeat("b", 40)}
					args := operatortool.ChangeArguments{ProjectID: string(f.project), ItemID: string(issue.WorkItemID), AttemptID: artifact.NewID("attempt"), ChangeID: artifact.NewID("change"), VersionID: artifact.NewID("version"), RequestID: "role-refusal", Policy: &rules, ExpectedPolicyID: rules.ID, Bundle: &bundle, FileSHA256: strings.Repeat("f", 64), Viewed: true}
					result, err := (hostedOperatorExecutor{f.service}).Execute(ctx, parityToolCall(t, name, args))
					if !errors.Is(err, operatortool.ErrAccessDenied) || len(result.Content) != 0 {
						t.Fatalf("reporter disclosed another principal's work: %s error=%v", result.Content, err)
					}
				})
			}
		})
	}
}
