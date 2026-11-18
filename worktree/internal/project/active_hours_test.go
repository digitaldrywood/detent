package project

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/activehours"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestEffectiveActiveHoursPrecedence(t *testing.T) {
	t.Parallel()
	workflow := activehours.Config{Timezone: "UTC", Windows: []string{"Mon-Sun 01:00-02:00"}}
	globalDefault := activehours.Config{Timezone: "America/Chicago", Windows: []string{"Mon-Sun 03:00-04:00"}}
	projectOverride := activehours.Config{Timezone: "America/New_York", Windows: []string{"Mon-Sun 05:00-06:00"}}
	tests := []struct {
		name    string
		project globalconfig.Project
		want    string
	}{
		{name: "workflow", project: globalconfig.Project{}, want: "UTC"},
		{name: "global default", project: globalconfig.Project{GlobalActiveHours: &globalDefault}, want: "America/Chicago"},
		{name: "project override", project: globalconfig.Project{GlobalActiveHours: &globalDefault, ActiveHours: &projectOverride}, want: "America/New_York"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := EffectiveActiveHours(test.project, workflow).Timezone; got != test.want {
				t.Fatalf("Timezone = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWorkflowConfigAppliesHostActiveHours(t *testing.T) {
	t.Parallel()
	host := activehours.Config{Timezone: "America/Chicago", Windows: []string{"Mon-Sun 22:00-06:00"}}
	workflow := workflowconfig.Config{ActiveHours: activehours.Config{Timezone: "UTC", Windows: []string{"Mon-Sun 09:00-17:00"}}}

	got := workflowConfigWithProjectIdentity(globalconfig.Project{ActiveHours: &host}, workflow)
	if got.ActiveHours.Timezone != host.Timezone || got.ActiveHours.Windows[0] != host.Windows[0] {
		t.Fatalf("ActiveHours = %#v, want %#v", got.ActiveHours, host)
	}
}
