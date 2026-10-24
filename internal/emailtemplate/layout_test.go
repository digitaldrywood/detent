package emailtemplate

import (
	"bytes"
	"image/png"
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, value, escaped string
	}{
		{name: "ordinary", value: "Example", escaped: "Example"},
		{name: "escaped", value: "<script>alert(1)</script> & Example", escaped: "&lt;script&gt;alert(1)&lt;/script&gt; &amp; Example"},
	} {
		t.Run(test.name, func(t *testing.T) {
			message, err := Render(Content{
				Title: "Title " + test.value, Heading: "Heading " + test.value,
				Note:        "Note " + test.value,
				Details:     []Detail{{Label: "Label " + test.value, Value: "Value " + test.value}, {Label: "Time", Value: "Oct 22, 2026 20:52 UTC"}},
				ActionLabel: "Action " + test.value, ActionURL: "https://example.test/open?one=1&two=%3Cscript%3E",
				Footer: "Footer " + test.value,
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"Title", "Heading", "Note", "Label", "Value", "Action", "Footer"} {
				if !strings.Contains(message.HTMLBody, field+" "+test.escaped) {
					t.Fatalf("missing escaped %s: %s", field, message.HTMLBody)
				}
			}
			for _, want := range []string{`href="https://example.test/open?one=1&amp;two=%3Cscript%3E"`, "Oct 22, 2026 20:52 UTC", `src="cid:detent-mark"`} {
				if !strings.Contains(message.HTMLBody, want) {
					t.Fatalf("HTML missing %q: %s", want, message.HTMLBody)
				}
			}
			if strings.Contains(message.HTMLBody, "<script>") {
				t.Fatal("unescaped script in HTML")
			}
			if !strings.Contains(message.Body, "Label "+test.value+": Value "+test.value+"\n") || !strings.Contains(message.Body, "https://example.test/open?one=1&two=%3Cscript%3E\n") || !strings.HasSuffix(message.Body, "Footer "+test.value+"\n") {
				t.Fatalf("plain-text facts or footer changed: %s", message.Body)
			}
			if len(message.InlineImages) != 1 || message.InlineImages[0].ContentID != "detent-mark" || message.InlineImages[0].ContentType != "image/png" {
				t.Fatalf("missing embedded mark: %+v", message.InlineImages)
			}
			if _, err := png.Decode(bytes.NewReader(message.InlineImages[0].Data)); err != nil {
				t.Fatalf("invalid mark PNG: %v", err)
			}
		})
	}
}

func TestRenderActionURL(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"javascript:alert(1)", `https://example.test/?value=<script>&quoted="yes"`} {
		t.Run(target, func(t *testing.T) {
			message, err := Render(Content{ActionURL: target})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(message.HTMLBody, `href="javascript:`) || strings.Contains(message.HTMLBody, "<script>") || strings.Contains(message.HTMLBody, `quoted="yes"`) {
				t.Fatalf("unsafe URL interpolation: %s", message.HTMLBody)
			}
		})
	}
}
