package hubclient

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/digitaldrywood/detent/internal/runner"
)

// The runner downloads what a control named, with its own token, through the
// hub path the control carries (decisions section 17.1).

// attachmentHub serves the files a test names and refuses everything else,
// the way the hub refuses an attachment the caller may not read.
func attachmentHub(t *testing.T, files map[string][]byte) *NativeClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		content, served := files[r.URL.Path]
		if !served {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/markdown")
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test" }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	return native
}

func attachmentPath(id string) string {
	return "/api/v2/organizations/org_test/projects/prj_test/conversations/cnv_1/attachments/" + id
}

func TestFetchConversationAttachments(t *testing.T) {
	t.Parallel()
	served := map[string][]byte{}
	named := make([]ConversationAttachment, 0, maxConversationAttachmentsPerTurn+2)
	for i := range maxConversationAttachmentsPerTurn + 2 {
		id := "att_" + strconv.Itoa(i)
		body := []byte("file " + strconv.Itoa(i))
		served[attachmentPath(id)] = body
		named = append(named, ConversationAttachment{ID: id, Name: id + ".md", MIME: "text/markdown", Size: int64(len(body)), URL: attachmentPath(id)})
	}
	missing := ConversationAttachment{ID: "att_gone", Name: "gone.md", MIME: "text/markdown", Size: 4, URL: attachmentPath("att_gone")}
	offHub := ConversationAttachment{ID: "att_remote", Name: "remote.md", MIME: "text/markdown", Size: 4, URL: "https://elsewhere.test/steal"}

	cases := []struct {
		name  string
		named []ConversationAttachment
		want  []string
	}{
		{name: "nothing named"},
		{name: "one file", named: named[:1], want: []string{"att_0"}},
		{name: "a file the hub will not serve is left out", named: []ConversationAttachment{named[0], missing, named[1]}, want: []string{"att_0", "att_1"}},
		{name: "an absolute URL is never followed", named: []ConversationAttachment{offHub, named[0]}, want: []string{"att_0"}},
		{
			name:  "more than one turn's worth is truncated",
			named: named,
			want:  []string{"att_0", "att_1", "att_2", "att_3", "att_4", "att_5", "att_6", "att_7", "att_8", "att_9"},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			session := &conversationSession{client: attachmentHub(t, served), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			fetched := session.fetchAttachments(t.Context(), test.named)
			if len(fetched) != len(test.want) {
				t.Fatalf("fetchAttachments() = %d files, want %d", len(fetched), len(test.want))
			}
			for i, id := range test.want {
				if fetched[i].ID != id {
					t.Fatalf("fetchAttachments()[%d] = %q, want %q", i, fetched[i].ID, id)
				}
				if got, want := string(fetched[i].Content), string(served[attachmentPath(id)]); got != want {
					t.Fatalf("fetchAttachments()[%d] content = %q, want %q", i, got, want)
				}
				if fetched[i].MIME != "text/markdown" || fetched[i].Name != id+".md" {
					t.Fatalf("fetchAttachments()[%d] = %+v, want the named file", i, fetched[i])
				}
			}
		})
	}
}

// PendingAttachments hands back a copy, so a caller cannot rewrite what the
// session will hand to the next turn.
func TestPendingAttachmentsCopies(t *testing.T) {
	t.Parallel()
	session := &conversationSession{pendingAttachments: []runner.AgentAttachment{{ID: "att_1", Name: "notes.md"}}}
	pending := session.PendingAttachments()
	if len(pending) != 1 || pending[0].ID != "att_1" {
		t.Fatalf("PendingAttachments() = %+v, want the one attachment", pending)
	}
	pending[0].Name = "rewritten.md"
	if session.PendingAttachments()[0].Name != "notes.md" {
		t.Fatal("PendingAttachments() shares its backing array with the session")
	}
}
