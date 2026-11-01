package hubserver

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/skills"
)

func TestConversationProjectSkills(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	r := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Heartbeat)
	r.enroll(t)
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-skills")
	r2 := prepareRunner(t, other, runnerauth.Read, runnerauth.Heartbeat)
	r2.enroll(t)
	skill := skills.ProviderSkill{Name: "review", Description: "Review this project", Path: "/project/.agents/skills/review/SKILL.md", Scope: "project", Enabled: true, UserInvocable: true, Invocation: "$review"}
	for _, fixture := range []struct {
		runner runnerFixture
		base   string
		name   string
	}{{r, f.base, "review"}, {r2, other.base, "foreign"}} {
		report := skill
		report.Name, report.Invocation = fixture.name, "$"+fixture.name
		response := performHubAPIRequest(t, f.service, http.MethodPost, fixture.base+"/machines/"+string(fixture.runner.binding.MachineID)+"/heartbeat", fixture.runner.redemption.Credential, map[string]any{"capacity": 2, "version": "test", "skills": []skills.ProviderSkill{report}})
		requireNativeStatus(t, response, http.StatusOK)
	}
	created := f.create(t, f.token, map[string]any{"title": "Project skill menu"})
	path := f.base + "/conversations/" + created.Conversation.ID
	for _, test := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"reported", nil, 1},
		{"older runner preserves report", map[string]any{"capacity": 2, "version": "test"}, 1},
		{"empty report clears skills", map[string]any{"capacity": 2, "version": "test", "skills": []skills.ProviderSkill{}}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.body != nil {
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, test.body)
				requireNativeStatus(t, response, http.StatusOK)
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var snapshot conversationSnapshot
			decodeHubResponse(t, response, &snapshot)
			if len(snapshot.Skills) != test.want || test.want == 1 && snapshot.Skills[0] != skill {
				t.Fatalf("project skills = %#v", snapshot.Skills)
			}
		})
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"capacity": 2, "version": "test", "skills": []skills.ProviderSkill{{Name: "invalid name"}}})
	requireNativeStatus(t, response, http.StatusUnprocessableEntity)
}
