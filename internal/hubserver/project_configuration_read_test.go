package hubserver

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPreparedConfigurationPreservesOtherProjects(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	r := prepareRunnerForScope(t, f, "organization", runnerauth.Read, runnerauth.Heartbeat)
	r.enroll(t)
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-configuration")
	currentPolicy := hubTestPolicy()
	first := runnerauth.ProjectConfiguration{ProjectID: string(f.project.ID), EffectivePolicy: &currentPolicy, ObservedAt: f.service.config.now()}
	second := runnerauth.ProjectConfiguration{ProjectID: string(other.project.ID), EffectivePolicy: &currentPolicy, ObservedAt: first.ObservedAt}
	seedHubConfiguration(t, f, r.binding.RunnerID, first)
	seedHubConfiguration(t, other, r.binding.RunnerID, second)
	first.Paused = true
	first.Authority = "hub_configuration"
	var err error
	first.ConfigRevision, err = projectConfigurationDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := f.service.prepareConfigurationPayload(t.Context(), f.project.OrganizationID, first)
	if err != nil {
		t.Fatal(err)
	}
	second.Draining = true
	seedHubConfiguration(t, other, r.binding.RunnerID, second)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		identity, err := w.RunnerIdentity(t.Context(), r.binding.RunnerID)
		if err != nil {
			return err
		}
		owner := w.(postgres.ConfigurationWriter)
		header, err := owner.ConfigurationHeader(t.Context(), r.binding.RunnerID)
		if err != nil {
			return err
		}
		now := f.service.config.now()
		_, err = owner.ApplyConfiguration(t.Context(), postgres.ConfigurationChange{OrganizationID: f.project.OrganizationID, Authority: postgres.ConfigurationAuthority{RunnerID: r.binding.RunnerID, EnrollmentID: identity.EnrollmentID, Revision: identity.Revision, At: now}, ExpectedRevision: header.Revision, ExpectedSourceRevision: header.SourceRevision, SourceRevision: first.ConfigRevision, Projects: []postgres.ConfigurationProject{configurationProjectRow(f.project.OrganizationID, r.binding.RunnerID, first, prepared, now)}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []runnerauth.ProjectConfiguration{first, second} {
		var row postgres.ConfigurationProject
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
			var err error
			row, err = reader.(postgres.ConfigurationReader).ConfigurationProject(t.Context(), r.binding.RunnerID, tracker.ProjectID(want.ProjectID))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var got runnerauth.ProjectConfiguration
		if err := f.service.readConfigurationPayload(t.Context(), f.project.OrganizationID, eventstore.Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes), &got); err != nil {
			t.Fatal(err)
		}
		if got.ProjectID != want.ProjectID || got.Paused != want.Paused || got.Draining != want.Draining || !got.ObservedAt.Equal(want.ObservedAt) {
			t.Fatalf("concurrent project configuration lost: got=%+v want=%+v", got, want)
		}
	}
}
