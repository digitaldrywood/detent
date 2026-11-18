package runnerenroll

import (
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerEnrollmentSingleRedemption(t *testing.T) {
	t.Parallel()
	for _, f := range enrollmentFixtures(t) {
		r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
		start := make(chan struct{})
		statuses := make(chan int, 8)
		var workers sync.WaitGroup
		for range 8 {
			workers.Go(func() {
				<-start
				_, err := f.service.Redeem(t.Context(), f.project.OrganizationID, r.enrollment.Token, r.redemption)
				statuses <- status(err, http.StatusCreated)
			})
		}
		close(start)
		workers.Wait()
		close(statuses)
		winners := 0
		for status := range statuses {
			switch status {
			case http.StatusCreated:
				winners++
			case http.StatusUnauthorized:
			default:
				t.Fatalf("redemption status = %d", status)
			}
		}
		if winners != 1 {
			t.Fatalf("redemption winners = %d", winners)
		}
		_, err := f.service.Redeem(t.Context(), f.project.OrganizationID, r.enrollment.Token, r.redemption)
		requireStatus(t, err, http.StatusCreated, http.StatusUnauthorized)
		if err := f.repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
			after := ""
			for {
				credentials, err := reader.AccessCredentials(t.Context(), after, corestore.MaxPageSize)
				if err != nil {
					return err
				}
				for _, credential := range credentials {
					if credential.Hash == r.enrollment.Token || credential.Hash == r.redemption.Credential {
						t.Fatal("plaintext credential persisted")
					}
				}
				if len(credentials) < corestore.MaxPageSize {
					return nil
				}
				next := credentials[len(credentials)-1].ID
				if next <= after {
					return errors.New("credential page did not advance")
				}
				after = next
			}
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunnerUnboundEnrollment(t *testing.T) {
	t.Parallel()
	for _, f := range enrollmentFixtures(t) {
		create := func(t *testing.T, request runnerauth.EnrollmentRequest) runnerauth.Enrollment {
			t.Helper()
			value, err := f.service.Create(t.Context(), f.scope, request)
			requireStatus(t, err, http.StatusCreated, http.StatusCreated)
			return value.(runnerauth.Enrollment)
		}
		redemption := func(t *testing.T, binding runnerauth.Binding) runnerauth.Redemption {
			t.Helper()
			credential, err := apikey.GenerateToken()
			if err != nil {
				t.Fatal(err)
			}
			return runnerauth.Redemption{Binding: binding, Credential: credential, Hostname: "customer-host", DisplayName: "Runner", Capacity: 2, Version: "test"}
		}
		unbound := runnerauth.EnrollmentRequest{ProjectIDs: []tracker.ProjectID{f.project.ID}, Operations: []string{runnerauth.Read, runnerauth.Heartbeat}, TTLSeconds: 60}

		for _, test := range []struct {
			sprite string
			status int
		}{
			{"customer-host", http.StatusCreated},
			{"other-host", http.StatusUnprocessableEntity},
			{"Customer-host", http.StatusUnprocessableEntity},
		} {
			t.Run("Sprite identity "+test.sprite, func(t *testing.T) {
				enrollment := create(t, unbound)
				body := redemption(t, runnerauth.NewBinding())
				body.SpriteName = test.sprite
				_, err := f.service.Redeem(t.Context(), f.project.OrganizationID, enrollment.Token, body)
				requireStatus(t, err, http.StatusCreated, test.status)
				if test.status == http.StatusCreated {
					if err := f.repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
						machine, err := reader.Machine(t.Context(), string(body.MachineID))
						if err == nil && machine.SpriteName != test.sprite {
							t.Fatalf("persisted Sprite = %q, want %q", machine.SpriteName, test.sprite)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			})
		}

		for _, test := range []struct {
			name    string
			request runnerauth.EnrollmentRequest
			status  int
		}{
			{"unbound", unbound, http.StatusCreated},
			{"runner without machine", func() runnerauth.EnrollmentRequest {
				request := unbound
				request.RunnerID = runnerauth.NewBinding().RunnerID
				return request
			}(), http.StatusUnprocessableEntity},
			{"shared without a host", func() runnerauth.EnrollmentRequest {
				request := unbound
				request.SharedMachine = true
				return request
			}(), http.StatusUnprocessableEntity},
		} {
			t.Run(test.name, func(t *testing.T) {
				_, err := f.service.Create(t.Context(), f.scope, test.request)
				requireStatus(t, err, http.StatusCreated, test.status)
			})
		}

		enrollment := create(t, unbound)
		first := runnerauth.NewBinding()
		statuses := make(chan int, 4)
		var workers sync.WaitGroup
		for i := range 4 {
			binding := first
			if i > 0 {
				binding = runnerauth.NewBinding()
			}
			body := redemption(t, binding)
			workers.Go(func() {
				_, err := f.service.Redeem(t.Context(), f.project.OrganizationID, enrollment.Token, body)
				statuses <- status(err, http.StatusCreated)
			})
		}
		workers.Wait()
		close(statuses)
		winners := 0
		for status := range statuses {
			if status == http.StatusCreated {
				winners++
			} else if status != http.StatusUnauthorized {
				t.Fatalf("redemption status = %d", status)
			}
		}
		if winners != 1 {
			t.Fatalf("unbound redemption winners = %d, want 1", winners)
		}
		var runner, machine string
		if err := f.repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
			stored, err := reader.RunnerEnrollment(t.Context(), enrollment.ID)
			if err != nil {
				return err
			}
			runner, machine = stored.RunnerID, string(stored.MachineID)
			if runner == "" || machine == "" || stored.RedeemedAt == nil {
				t.Fatalf("redeemed enrollment binding = %q %q", runner, machine)
			}
			identity, err := reader.RunnerIdentity(t.Context(), runner)
			if err == nil && (identity.RunnerID != runner || string(identity.MachineID) != machine || identity.EnrollmentID != enrollment.ID) {
				t.Fatalf("runner identity binding = %+v", identity)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}

		taken := create(t, unbound)
		_, err := f.service.Redeem(t.Context(), f.project.OrganizationID, taken.Token, redemption(t, runnerauth.Binding{RunnerID: runner, MachineID: tracker.MachineID(machine)}))
		requireStatus(t, err, http.StatusCreated, http.StatusConflict)
	}
}

func TestSharedEnrollmentRequiresExplicitHostApproval(t *testing.T) {
	t.Parallel()
	for _, f := range enrollmentFixtures(t) {
		r := prepareRunner(t, f, runnerauth.Read)
		r.enroll(t)
		for _, test := range []struct {
			name         string
			shared       bool
			machine      tracker.MachineID
			organization string
			want         int
		}{
			{"implicit collision", false, r.binding.MachineID, string(f.project.OrganizationID), http.StatusConflict},
			{"unknown shared host", true, runnerauth.NewBinding().MachineID, string(f.project.OrganizationID), http.StatusConflict},
			{"approved shared host", true, r.binding.MachineID, string(f.project.OrganizationID), http.StatusCreated},
		} {
			t.Run(test.name, func(t *testing.T) {
				binding := runnerauth.NewBinding()
				binding.MachineID = test.machine
				request := runnerauth.EnrollmentRequest{Binding: binding, SharedMachine: test.shared, ProjectIDs: []tracker.ProjectID{f.project.ID}, Operations: []string{runnerauth.Read}, TTLSeconds: 60}
				_, err := f.service.Create(t.Context(), f.scope, request)
				requireStatus(t, err, http.StatusCreated, test.want)
			})
		}
	}
}

func TestRunnerEnrollmentIsolationSelection(t *testing.T) {
	for _, test := range []struct {
		name, selected, want string
		status               int
	}{
		{"legacy", "", "sandbox", http.StatusCreated},
		{"sandbox", "sandbox", "sandbox", http.StatusCreated},
		{"full access", "native-trusted", "native-trusted", http.StatusCreated},
		{"invalid", "root", "", http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, f := range enrollmentFixtures(t) {
				r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
				r.redemption.IsolationTier = test.selected
				_, err := f.service.Redeem(t.Context(), f.project.OrganizationID, r.enrollment.Token, r.redemption)
				requireStatus(t, err, http.StatusCreated, test.status)
				if test.want == "" {
					continue
				}
				var runner runnerstore.Identity
				if err := f.repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					var err error
					runner, err = reader.RunnerIdentity(t.Context(), r.binding.RunnerID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if runner.IsolationTier != test.want {
					t.Fatalf("tier=%q, want %q", runner.IsolationTier, test.want)
				}
			}
		})
	}
}
