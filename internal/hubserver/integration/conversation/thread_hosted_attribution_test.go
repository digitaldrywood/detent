package conversation_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func TestTeamThreadHostedAgentAttribution(t *testing.T) {
	t.Parallel()
	hub := hubfixture.NewHostedSecurity(t)
	user := hub.User(t, "thread-member", "member", "alex@example.test", "write", "")
	var member accessstore.Member
	if err := hub.Repository.ReadTenant(t.Context(), hub.Primary, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		member, err = reader.AccessMember(t.Context(), user.Identity.Subject)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	key := hubtest.Credential(t, hub.Repository, hub.Primary, hubtest.CredentialOptions{Member: &hubtest.SeededMember{Member: member}, AllProject: true})
	protocol := hubfixture.MCP(t, hub.Handler(), hubfixture.MCPPath(hub.Primary), key.Token)
	list := func() map[string]string {
		t.Helper()
		reply := protocol("tools/list", "", nil)
		var catalog struct {
			Tools []struct {
				Name, Description string
			} `json:"tools"`
		}
		if err := json.Unmarshal(reply.Result, &catalog); err != nil || len(reply.Error) != 0 {
			t.Fatalf("thread catalog: %v %s", err, reply.Error)
		}
		tools := map[string]string{}
		for _, tool := range catalog.Tools {
			if operatortool.IsThreadTool(tool.Name) {
				tools[tool.Name] = tool.Description
			}
		}
		return tools
	}
	tools := list()
	for _, name := range []string{"list_threads", "get_thread", "create_thread", "post_to_thread", "resolve_thread"} {
		if !strings.Contains(tools[name], "informational") {
			t.Fatalf("catalog lacks informational %s: %q", name, tools[name])
		}
	}
	call := func(name string, args operatortool.ThreadArguments, output any) {
		t.Helper()
		data := hubfixture.MCPData(t, protocol("tools/call", name, args), false)
		var result struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(result.Data, output); err != nil {
			t.Fatal(err)
		}
	}
	var thread threadstore.Thread
	call("create_thread", operatortool.ThreadArguments{Scope: "project", ProjectID: string(hub.Project), Title: "Shared findings"}, &thread)
	var post threadstore.Post
	args := operatortool.ThreadArguments{ThreadID: thread.ID, Kind: "finding", Body: "Informational finding"}
	call("post_to_thread", args, &post)
	attributed := func(post threadstore.Post) bool {
		return post.ActingFor == member.PrincipalID && post.Author == member.PrincipalID && post.Client == "fleet-test" && post.Name == user.Identity.Email && strings.Contains(post.Body, args.Body)
	}
	if !attributed(post) {
		t.Fatalf("thread finding has incorrect attribution: %+v", post)
	}
	var read threadstore.Read
	call("get_thread", operatortool.ThreadArguments{ThreadID: thread.ID, Since: "last_read"}, &read)
	if !slices.ContainsFunc(read.Posts, attributed) {
		t.Fatalf("stored finding lost its attribution: %+v", read.Posts)
	}
	if _, err := hub.Admin.ExecContext(t.Context(), "UPDATE api_tokens SET operator_key_scope='read' WHERE organization_id=$1 AND id=$2", hub.Primary, key.ID); err != nil {
		t.Fatal(err)
	}
	args.Body = "Write after downgrade must be denied"
	hubfixture.MCPData(t, protocol("tools/call", "post_to_thread", args), true)
	tools = list()
	for _, name := range []string{"create_thread", "post_to_thread", "resolve_thread"} {
		if _, ok := tools[name]; ok {
			t.Fatalf("a downgraded key still advertises %s", name)
		}
	}
}
