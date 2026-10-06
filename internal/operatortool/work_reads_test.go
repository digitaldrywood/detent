package operatortool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestWorkReadArguments(t *testing.T) {
	// Catch tools/call bypassing schema validation before any application read.
	for _, test := range []struct {
		name, tool, raw string
		valid           bool
	}{
		{"list default", WorkList, `{"project_id":"project"}`, true},
		{"open fingerprint search", WorkList, `{"project_id":"project","fingerprint":"canonical","open":true,"limit":2}`, true},
		{"nonboolean open", WorkList, `{"project_id":"project","open":"true"}`, false},
		{"search", WorkList, `{"project_id":"project","query":"needle","limit":2,"offset":3}`, true},
		{"native filters and projection", WorkList, `{"project_id":"project","archived":"all","states":["Todo","Done"],"labels":["bug"],"assignees":["operator"],"priorities":[0,3],"include":["work","workspace"]}`, true},
		{"native singleton filters", WorkList, `{"project_id":"project","assignee":"operator","priority":0,"archived":"false"}`, true},
		{"invalid archive", WorkList, `{"project_id":"project","archived":"maybe"}`, false},
		{"invalid priority filter", WorkList, `{"project_id":"project","priorities":[4]}`, false},
		{"null archive", WorkList, `{"project_id":"project","archived":null}`, false},
		{"unknown projection", WorkList, `{"project_id":"project","include":["coordinator"]}`, false},
		{"empty filter member", WorkList, `{"project_id":"project","states":[""]}`, false},
		{"oversized filter member", WorkList, `{"project_id":"project","labels":["` + strings.Repeat("x", 257) + `"]}`, false},
		{"too many filters", WorkList, `{"project_id":"project","assignees":["` + strings.Repeat(`x","`, 32) + `x"]}`, false},
		{"too many combined filters", WorkList, `{"project_id":"project","assignee":"owner","assignees":["` + strings.Repeat(`x","`, 31) + `x"]}`, false},
		{"conflicting projections", WorkList, `{"project_id":"project","include":["work","summary"]}`, false},
		{"missing project", WorkList, `{}`, false},
		{"foreign authority argument", WorkList, `{"project_id":"project","organization_id":"other"}`, false},
		{"wrong detail fields", WorkItem, `{"project_id":"project","reference":"item","query":"ignored"}`, false},
		{"detail", WorkItem, `{"project_id":"project","reference":"#1"}`, true},
		{"PR discussion", WorkPRComments, `{"project_id":"project","reference":"#1","offset":1,"limit":1}`, true},
		{"PR repository injection", WorkPRComments, `{"project_id":"project","reference":"#1","repository":"foreign/private"}`, false},
		{"PR number injection", WorkPRComments, `{"project_id":"project","reference":"#1","pull_request":2}`, false},
		{"PR cursor injection", WorkPRComments, `{"project_id":"project","reference":"#1","cursor":"foreign"}`, false},
		{"empty detail", WorkItem, `{"project_id":"project","reference":" "}`, false},
		{"zero limit", WorkList, `{"project_id":"project","limit":0}`, false},
		{"too large limit", WorkList, `{"project_id":"project","limit":201}`, false},
		{"negative offset", WorkList, `{"project_id":"project","offset":-1}`, false},
		{"missing revision", WorkVersion, `{"project_id":"project","reference":"item"}`, false},
		{"null revision", WorkVersion, `{"project_id":"project","reference":"item","revision":null}`, false},
		{"version", WorkVersion, `{"project_id":"project","reference":"item","revision":1}`, true},
		{"native receipt", WorkAttemptReceipt, `{"project_id":"project","reference":"item","native_attempt_id":"attempt_123"}`, true},
		{"local receipt", WorkAttemptReceipt, `{"project_id":"project","reference":"item","attempt_id":168}`, true},
		{"missing attempt", WorkAttemptReceipt, `{"project_id":"project","reference":"item"}`, false},
		{"ambiguous attempt", WorkAttemptReceipt, `{"project_id":"project","reference":"item","attempt_id":168,"native_attempt_id":"attempt_123"}`, false},
		{"ambiguous history", BoardSessionHistory, `{"project_id":"project","reference":"item","attempt_id":168,"native_attempt_id":"attempt_123"}`, false},
		{"invalid native attempt", WorkAttemptReceipt, `{"project_id":"project","reference":"item","native_attempt_id":"other"}`, false},
		{"null local attempt", WorkAttemptReceipt, `{"project_id":"project","reference":"item","attempt_id":null}`, false},
		{"paged native activity", BoardActivity, `{"project_id":"project","reference":"item","cursor":"page","limit":1}`, true},
		{"trailing payload", WorkList, `{"project_id":"project"}{}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeWorkRead(test.tool, json.RawMessage(test.raw))
			if (err == nil) != test.valid {
				t.Fatalf("decode=%v", err)
			}
		})
	}
	if _, err := DecodeWorkRead(WorkList, json.RawMessage(`{"project_id":"`+strings.Repeat("x", 257)+`"}`)); !errors.Is(err, ErrInvalidArguments) {
		t.Fatal(err)
	}
	for _, definition := range WorkReadCatalog() {
		if !definition.Annotations.ReadOnly || definition.Annotations.Destructive || !definition.Annotations.Idempotent || definition.Meta.Toolset != "board" {
			t.Fatalf("unsafe read definition: %#v", definition)
		}
	}
}

type workReadProbe struct {
	calls   int
	failure error
	content json.RawMessage
}

func (p *workReadProbe) ReadWork(context.Context, string, WorkReadRequest) (Result, error) {
	p.calls++
	if p.content != nil {
		return Result{Content: p.content}, p.failure
	}
	return Result{Content: json.RawMessage(`{"items":[]}`)}, p.failure
}

func TestWorkReadDirectAuthorityAndSafeFailure(t *testing.T) {
	for _, test := range []struct {
		name         string
		project, org string
		denied       bool
		failure      error
		oversized    bool
	}{
		{name: "authorized empty", project: "project", org: "org"},
		{name: "foreign project", project: "other", org: "org", denied: true},
		{name: "foreign organization", project: "project", org: "other", denied: true},
		{name: "provider detail opaque", project: "project", org: "org", failure: errors.New("secret provider URL and token")},
		{name: "oversized read", project: "project", org: "org", oversized: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity := Identity{PrincipalID: "principal", OrganizationID: "org", CredentialID: "key"}
			probe := &workReadProbe{failure: test.failure}
			if test.oversized {
				probe.content = json.RawMessage(`{"private":"` + strings.Repeat("secret", MaxResultBytes/6) + `"}`)
			}
			current := identity
			current.OrganizationID = test.org
			ctx := WithConnection(t.Context(), Connection{Identity: identity, Resolve: func(context.Context) (Authority, error) {
				return Authority{Identity: current, Check: func(_ context.Context, r Requirement) error {
					if r.ProjectID != "" && r.ProjectID != "project" {
						return ErrAccessDenied
					}
					return nil
				}, WorkReads: probe}, nil
			}})
			e := NewAuthorizedExecutor(NewExecutor(Dependencies{}))
			_, err := e.Execute(ctx, Call{Name: WorkItem, Arguments: json.RawMessage(`{"project_id":"` + test.project + `","reference":"item"}`)})
			switch {
			case test.denied:
				if !errors.Is(err, ErrAccessDenied) || probe.calls != 0 {
					t.Fatalf("err=%v calls=%d", err, probe.calls)
				}
			case test.failure != nil || test.oversized:
				if !errors.Is(err, ErrReadUnavailable) || strings.Contains(err.Error(), "secret") {
					t.Fatal(err)
				}
				if test.failure != nil && !errors.Is(err, test.failure) {
					t.Fatalf("lost underlying read failure: %v", err)
				}
			default:
				if err != nil || probe.calls != 1 {
					t.Fatalf("err=%v calls=%d", err, probe.calls)
				}
			}
		})
	}
}
