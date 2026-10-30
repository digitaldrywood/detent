package hubhealth

import (
	"html"
	"strings"
	"testing"
	"time"
)

func TestHealthEmailTemplate(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ kind, path, state string }{
		{"work_item", "/local-organization/native/issues/1", "opened"},
		{"runner", "/fleet", "opened"},
		{"project", "/fleet", "opened"},
		{"org", "/fleet", "opened"},
		{"runner", "/fleet", "resolved"},
	} {
		t.Run(test.kind+" "+test.state, func(t *testing.T) {
			opened := time.Date(2026, 10, 6, 20, 0, 0, 0, time.FixedZone("fixture", 2*60*60))
			f := NewFinding("signal <script> & health", "instance", test.kind, "wi_example", "Summary <script> & details.\nprivate raw log sentinel", "The owner checks <runner> & acts.", nil, Evidence{EventIDs: []string{"private-event", "private-event-two"}, AttemptIDs: []string{"private-attempt"}, Counts: map[string]int{"leases <active> & ready": 3}})
			f.OpenedAt = opened
			subject := test.kind + " " + "wi_example"
			if test.kind == "runner" {
				f.Subject.ID += " <script> & subject"
				subject += " <script> & subject"
			}
			if test.state == "resolved" {
				resolved := opened.Add(time.Hour)
				f.ResolvedAt = &resolved
			}
			link := "https://cloud.example.test" + test.path
			message, err := RenderEmail(f, link)
			if err != nil {
				t.Fatal(err)
			}
			if message.Subject != "[Detent] signal <script> & health: "+subject {
				t.Fatalf("subject changed: %q", message.Subject)
			}
			for _, part := range []struct{ name, body string }{{"text", message.Body}, {"HTML", html.UnescapeString(message.HTMLBody)}} {
				for _, want := range []string{"Finding " + test.state, "signal <script> & health", subject, "Summary <script> & details.", "The owner checks <runner> & acts.", "Oct 6, 2026 18:00 UTC", "Events", "Attempts", "leases <active> & ready", link} {
					if !strings.Contains(part.body, want) {
						t.Errorf("%s missing %q: %s", part.name, want, part.body)
					}
				}
				if strings.Contains(part.body, "Oct 6, 2026 19:00 UTC") != (test.state == "resolved") {
					t.Errorf("%s has wrong resolved time: %s", part.name, part.body)
				}
			}
			for _, want := range []string{"Events: 2", "Attempts: 1", "leases <active> & ready: 3"} {
				if !strings.Contains(message.Body, want) {
					t.Errorf("text missing %q", want)
				}
			}
			for _, want := range []string{"&lt;script&gt; &amp; health", "Summary &lt;script&gt; &amp; details.", "&lt;runner&gt; &amp; acts.", "leases &lt;active&gt; &amp; ready", `href="` + link + `"`, ">1</td>", ">2</td>", ">3</td>", "Open " + strings.ReplaceAll(test.kind, "_", " ")} {
				if !strings.Contains(message.HTMLBody, want) {
					t.Errorf("HTML missing %q: %s", want, message.HTMLBody)
				}
			}
			if strings.Contains(message.HTMLBody, "<script>") || strings.Contains(message.HTMLBody, "<runner>") || len(message.InlineImages) != 1 {
				t.Fatal("HTML has unescaped content or lacks the shared mark")
			}
			color := "#fe9a00"
			if test.state == "resolved" {
				color = "#00bc7d"
			}
			if !strings.Contains(message.HTMLBody, "background-color:"+color) {
				t.Error("missing state indicator")
			}
			for _, private := range []string{"private raw log sentinel", "private-event", "private-attempt"} {
				if strings.Contains(message.Body+message.HTMLBody, private) {
					t.Fatalf("private evidence leaked: %s", private)
				}
			}
			for _, credential := range []string{
				"password=hunter2", "api_key=private-key", "token=private-token", "secret='private value'", "Authorization: Bearer abc.def", "ghp_abcdefghijklmnopqrstuvwxyz123456789", "github_pat_private", "sk-proj-private", "AKIA1234567890123456", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.private", "https://user:password@example.test/path?token=private", "0123456789abcdefghijklmnopqrstuvwxyz",
			} {
				f.Summary, f.NextAction, f.Signal = credential, credential, credential
				f.Subject.ID = credential
				f.Subject.Kind = credential
				f.Evidence.Counts = map[string]int{credential: 1}
				message, err := RenderEmail(f, link)
				if err != nil {
					t.Fatal(err)
				}
				for _, part := range []string{message.Body, html.UnescapeString(message.HTMLBody), message.Subject} {
					if strings.Contains(part, credential) || !strings.Contains(part, "[redacted]") {
						t.Fatalf("credential not redacted: %q", credential)
					}
				}
			}
		})
	}
}
