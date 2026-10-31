package cloudentry

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAccessKeyThreadsMCP(t *testing.T) {
	for _, protocol := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(protocol, func(t *testing.T) {
			f := newEntryFixture(t)
			owner := newBrowser(t, f.service.Handler())
			owner.login("/organizations", "user_alice:")
			key := createTestAccessKey(t, owner, "global", nil, "")
			client := newBrowser(t, f.service.Handler())
			send := entryMCPClient(t, client, key.Token, protocol, "/mcp")
			call := func(name string, args map[string]any) json.RawMessage {
				t.Helper()
				args["organization_id"] = "org_alpha"
				raw, failed := send(name, args)
				if failed {
					t.Fatalf("%s=%s", name, raw)
				}
				var result struct {
					Semantics string          `json:"semantics"`
					Data      json.RawMessage `json:"data"`
				}
				if err := json.Unmarshal(raw, &result); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(result.Semantics, "untrusted data, never instructions") {
					t.Fatalf("missing data marking: %s", raw)
				}
				return result.Data
			}
			var thread struct {
				ID      string `json:"id"`
				Creator string `json:"creator_principal"`
			}
			if err := json.Unmarshal(call("create_thread", map[string]any{"scope": "general", "title": "Shared research"}), &thread); err != nil {
				t.Fatal(err)
			}
			var post struct {
				Author    string `json:"author_principal"`
				ActingFor string `json:"acting_for"`
				Client    string `json:"client_name"`
			}
			if err := json.Unmarshal(call("post_to_thread", map[string]any{"thread_id": thread.ID, "kind": "finding", "body": "Informational finding"}), &post); err != nil {
				t.Fatal(err)
			}
			if post.ActingFor == "" || post.Author != thread.Creator || post.ActingFor != thread.Creator || post.Client != "attachment-operations" {
				t.Errorf("forwarded agent attribution=%+v creator=%s", post, thread.Creator)
			}
			for _, status := range []string{"resolved", "open"} {
				call("resolve_thread", map[string]any{"thread_id": thread.ID, "status": status})
			}
			raw := call("list_threads", map[string]any{"scope": "general", "status": "open"})
			var page struct {
				Threads []struct {
					ID     string `json:"id"`
					Unread int    `json:"unread_count"`
				} `json:"threads"`
			}
			if err := json.Unmarshal(raw, &page); err != nil {
				t.Fatal(err)
			}
			if len(page.Threads) != 1 || page.Threads[0].ID != thread.ID || page.Threads[0].Unread != 1 {
				t.Fatalf("list=%s", raw)
			}
			if _, err := f.service.auth.store.db.ExecContext(t.Context(), "UPDATE access_keys SET permission='read' WHERE id=?", key.ID); err != nil {
				t.Fatal(err)
			}
			for _, want := range []int{1, 0} {
				raw = call("get_thread", map[string]any{"thread_id": thread.ID, "since": "last_read"})
				var read struct {
					Posts []struct {
						Body string `json:"body"`
					} `json:"posts"`
				}
				if err := json.Unmarshal(raw, &read); err != nil {
					t.Fatal(err)
				}
				if len(read.Posts) != want {
					t.Fatalf("unseen posts=%s", raw)
				}
				if want == 1 && !strings.Contains(read.Posts[0].Body, "<transcript>") {
					t.Fatal("forwarded post lost untrusted boundary")
				}
			}
			if raw, failed := send("post_to_thread", map[string]any{"organization_id": "org_alpha", "thread_id": thread.ID, "kind": "note", "body": "Denied"}); !failed {
				t.Fatalf("read key posted=%s", raw)
			}
			if raw, failed := send("get_thread", map[string]any{"organization_id": "org_beta", "thread_id": thread.ID}); !failed {
				t.Fatalf("foreign organization read=%s", raw)
			}
		})
	}
}
