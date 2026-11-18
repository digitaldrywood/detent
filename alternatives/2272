package hubfixture

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const StartupOrganization = tracker.OrganizationID("org_startup")

type LeasedRunners struct {
	Organization tracker.OrganizationID
	Admin        *sql.DB
	Bindings     []runnerauth.Binding
	Hub          *Hub
	Server       *httptest.Server
	Policy       policystore.Revision
	Leases       []tracker.NativeLease
	Tokens       []string
	Data         []tracker.NativeRunData
}

func StartupConfig() Config {
	return Config{Hosted: &hostedplans.HostedConfig{
		OrganizationID:       string(StartupOrganization),
		WorkOSOrganizationID: "provider_org_startup",
		PublicURL:            "https://hub.example.test",
		Provider:             NewHostedProvider(),
	}, RunDetails: hubtest.NewDetails()}
}

func OpenStartup(t *testing.T, cfg Config) *Hub {
	t.Helper()
	return OpenStartupOn(t, hubtest.Database(t), cfg)
}

func OpenStartupOn(t *testing.T, admin *sql.DB, cfg Config) *Hub {
	t.Helper()
	if cfg.Hosted == nil {
		hosted := StartupConfig()
		cfg.Hosted = hosted.Hosted
		if cfg.RunDetails == nil {
			cfg.RunDetails = hosted.RunDetails
		}
	}
	cfg = defaults(t, cfg)
	repository := hubtest.RepositoryOn(t, admin, hubtest.WithClock(cfg.Clock))
	organization := tracker.OrganizationID(cfg.Hosted.OrganizationID)
	now := time.Now().UTC()
	deployment, generation := cfg.Hosted.Deployment()
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example", Slug: "example-" + strings.TrimPrefix(string(organization), "org_"), CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: cfg.Hosted.WorkOSOrganizationID, BootstrapSubject: cfg.Hosted.BootstrapSubject, Deployment: deployment, PublicURL: cfg.Hosted.PublicURL, AllocationGeneration: generation}); err != nil {
			return err
		}
		return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: AdminCredentialID, Name: "Example administrator", Hash: apikey.HashToken(string(cfg.InitialAdminToken)), Scope: "admin", CreatedAt: now, UpdatedAt: now, KeyScope: "admin", ProjectAccess: "all", Kind: "service"})
	}); err != nil {
		t.Fatal(err)
	}
	return open(t, &Hub{Admin: admin, Primary: organization, owner: t}, cfg, repository)
}

func NewLeasedRunners(t *testing.T, customize ...func(*policy.Descriptor)) LeasedRunners {
	t.Helper()
	return NewLeasedRunnersWithDetails(t, nil, customize...)
}

func NewLeasedRunnersWithDetails(t *testing.T, decorate func(attachment.DetailStore) attachment.DetailStore, customize ...func(*policy.Descriptor)) LeasedRunners {
	t.Helper()
	cfg := StartupConfig()
	if decorate != nil {
		cfg.RunDetails = decorate(cfg.RunDetails)
	}
	return SeedLeasedRunners(t, OpenStartup(t, cfg), StartupOrganization, customize...)
}

func (f LeasedRunners) Client(t *testing.T, index int) (*api.NativeClient, context.Context) {
	t.Helper()
	client, err := api.New(api.Config{URL: f.Server.URL, TokenSource: func() string { return f.Tokens[index] }, HTTPClient: &http.Client{Transport: &http.Transport{}}})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native(f.Organization, "prj_runner")
	if err != nil {
		t.Fatal(err)
	}
	return native, api.WithMutationAuthority(t.Context(), native.Scope(), f.Leases[index], nil)
}

func SeedLeasedRunners(t *testing.T, h *Hub, organization tracker.OrganizationID, customize ...func(*policy.Descriptor)) LeasedRunners {
	t.Helper()
	var err error
	f := LeasedRunners{Organization: organization, Admin: h.Admin, Hub: h, Server: httptest.NewServer(h.Handler())}
	t.Cleanup(f.Server.Close)
	now := time.Now().UTC().Truncate(time.Microsecond)
	expires := now.Add(time.Hour)
	descriptor := Policy()
	for _, edit := range customize {
		edit(&descriptor)
	}
	policy := descriptor.WithID()
	f.Policy, err = eventstore.NewContent(h.Config.RunDetails).PrepareProjectPolicy(t.Context(), organization, policy)
	if err != nil {
		t.Fatal(err)
	}
	f.Policy.ProjectID = "prj_runner"
	f.Policy.ApprovedAt, f.Policy.ApprovedBy = now, "test_hub_admin"
	err = h.Repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization, ID: "hub_" + strings.Repeat("a", 32), CursorKey: bytes.Repeat([]byte{0x5a}, 32)}); err != nil {
			return err
		}
		if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "prj_runner", Name: "Example", Profile: "native"}, States: States()}); err != nil {
			return err
		}
		if err := w.ApproveProjectPolicy(t.Context(), f.Policy, ""); err != nil {
			return err
		}
		for index := 1; index <= 2; index++ {
			id := strconv.Itoa(index)
			token, err := apikey.GenerateToken()
			if err != nil {
				return err
			}
			binding := runnerauth.NewBinding()
			f.Bindings = append(f.Bindings, binding)
			runnerID, machineID, credentialID := binding.RunnerID, string(binding.MachineID), "credential_"+id
			f.Tokens = append(f.Tokens, token)
			hash := apikey.HashToken(token)
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: credentialID, Name: "Example runner " + id, Hash: hash, Fingerprint: hubapi.TokenFingerprint(hash), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: credentialID, ProjectID: "prj_runner"}); err != nil {
				return err
			}
			operations := []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}
			if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization, ID: "enrollment_" + id, RunnerID: runnerID, MachineID: tracker.MachineID(machineID), TokenHash: apikey.HashToken("example_enrollment_" + id), Scope: "projects", ProjectIDs: []tracker.ProjectID{"prj_runner"}, Operations: operations, CreatedAt: now, ExpiresAt: expires, CreatedBy: "test_hub_admin"}); err != nil {
				return err
			}
			if err := w.RedeemRunnerEnrollment(t.Context(), "enrollment_"+id, runnerID, tracker.MachineID(machineID), now); err != nil {
				return err
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: machineID, Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
			if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: runnerID, MachineID: tracker.MachineID(machineID), TokenID: credentialID, EnrollmentID: "enrollment_" + id, Scope: "projects", State: "active", DisplayName: "Example runner " + id, Operations: operations, CapacityLimit: 1, ReportedCapacity: 1, ProjectIDs: []tracker.ProjectID{"prj_runner"}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
				return err
			}
			state := "Todo"
			item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: "prj_runner", NativeID: tracker.NativeWorkItemID("wi_" + id), Number: int64(index), Title: "Example", WorkflowStateName: &state})
			if err != nil {
				return err
			}
			itemRaw, err := json.Marshal(tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: "prj_runner", WorkItemID: item.NativeID, Number: int(item.Number), Revision: tracker.Revision(item.Revision)}, Title: "Example", State: "Todo"})
			if err != nil {
				return err
			}
			itemVersion, err := eventstore.NewContent(h.Config.RunDetails).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: "prj_runner", WorkItemID: item.NativeID, RecordID: string(item.NativeID), Revision: item.Revision}, itemRaw)
			if err != nil {
				return err
			}
			if _, err := w.CreateCollaborationVersion(t.Context(), itemVersion); err != nil {
				return err
			}
			lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: machineID, SessionID: "execution_" + id, LeaseID: "lease_" + id, TTL: time.Hour})
			if err != nil {
				return err
			}
			if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: organization, LeaseID: lease.LeaseID, RunnerID: runnerID, AssignedAt: now}); err != nil {
				return err
			}
			if err := w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: organization, ProjectID: "prj_runner", LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: policy.ID, BoundAt: now, Selector: &policy.Requirements}); err != nil {
				return err
			}
			f.Leases = append(f.Leases, tracker.NativeLease{ID: tracker.LeaseID(lease.LeaseID), WorkItemID: item.NativeID, MachineID: tracker.MachineID(machineID), SessionID: lease.SessionID, FencingToken: tracker.FencingToken(lease.FencingToken), PolicyID: policy.ID, ExpiresAt: lease.ExpiresAt})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for index, lease := range f.Leases {
		data := tracker.NativeRunData{AttemptID: hubapi.NewID("attempt"), RunID: hubapi.NewID("run"), LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: policy.ID, Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "code", Backend: "example-backend", Model: "example-model"}}
		f.Data = append(f.Data, data)
		event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "start_" + data.AttemptID}, Type: "run.started", SchemaVersion: 1, Data: data}
		path := "/api/v2/organizations/" + string(organization) + "/projects/prj_runner/work-items/" + string(lease.WorkItemID) + "/events"
		RequireStatus(t, h.Request(t, http.MethodPost, path, f.Tokens[index], event), http.StatusOK)
	}
	return f
}
