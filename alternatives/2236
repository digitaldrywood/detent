package coordinator_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type attentionItem struct {
	WorkItemID string `json:"work_item_id"`
	Reason     string `json:"reason"`
}

type attention struct {
	Blocked   []attentionItem `json:"blocked"`
	Truncated bool            `json:"truncated"`
}

func addBlocker(t *testing.T, h *hubfixture.Hub, p hubfixture.Native, item *tracker.NativeIssue, blocker tracker.NativeWorkItemID) {
	t.Helper()
	request := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "block-" + string(item.WorkItemID) + "-" + string(blocker)}, ExpectedRevision: item.Revision, RelatedWorkItemID: blocker, Operation: "add", RelationshipType: "dependency"}
	response := h.Request(t, http.MethodPost, p.Base+"/work-items/"+string(item.WorkItemID)+"/dependencies", h.AdminToken(), request)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	hubfixture.Decode(t, response, item)
}

func decodeAttention(t *testing.T, raw runner.AgentToolResult, hidden []string) attention {
	t.Helper()
	if !raw.Success {
		t.Fatalf("list_attention failed: %s", raw.Content)
	}
	for _, id := range hidden {
		if strings.Contains(raw.Content, id) {
			t.Fatalf("attention discloses hidden identifier %s: %s", id, raw.Content)
		}
	}
	var got attention
	if err := json.Unmarshal([]byte(raw.Content), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

const listAttention = `{"scope":"project","limit":50}`

func TestListAttentionLeavesNoTraceOfHiddenBlockers(t *testing.T) {
	t.Parallel()
	backend := newToolBackend()
	h := backend.open(t)
	hidden := hubfixture.NewNativeWith(t, h, h.Primary, hubtest.ProjectOptions{ID: "prj_a_hidden", Name: "Example hidden project", RequireDependencies: true})
	visible := hubfixture.NewNativeWith(t, h, h.Primary, hubtest.ProjectOptions{ID: "prj_c_visible", Name: "Example project", RequireDependencies: true})
	blocker := visible.Create(t, "Example visible blocker")
	subject := visible.Create(t, "Example subject")
	addBlocker(t, h, visible, &subject, blocker.WorkItemID)

	chat, first := startConversation(t, backend, h, visible, "list_attention", listAttention)
	without := decodeAttention(t, first, nil)
	if len(without.Blocked) != 1 || without.Blocked[0].WorkItemID != string(subject.WorkItemID) || without.Blocked[0].Reason != "blocked by "+string(blocker.WorkItemID) {
		t.Fatalf("dataset without hidden links lacks its visible blocker: %s", first.Content)
	}

	hiddenIDs := []string{string(hidden.Project.ID)}
	for index := range corestore.MaxPageSize {
		item := hidden.Create(t, "Example hidden blocker number "+strconv.Itoa(index))
		hiddenIDs = append(hiddenIDs, string(item.WorkItemID))
		addBlocker(t, h, visible, &subject, item.WorkItemID)
	}
	second := chat.ask(t, "list_attention", listAttention)
	with := decodeAttention(t, second, hiddenIDs)
	if !reflect.DeepEqual(with.Blocked, without.Blocked) || with.Truncated != without.Truncated {
		t.Fatalf("hidden blockers changed the visible attention list\nwith:    %s\nwithout: %s", second.Content, first.Content)
	}
}
