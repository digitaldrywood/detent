package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedIndependentChangeChecks(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	t.Cleanup(func() {
		if err := f.service.Close(); err != nil {
			t.Error(err)
		}
	})
	var live []struct {
		name, path string
		created    tokenResponse
		version    tracker.ChangeVersion
	}
	for _, test := range []struct {
		name, project string
		review        bool
	}{
		{"human review", f.project, true},
		{"automatic", f.privateProject, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := "/api/v2/organizations/org_browser_preview/projects/" + test.project
			requireNativeStatus(t, f.form(t, "owner", "/organization/grants", url.Values{"user": {"user_browser_owner"}, "project": {test.project}, "write": {"true"}}), http.StatusSeeOther)
			descriptor := hubTestPolicy()
			descriptor.ConfigDigest = policy.Digest([]byte(test.project))
			descriptor.Gates.Kind = "command"
			if test.review {
				descriptor.Gates.Kind = "human_review"
			}
			descriptor.Gates.RequiredChecks, descriptor.Gates.Validator, descriptor.Gates.AutomatedReview = 1, true, ""
			if !test.review {
				descriptor.Gates.AutomatedReview = "off"
				descriptor.Gates.AutoPromote = true
			}
			descriptor = descriptor.WithID()
			requireNativeStatus(t, f.setupRequest(t, "owner", http.MethodPut, base+"/onboarding/policy", policy.Change{ExpectedID: hubTestPolicy().ID, Policy: descriptor}), http.StatusOK)
			reopenCredentialFixture(t, f, true)
			response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v1/tokens", testHubAdminToken, tokenRequest{Name: "ci-" + test.project, Scope: apiScopeOperator})
			requireNativeStatus(t, response, http.StatusCreated)
			var created tokenResponse
			decodeHubResponse(t, response, &created)
			principal, token := created.ID, created.Token
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/tokens/"+principal+"/grants", testHubAdminToken, map[string]string{"organization_id": "org_browser_preview", "project_id": test.project}), http.StatusNoContent)
			reopenCredentialFixture(t, f, false)
			rules := tracker.ChangeReviewPolicy{PolicyID: descriptor.ID, RequireReview: test.review, RequiredChecks: []tracker.ChangeCheckSpec{{Name: "test", PrincipalID: principal, WorkflowID: "ci.yml", WorkflowSHA256: policy.Digest([]byte("trusted CI")), Source: "independent", MaxAgeSeconds: 3600}}}
			requireNativeStatus(t, f.setupRequest(t, "owner", http.MethodPut, base+"/change-review-policy", tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "rules"}, Policy: rules}), http.StatusOK)
			response = f.setupRequest(t, "owner", http.MethodPost, base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "work"}, Title: "Independent CI", Body: nativeContractTestBody, State: "Todo"})
			requireNativeStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, response, &issue)
			path := base + "/work-items/" + string(issue.WorkItemID) + "/changes"
			response = f.setupRequest(t, "owner", http.MethodPost, path, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Independent CI"})
			requireNativeStatus(t, response, http.StatusOK)
			var change tracker.ChangeRequest
			decodeHubResponse(t, response, &change)
			path += "/" + change.ID
			input := changeTestInput()
			input.PolicyID = descriptor.ID
			input.External = &tracker.ChangeExternalReference{Provider: "github", ID: "123", URL: "https://github.com/example/repo/pull/123"}
			response = f.setupRequest(t, "owner", http.MethodPost, path+"/versions", tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "publish"}, ChangeVersionInput: input})
			requireNativeStatus(t, response, http.StatusOK)
			var version tracker.ChangeVersion
			decodeHubResponse(t, response, &version)
			checkPath := path + "/versions/" + version.ID + "/checks"
			detail := func() tracker.ChangeDetail {
				t.Helper()
				response := f.setupRequest(t, "owner", http.MethodGet, path, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var result tracker.ChangeDetail
				decodeHubResponse(t, response, &result)
				return result
			}
			if summary := detail().Summary; summary.Checks != "missing" || summary.Status != "needs_evidence" {
				t.Fatalf("before checks: %+v", summary)
			}
			testHostedCheckBoundaries(t, f, base, checkPath, principal, token, version)
			variants := prepareHostedCheckPolicyVariants(t, f, tracker.ProjectID(test.project))
			testHostedCheckRevocation(t, f, base, checkPath, principal, token, version, variants, false)
			for _, invalid := range []struct {
				name string
				edit func(*tracker.SubmitChangeCheck)
				want int
			}{
				{"head", func(r *tracker.SubmitChangeCheck) { r.HeadSHA = strings.Repeat("f", 40) }, 422},
				{"run", func(r *tracker.SubmitChangeCheck) { r.RunID = "wrong" }, 422},
				{"check run", func(r *tracker.SubmitChangeCheck) { r.CheckRunID = "wrong" }, 422},
				{"policy", func(r *tracker.SubmitChangeCheck) { r.PolicyID = "wrong" }, 422},
				{"config", func(r *tracker.SubmitChangeCheck) { r.ConfigDigest = "wrong" }, 422},
				{"workflow", func(r *tracker.SubmitChangeCheck) { r.WorkflowID = "wrong" }, 422},
				{"workflow digest", func(r *tracker.SubmitChangeCheck) { r.WorkflowSHA256 = "wrong" }, 422},
				{"source", func(r *tracker.SubmitChangeCheck) { r.Source = "customer" }, 404},
				{"old completion", func(r *tracker.SubmitChangeCheck) { r.CompletedAt = version.CreatedAt.Add(-time.Second) }, 422},
				{"future completion", func(r *tracker.SubmitChangeCheck) { r.CompletedAt = time.Now().Add(time.Hour) }, 422},
			} {
				t.Run(invalid.name, func(t *testing.T) {
					request := changeTestResult(version)
					request.IdempotencyKey = invalid.name
					invalid.edit(&request)
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, checkPath, token, request), invalid.want)
				})
			}
			request := changeTestResult(version)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, checkPath, token, request), http.StatusOK)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, checkPath, token, request), http.StatusOK)
			testHostedCheckRevocation(t, f, base, checkPath, principal, token, version, variants, true)
			request.Conclusion = "failure"
			for _, key := range []string{"check", "changed-result"} {
				request.IdempotencyKey = key
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, checkPath, token, request), http.StatusConflict)
			}
			current := detail()
			if len(current.Checks) != 1 || current.Summary.Checks != "success" {
				t.Fatalf("accepted checks: %+v", current)
			}
			if test.review {
				if current.Summary.Status != "needs_evidence" || current.Summary.NativeReview != "pending" {
					t.Fatalf("review bypassed: %+v", current.Summary)
				}
				requireNativeStatus(t, f.setupRequest(t, "owner", http.MethodPost, path+"/versions/"+version.ID+"/reviews", tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approve"}, Decision: "approved"}), http.StatusOK)
			}
			if summary := detail().Summary; summary.Status != "needs_evidence" || summary.ExternalReview != "external_gate" {
				t.Fatalf("readiness or external protection changed: %+v", summary)
			}
			func() {
				service := f.service
				now := service.config.now
				ownerCookie := f.cookies["owner"]
				if ownerCookie == nil {
					t.Fatal("owner freshness fixture cookie is missing")
				}
				staleAt := version.CreatedAt.Add(time.Hour)
				identity := f.provider.identity("user_browser_owner", "owner@example.test", f.provider.organization.ID, "")
				identity.Hosted.ExpiresAt = staleAt.Add(browserHostedPreviewLifetime)
				f.provider.mu.Lock()
				f.provider.sessions[identity.Hosted.SessionID] = *identity.Hosted
				f.provider.mu.Unlock()
				token, session := browserHostedSession(t, service, identity)
				freshCookie := *ownerCookie
				freshCookie.Value, freshCookie.Expires = token, session.ExpiresAt
				defer func() {
					service.config.now = now
					f.cookies["owner"] = ownerCookie
				}()
				service.config.now = func() time.Time { return staleAt }
				requireNativeStatus(t, f.setupRequest(t, "owner", http.MethodGet, path, nil), http.StatusForbidden)
				f.cookies["owner"] = &freshCookie
				if summary := detail().Summary; summary.Checks != "stale" || summary.Status != "needs_evidence" {
					t.Fatalf("freshness bypassed: %+v", summary)
				}
			}()
			live = append(live, struct {
				name, path string
				created    tokenResponse
				version    tracker.ChangeVersion
			}{test.name, checkPath, created, version})
		})
	}
	if len(live) != 2 {
		t.Fatal("both project scenarios must finish before isolation checks")
	}
	for i, current := range live {
		t.Run(current.name+"/live project isolation", func(t *testing.T) {
			other := live[1-i]
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, current.path, current.created.Token, changeTestResult(current.version)), http.StatusOK)
			before := hostedIndependentStoredChecks(t, f, other.path)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, other.path, current.created.Token, changeTestResult(other.version)), http.StatusNotFound)
			if after := hostedIndependentStoredChecks(t, f, other.path); !reflect.DeepEqual(after, before) {
				t.Fatal("foreign project submission changed accepted checks")
			}
		})
	}
	for _, current := range live {
		t.Run(current.name+"/maintenance", func(t *testing.T) {
			testMaintenanceCheckLifecycle(t, f, current.path, current.created, current.version)
		})
	}
}

func testHostedCheckBoundaries(t *testing.T, f *browserHostedFixture, base, path, principal, token string, version tracker.ChangeVersion) {
	t.Helper()
	for _, scope := range []apiScope{apiScopeWorker, apiScopeOperator, apiScopeAdmin} {
		id := principal + "-" + string(scope)
		created, err := parseTimeValue(testTimestamp)
		if err != nil {
			t.Fatal(err)
		}
		hash := apikey.HashToken(id)
		organization := f.service.accessOrganization()
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: id, Name: id, Hash: hash, Fingerprint: tokenFingerprint(hash), Scope: string(scope), Kind: "service", ProjectAccess: "selected", CreatedAt: created, UpdatedAt: created}); err != nil {
				return err
			}
			return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: id, ProjectID: tracker.ProjectID(strings.TrimPrefix(base, "/api/v2/organizations/org_browser_preview/projects/"))})
		}); err != nil {
			t.Fatal(err)
		}
		t.Run("unapproved "+string(scope), func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, id, changeTestResult(version)), http.StatusNotFound)
		})
	}
	requireNativeStatus(t, f.setupRequest(t, "owner", http.MethodPost, path, changeTestResult(version)), http.StatusNotFound)
	for _, test := range []struct{ name, method, path string }{
		{"change read", http.MethodGet, strings.Split(path, "/versions/")[0]},
		{"review", http.MethodPost, strings.TrimSuffix(path, "checks") + "reviews"},
		{"publish", http.MethodPost, strings.Split(path, "/versions/")[0] + "/versions"},
		{"policy", http.MethodPut, base + "/change-review-policy"},
		{"token administration", http.MethodPost, "/api/v1/tokens"},
		{"grant administration", http.MethodPost, "/api/v2/tokens/" + principal + "/grants"},
		{"wrong organization", http.MethodPost, strings.Replace(path, "org_browser_preview", "org_other", 1)},
		{"wrong project", http.MethodPost, strings.Replace(path, base, "/api/v2/organizations/org_browser_preview/projects/prj_unknown", 1)},
		{"wrong item", http.MethodPost, strings.Replace(path, "/work-items/", "/work-items/other", 1)},
		{"wrong version", http.MethodPost, strings.Replace(path, version.ID, "version_other", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := http.StatusNotFound
			before := hostedIndependentStoredChecks(t, f, path)
			response := performHubAPIRequest(t, f.service, test.method, test.path, token, changeTestResult(version))
			if test.name == "token administration" {
				requireNativeStatus(t, response, http.StatusForbidden)
				var refusal apiErrorResponse
				decodeHubResponse(t, response, &refusal)
				if refusal.Code != "invalid_csrf" {
					t.Fatalf("bearer-only reserved mutation refusal = %s", refusal.Code)
				}
				if after := hostedIndependentStoredChecks(t, f, path); !reflect.DeepEqual(after, before) {
					t.Fatal("CSRF refusal changed accepted checks")
				}
				cookie := f.cookies["owner"]
				if cookie == nil {
					t.Fatal("owner CSRF fixture cookie is missing")
				}
				request := newHubAPIRequest(t, test.method, test.path, token, changeTestResult(version))
				request.Header.Set("Origin", f.server.URL)
				request.AddCookie(cookie)
				request.Header.Set("X-CSRF-Token", hostedCSRF(cookie.Value))
				response = httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, request)
				requireNativeStatus(t, response, want)
				apiKeys := strings.Split(base, "/projects/")[0] + "/api-keys"
				project := base[strings.LastIndex(base, "/")+1:]
				control := hostedKeyRequest{Kind: "service", Name: "ci-key-control-" + project, Scope: apikey.ScopeRead, NeverExpires: true, ProjectAccess: hostedProjectsSelected, Projects: []string{project}}
				created := f.setupRequest(t, "owner", http.MethodPost, apiKeys, control)
				requireNativeStatus(t, created, http.StatusCreated)
				var probe tokenResponse
				decodeHubResponse(t, created, &probe)
				defer func() {
					cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), f.service.config.ShutdownTimeout)
					defer cancel()
					request := newHubAPIRequest(t, http.MethodDelete, apiKeys+"/"+probe.ID, "", nil).WithContext(cleanup)
					request.AddCookie(cookie)
					request.Header.Set("Origin", f.server.URL)
					request.Header.Set("X-CSRF-Token", hostedCSRF(cookie.Value))
					response := httptest.NewRecorder()
					f.service.Handler().ServeHTTP(response, request)
					if response.Code != http.StatusNoContent {
						t.Errorf("owner key cleanup status = %d, want 204", response.Code)
					}
				}()
				readProbe := func() accessstore.Credential {
					t.Helper()
					var row accessstore.Credential
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						row, err = r.AccessCredential(t.Context(), probe.ID)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					return row
				}
				originalProbe := readProbe()
				if originalProbe.RevokedAt != nil || originalProbe.UserID != "user_browser_owner" || originalProbe.ID != probe.ID {
					t.Fatal("owner key control is not an active owned credential")
				}
				denied := control
				denied.Name = "ci-key-denied-" + project
				for _, mixed := range []bool{false, true} {
					for _, operation := range []struct {
						method, path string
						body         any
					}{
						{http.MethodPost, apiKeys, denied},
						{http.MethodDelete, apiKeys + "/" + probe.ID, nil},
					} {
						request := newHubAPIRequest(t, operation.method, operation.path, token, operation.body)
						if mixed {
							request.AddCookie(cookie)
							request.Header.Set("Origin", f.server.URL)
							request.Header.Set("X-CSRF-Token", hostedCSRF(cookie.Value))
						}
						response := httptest.NewRecorder()
						f.service.Handler().ServeHTTP(response, request)
						requireNativeStatus(t, response, http.StatusForbidden)
						var refusal apiErrorResponse
						decodeHubResponse(t, response, &refusal)
						if refusal.Code != "forbidden" {
							t.Fatalf("actual key owner refusal = %s, want forbidden", refusal.Code)
						}
						if after := readProbe(); !reflect.DeepEqual(after, originalProbe) {
							t.Fatal("denied check credential changed the owner key")
						}
						var count int
						if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM api_tokens WHERE organization_id=$1 AND name=$2", f.service.accessOrganization(), denied.Name).Scan(&count); err != nil || count != 0 {
							t.Fatalf("denied check credential created keys: count=%d error=%v", count, err)
						}
					}
				}
			} else {
				requireNativeStatus(t, response, want)
			}
			if after := hostedIndependentStoredChecks(t, f, path); !reflect.DeepEqual(after, before) {
				t.Fatal("denied submission changed accepted checks")
			}
		})
	}
}

func testHostedCheckRevocation(t *testing.T, f *browserHostedFixture, base, path, principal, token string, version tracker.ChangeVersion, variants hostedCheckPolicyVariants, replay bool) {
	t.Helper()
	ctx := t.Context()
	organization := f.service.accessOrganization()
	project := tracker.ProjectID(strings.TrimPrefix(base, "/api/v2/organizations/org_browser_preview/projects/"))
	var credential accessstore.Credential
	var grant accessstore.TokenGrant
	if err := f.service.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		credential, err = r.AccessCredential(ctx, principal)
		if err != nil {
			return err
		}
		grant, err = r.AccessTokenGrant(ctx, principal, project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	write := func(ctx context.Context, fn func(tenantstore.Writer) error) error {
		return f.service.config.TenantRepository.WriteTenant(ctx, organization, corestore.Serving, fn)
	}
	admin := hostedPostgresAdmin(t, f.service)
	cas := func(ctx context.Context, statement string, args ...any) error {
		result, err := admin.ExecContext(ctx, statement, args...)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("credential fixture CAS affected %d rows", count)
		}
		return nil
	}
	rotatedHash := apikey.HashToken(token + "/revocation-fixture")
	at := credential.CreatedAt
	policyMutation := func(row eventstore.ReviewPolicy, expected string) func(context.Context) error {
		return func(ctx context.Context) error {
			return write(ctx, func(w tenantstore.Writer) error { return w.SetReviewPolicy(ctx, row, expected) })
		}
	}
	for _, test := range []struct {
		name            string
		mutate, restore func(context.Context) error
		want            int
	}{
		{"revoked", func(ctx context.Context) error {
			return write(ctx, func(w tenantstore.Writer) error { return w.RevokeAccessCredential(ctx, principal, at) })
		}, func(ctx context.Context) error {
			return cas(ctx, `UPDATE api_tokens SET revoked_at=$3,updated_at=$4 WHERE organization_id=$1 AND id=$2 AND revoked_at=$5 AND updated_at=$5`, organization, principal, credential.RevokedAt, credential.UpdatedAt, at)
		}, 401},
		{"rotated", func(ctx context.Context) error {
			return write(ctx, func(w tenantstore.Writer) error {
				return w.RotateAccessCredential(ctx, principal, rotatedHash, tokenFingerprint(rotatedHash), at)
			})
		}, func(ctx context.Context) error {
			return cas(ctx, `UPDATE api_tokens SET token_hash=$3,token_fingerprint=$4,rotated_at=$5,updated_at=$6 WHERE organization_id=$1 AND id=$2 AND token_hash=$7 AND token_fingerprint=$8 AND rotated_at=$9 AND updated_at=$9`, organization, principal, credential.Hash, credential.Fingerprint, credential.RotatedAt, credential.UpdatedAt, rotatedHash, tokenFingerprint(rotatedHash), at)
		}, 401},
		{"expired", func(ctx context.Context) error {
			return cas(ctx, `UPDATE api_tokens SET expires_at=$3 WHERE organization_id=$1 AND id=$2 AND expires_at IS NOT DISTINCT FROM $4`, organization, principal, at, credential.ExpiresAt)
		}, func(ctx context.Context) error {
			return cas(ctx, `UPDATE api_tokens SET expires_at=$3 WHERE organization_id=$1 AND id=$2 AND expires_at=$4`, organization, principal, credential.ExpiresAt, at)
		}, 401},
		{"role changed", func(ctx context.Context) error {
			return cas(ctx, `UPDATE api_tokens SET scope='admin' WHERE organization_id=$1 AND id=$2 AND scope=$3`, organization, principal, credential.Scope)
		}, func(ctx context.Context) error {
			return cas(ctx, `UPDATE api_tokens SET scope=$3 WHERE organization_id=$1 AND id=$2 AND scope='admin'`, organization, principal, credential.Scope)
		}, 404},
		{"project grant", func(ctx context.Context) error {
			return write(ctx, func(w tenantstore.Writer) error { return w.DeleteAccessTokenGrant(ctx, principal, project) })
		}, func(ctx context.Context) error {
			return write(ctx, func(w tenantstore.Writer) error { return w.CreateAccessTokenGrant(ctx, grant) })
		}, 404},
		{"policy principal", policyMutation(variants.principal, variants.original.ReviewPolicyID), policyMutation(variants.original, variants.principal.ReviewPolicyID), 404},
		{"policy source", policyMutation(variants.source, variants.original.ReviewPolicyID), policyMutation(variants.original, variants.source.ReviewPolicyID), 404},
		{"repository configuration identity", policyMutation(variants.configuration, variants.original.ReviewPolicyID), policyMutation(variants.original, variants.configuration.ReviewPolicyID), http.StatusOK},
	} {
		for _, during := range []bool{false, true} {
			name := test.name + "/admission"
			if during {
				name = test.name + "/transaction"
			}
			if replay {
				name += "/replay"
			}
			t.Run(name, func(t *testing.T) {
				before := hostedIndependentStoredChecks(t, f, path)
				changed := false
				restore := func() {
					if !changed {
						return
					}
					cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), f.service.config.ShutdownTimeout)
					defer cancel()
					changed = false
					if err := test.restore(cleanup); err != nil {
						t.Error(err)
						return
					}
					if err := f.service.config.TenantRepository.ReadTenant(cleanup, organization, corestore.Serving, func(r tenantstore.Reader) error {
						actual, err := r.AccessCredential(cleanup, principal)
						if err != nil {
							return err
						}
						actual.LastUsedAt = credential.LastUsedAt
						if !reflect.DeepEqual(actual, credential) {
							return errors.New("credential fixture identity or fields were not restored")
						}
						actualGrant, err := r.AccessTokenGrant(cleanup, principal, project)
						if err != nil {
							return err
						}
						if actualGrant != grant {
							return errors.New("credential fixture grant was not restored")
						}
						actualPolicy, err := r.ReviewPolicy(cleanup, project)
						if err == nil && actualPolicy != variants.original {
							return errors.New("review policy fixture reference was not restored")
						}
						return err
					}); err != nil {
						t.Error(err)
					}
				}
				t.Cleanup(restore)
				mutate := func() {
					if err := test.mutate(t.Context()); err != nil {
						t.Fatal(err)
					}
					changed = true
				}
				raw, err := json.Marshal(changeTestResult(version))
				if err != nil {
					t.Fatal(err)
				}
				body := &hostedChangePolicyReader{Reader: strings.NewReader(string(raw))}
				if during {
					body.beforeRead = mutate
				} else {
					mutate()
				}
				request := httptest.NewRequest(http.MethodPost, path, body)
				request.Header.Set("Authorization", "Bearer "+token)
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, request)
				want := test.want
				if during && want != http.StatusOK {
					want = http.StatusNotFound
				}
				requireNativeStatus(t, response, want)
				if body.beforeRead != nil {
					t.Fatal("transaction revocation was not exercised")
				}
				restore()
				if want != http.StatusOK {
					if after := hostedIndependentStoredChecks(t, f, path); !reflect.DeepEqual(after, before) {
						t.Fatal("revoked submission changed accepted checks")
					}
				}
			})
		}
	}
}

type hostedCheckPolicyVariants struct {
	original, principal, source, configuration eventstore.ReviewPolicy
}

func prepareHostedCheckPolicyVariants(t *testing.T, f *browserHostedFixture, project tracker.ProjectID) hostedCheckPolicyVariants {
	t.Helper()
	ctx := t.Context()
	organization := f.service.accessOrganization()
	var variants hostedCheckPolicyVariants
	var original policystore.Revision
	if err := f.service.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		variants.original, err = r.ReviewPolicy(ctx, project)
		if err != nil {
			return err
		}
		original, err = r.ProjectPolicy(ctx, project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	content := eventstore.NewContent(f.service.runDetailStore())
	rules, err := content.ReviewPolicy(ctx, variants.original)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.ProjectPolicy(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	descriptor.ConfigDigest = policy.Digest([]byte(original.PolicyID + "/compatible-configuration"))
	descriptor = descriptor.WithID()
	compatible, err := content.PrepareProjectPolicy(ctx, organization, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	compatible.ProjectID, compatible.ApprovedBy, compatible.ApprovedAt = project, original.ApprovedBy, original.ApprovedAt
	if err := f.service.config.TenantRepository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.ApproveProjectPolicy(ctx, compatible, original.PolicyID); err != nil {
			return err
		}
		return w.ApproveProjectPolicy(ctx, original, compatible.PolicyID)
	}); err != nil {
		t.Fatal(err)
	}
	for i, test := range []struct {
		name string
		row  *eventstore.ReviewPolicy
		edit func(*tracker.ChangeReviewPolicy)
	}{
		{"principal", &variants.principal, func(r *tracker.ChangeReviewPolicy) { r.RequiredChecks[0].PrincipalID = "other" }},
		{"source", &variants.source, func(r *tracker.ChangeReviewPolicy) { r.RequiredChecks[0].Source = "customer" }},
		{"configuration", &variants.configuration, func(r *tracker.ChangeReviewPolicy) { r.PolicyID = compatible.PolicyID }},
	} {
		changed := rules
		changed.ID = policy.Digest([]byte(rules.ID + "/" + test.name))
		changed.RequiredChecks = append([]tracker.ChangeCheckSpec(nil), rules.RequiredChecks...)
		test.edit(&changed)
		row := variants.original
		row.PolicyRevisionID, row.Revision = changed.PolicyID, row.Revision+int64(i+1)
		*test.row, err = content.PrepareReviewPolicy(ctx, row, changed)
		if err != nil {
			t.Fatal(err)
		}
	}
	return variants
}

func hostedIndependentStoredChecks(t *testing.T, f *browserHostedFixture, path string) []tracker.ChangeCheck {
	t.Helper()
	response := f.setupRequest(t, "owner", http.MethodGet, strings.Split(path, "/versions/")[0], nil)
	requireNativeStatus(t, response, http.StatusOK)
	var result tracker.ChangeDetail
	decodeHubResponse(t, response, &result)
	return result.Checks
}

func TestHostedRunnerChangeChecks(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		source string
		want   int
	}{
		{"customer", http.StatusOK},
		{"independent", http.StatusUnprocessableEntity},
	} {
		t.Run(test.source, func(t *testing.T) {
			t.Parallel()
			f := newChangeFixture(t, nil)
			runner := prepareRunner(t, f.nativeFixture, runnerauth.Collaborate)
			runner.enroll(t)
			if test.source == "customer" {
				rules := f.rules
				rules.RequiredChecks[0].PrincipalID = runner.identity.RunnerID
				rules.RequiredChecks[0].Source = test.source
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "customer-policy"}, ExpectedID: f.rules.ID, Policy: rules}), http.StatusOK)
			}
			version := f.publish(t, "v1", "")
			f.service.config.Hosted = &HostedConfig{OrganizationID: string(f.project.OrganizationID)}
			path := f.path + "/versions/" + version.ID + "/checks"
			request := changeTestResult(version)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.redemption.Credential, request), test.want)
			if test.source == "customer" {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.redemption.Credential, request), http.StatusOK)
				request.IdempotencyKey, request.Source = "forged-independent", "independent"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.redemption.Credential, request), http.StatusUnprocessableEntity)
			}
		})
	}
}
