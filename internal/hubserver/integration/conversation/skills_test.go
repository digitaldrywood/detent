package conversation_test

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/skills"
)

func TestConversationProjectSkills(t *testing.T) {
	t.Parallel()
	h := openConversationTestHub(t)
	f := hubfixture.NewNative(t, h, h.Primary, "skill project")
	r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	r.Enroll(t)
	other := hubfixture.NewNative(t, h, f.Project.OrganizationID, "other-skills")
	r2 := hubfixture.PrepareRunner(t, other, runnerauth.Read, runnerauth.Heartbeat)
	r2.Enroll(t)
	skill := skills.ProviderSkill{Name: "review", Description: "Review this project", Path: "/project/.agents/skills/review/SKILL.md", Scope: "project", Enabled: true, UserInvocable: true, Invocation: "$review"}
	for _, fixture := range []struct {
		runner hubfixture.Runner
		base   string
		name   string
	}{{r, f.Base, "review"}, {r2, other.Base, "foreign"}} {
		report := skill
		report.Name, report.Invocation = fixture.name, "$"+fixture.name
		response := h.Request(t, http.MethodPost, fixture.base+"/machines/"+string(fixture.runner.Binding.MachineID)+"/heartbeat", fixture.runner.Redemption.Credential, map[string]any{"display_name": "Example runner", "capacity": 2, "version": "test", "skills": []skills.ProviderSkill{report}})
		requireStatus(t, response, http.StatusOK)
	}
	createdResponse := f.Request(t, http.MethodPost, f.Base+"/conversations", conversations.ConversationCreateRequest{Key: "create", Title: "Project skill menu"})
	requireStatus(t, createdResponse, http.StatusCreated)
	var created conversations.ConversationCreatedResponse
	decodeResponse(t, createdResponse, &created)
	path := f.Base + "/conversations/" + created.Conversation.ID
	for _, test := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"reported", nil, 1},
		{"older runner preserves report", map[string]any{"display_name": "Example runner", "capacity": 2, "version": "test"}, 1},
		{"empty report clears skills", map[string]any{"display_name": "Example runner", "capacity": 2, "version": "test", "skills": []skills.ProviderSkill{}}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.body != nil {
				response := h.Request(t, http.MethodPost, f.Base+"/machines/"+string(r.Binding.MachineID)+"/heartbeat", r.Redemption.Credential, test.body)
				requireStatus(t, response, http.StatusOK)
			}
			response := h.Request(t, http.MethodGet, path, f.Token, nil)
			requireStatus(t, response, http.StatusOK)
			var snapshot conversations.ConversationSnapshot
			decodeResponse(t, response, &snapshot)
			if len(snapshot.Skills) != test.want || test.want == 1 && snapshot.Skills[0] != skill {
				t.Fatalf("project skills = %#v", snapshot.Skills)
			}
		})
	}
	response := h.Request(t, http.MethodPost, f.Base+"/machines/"+string(r.Binding.MachineID)+"/heartbeat", r.Redemption.Credential, map[string]any{"display_name": "Example runner", "capacity": 2, "version": "test", "skills": []skills.ProviderSkill{{Name: "invalid name"}}})
	requireStatus(t, response, http.StatusUnprocessableEntity)
}
