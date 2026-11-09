package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProjectIntegrationIdleIgnoresCompletedChat(t *testing.T) {
	admin := template.Database(t)
	seedCore(t, admin)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			now := time.Now().UTC()
			owner := "owner_" + string(org)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: owner, Name: "Example owner", Hash: apikey.HashToken(owner), Scope: "admin", Kind: "service", KeyScope: "admin", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				_, err := w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: org, ProjectID: "prj_shared", ID: "conv_0123456789abcdef0123456789abcdef", CreationKey: "create_" + string(org), CreationHash: "hash_" + string(org), OwnerPrincipalID: owner, Origin: "user", Title: "Example chat", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1, Execution: conversation.Execution{Status: conversation.ExecutionIdle, Settled: true}, Preferences: conversation.DefaultPreferences(), CreatedAt: now, UpdatedAt: now})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			check := func(name string, active bool) {
				t.Helper()
				t.Run(name, func(t *testing.T) {
					err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						return w.RequireProjectIntegrationIdle(t.Context(), "prj_shared", now)
					})
					if active && !errors.Is(err, projectstore.ErrIntegrationActiveWork) || !active && err != nil {
						t.Fatalf("integration guard=%v, active=%v", err, active)
					}
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						return w.RequireProjectIntegrationIdle(t.Context(), "prj_other", now)
					}); err != nil {
						t.Fatalf("other project refused: %v", err)
					}
				})
			}
			check("completed chat", false)
			err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.RequireProjectIdle(t.Context(), "prj_shared", now) })
			if !errors.Is(err, projectstore.ErrActiveWork) {
				t.Fatalf("deletion guard relaxed: %v", err)
			}
			var lease leasestore.Lease
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				machine := leasestore.Machine{OrganizationID: org, ID: "machine_" + string(org), Hostname: "example-host", Capacity: 1, Version: "test"}
				if err := w.RegisterMachine(t.Context(), machine); err != nil {
					return err
				}
				if err := w.HeartbeatMachine(t.Context(), machine, now); err != nil {
					return err
				}
				var err error
				lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: 41, ExpectedRevision: 1, MachineID: machine.ID, SessionID: "session_" + string(org), LeaseID: "lease_" + string(org), TTL: time.Hour})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			check("live lease", true)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "finished")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			check("released lease", false)
			payload, err := eventstore.NewContent(newHistoryObjects()).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org}, []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.(projectstore.GitHubOutboxWriter).EnqueueGitHubOutbound(t.Context(), projectstore.GitHubOutbound{ProjectID: "prj_shared", WorkItemID: "wi_shared", Key: "write_" + string(org), Repository: "example-org/example-project", Kind: "workpad", IssueNumber: 1, CreatedAt: now, Payload: payload})
			}); err != nil {
				t.Fatal(err)
			}
			check("pending outbound", false)

			if !t.Run("claim skips integration transaction", func(t *testing.T) {
				claimContext, cancelClaim := context.WithCancel(t.Context())
				release := make(chan struct{})
				releaseGuard := sync.OnceFunc(func() { close(release) })
				ready := make(chan error, 1)
				guardDone := make(chan error, 1)
				claimDone := make(chan error, 1)
				guardJoined, claimStarted, claimJoined := false, false, false
				t.Cleanup(func() {
					cancelClaim()
					releaseGuard()
					if claimStarted && !claimJoined {
						<-claimDone
					}
					if !guardJoined {
						<-guardDone
					}
				})
				go func() {
					guardDone <- repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						err := w.RequireProjectIntegrationIdle(t.Context(), "prj_shared", now)
						ready <- err
						if err != nil {
							return err
						}
						<-release
						return nil
					})
				}()
				if err := <-ready; err != nil {
					t.Fatal(err)
				}
				claimStarted = true
				go func() {
					claimDone <- repository.WriteTenant(claimContext, org, corestore.Background, func(w tenantstore.Writer) error {
						_, err := w.(projectstore.GitHubOutboxWriter).ClaimGitHubOutbound(claimContext, now, now.Add(time.Hour), "during_settings_"+string(org))
						return err
					})
				}()
				var claimErr error
				testenv.WaitUntil(t, t.Context(), func() error {
					select {
					case claimErr = <-claimDone:
						claimJoined = true
						return nil
					default:
					}
					var blocked bool
					if err := admin.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND backend_type='client backend' AND wait_event_type='Lock')").Scan(&blocked); err != nil {
						t.Fatal(err)
					}
					if blocked {
						cancelClaim()
						claimErr = <-claimDone
						claimJoined = true
						return nil
					}
					return errors.New("outbound claim is executing")
				})
				releaseGuard()
				guardErr := <-guardDone
				guardJoined = true
				if guardErr != nil {
					t.Fatal(guardErr)
				}
				if !errors.Is(claimErr, corestore.ErrNotFound) {
					t.Fatalf("claim crossed integration transaction: %v", claimErr)
				}
			}) {
				return
			}
			var outbound projectstore.GitHubOutbound
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				var err error
				outbound, err = w.(projectstore.GitHubOutboxWriter).ClaimGitHubOutbound(t.Context(), now, now.Add(time.Hour), "outbound_"+string(org))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			check("processing outbound", true)
			outbound.Status, outbound.Outcome = "completed", "committed"
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.(projectstore.GitHubOutboxWriter).FinishGitHubOutbound(t.Context(), outbound, now)
			}); err != nil {
				t.Fatal(err)
			}
			check("completed outbound", false)
		})
	}
}
