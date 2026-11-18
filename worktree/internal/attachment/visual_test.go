package attachment

import (
	"errors"
	"strings"
	"testing"
)

func TestVisualReferences(t *testing.T) {
	t.Parallel()
	const id = "att_0123456789abcdef0123456789abcdef"
	image := Metadata{ID: id, ProjectID: "prj_test", Name: "image.png", ContentType: "image/png", Size: 10, Width: 1, Height: 1, SHA256: strings.Repeat("a", 64)}
	paired := " ![upload](attachment:" + id + ")"
	for _, test := range []struct {
		name, body string
		reject     bool
	}{
		{"nested image link", "[![preview](https://example.test/render)](https://example.test/doc)", true},
		{"multiple explicit sources", "Screenshots: https://example.test/one https://example.test/two" + paired, true},
		{"paired screenshot followed by documentation", "Screenshot: https://example.test/preview" + paired + ". Documentation: [API guide](https://example.test/docs)", false},
		{"paired screenshot followed by PR", "Screenshot: https://example.test/preview" + paired + " [Pull request](https://github.com/example-org/example-project/pull/1)", false},
		{"documentation ends explicit introduction", "Screenshot: https://example.test/preview. Documentation: [API guide](https://example.test/docs)" + paired, false},
		{"bare documentation ends explicit introduction", "Screenshot: https://example.test/preview. Documentation: https://example.test/docs" + paired, false},
		{"plural sources separated by comma", "Screenshots: https://example.test/one, https://example.test/two" + paired, true},
		{"plural sources separated by conjunction", "Screenshots: [first](https://example.test/one) and [second](https://example.test/two)" + paired, true},
		{"later explicit source still counts", "Screenshot: https://example.test/one" + paired + ". Documentation: [API guide](https://example.test/docs). Wireframe: https://example.test/two", true},
		{"later image source still counts", "Screenshot: https://example.test/one" + paired + ". Documentation: [API guide](https://example.test/docs). https://example.test/screen.png", true},
		{"uppercase scheme", "HTTPS://example.test/image.png", true},
		{"inline code cannot label", "`Screenshot:` https://example.test/redirect", false},
		{"unused reference definition", "[source]: https://example.test/image.png", false},
		{"code attachment cannot pair", "https://example.test/image.png `" + paired + "`", true},
		{"attachment on another project path", "https://example.test/image.png ![upload](/organizations/org_test/api/v2/projects/prj_other/attachments/" + id + ")", true},
		{"attachment path inside external image", "![preview](<https://example.test/render?x=[](/organizations/org_test/api/v2/projects/prj_test/attachments/" + id + ")>)", true},
		{"label without separating space", "Screenshot:https://example.test/redirect", true},
		{"host-looking attachment path", "https://example.test/image.png ![upload](https://example.test/organizations/org_test/api/v2/projects/prj_test/attachments/" + id + ")", true},
		{"inline image", "![preview](https://example.test/view)", true},
		{"image suffix", "https://example.test/IMAGE%2EPNG?token=secret#view", true},
		{"reference link", "[preview][source]\n\n[source]: https://example.test/image.jpg", true},
		{"reference image", "![preview][source]\n\n[source]: https://example.test/view", true},
		{"autolink", "<https://example.test/image.gif>", true},
		{"claude artifact", "https://claude.ai/public/artifacts/abc", true},
		{"claude private artifact", "https://claude.ai/artifacts/abc", true},
		{"figma board", "https://www.figma.com/board/abc", true},
		{"figma file", "https://figma.com/file/abc", true},
		{"figma proto", "https://figma.com/proto/abc", true},
		{"figma design", "https://figma.com/design/abc", true},
		{"lookalike", "https://figma.com.example.test/design/abc https://other.test/claude.ai/artifacts/abc", false},
		{"ordinary links", "https://claude.ai/chat/abc https://figma.com/ https://example.test/doc.pdf [design doc](https://example.test/doc) https://example.test/pull/1", false},
		{"code", "`https://example.test/image.png`\n\n```md\n![preview](https://example.test/view)\n```", false},
		{"indented code", "    https://figma.com/design/abc", false},
		{"inline destination only", "[https://figma.com/design/abc](https://example.test/doc)", false},
		{"explicit prefix", "Screenshot: https://example.test/redirect", true},
		{"explicit label", "[DESIGN BOARD:](https://example.test/redirect)", true},
		{"explicit reference label", "[Wireframe][ref]\n\n[ref]: https://example.test/redirect", true},
		{"paired", "https://figma.com/design/abc" + paired, false},
		{"paired canonical", "https://figma.com/design/abc " + image.Markdown("org_test"), false},
		{"paired reference attachment", "https://figma.com/design/abc ![upload][shot]\n\n[shot]: attachment:" + id, false},
		{"paired reference canonical", "https://figma.com/design/abc ![upload][shot]\n\n[shot]: /organizations/org_test/api/v2/projects/prj_test/attachments/" + id, false},
		{"unused attachment definition cannot pair", "https://figma.com/design/abc\n\n[shot]: attachment:" + id, true},
		{"other paragraph", "https://figma.com/design/abc\n\n" + paired, true},
		{"other list item", "- https://figma.com/design/abc\n- " + paired, true},
		{"same list item", "- https://figma.com/design/abc\n\n  " + paired, false},
		{"other table cell", "|Source|Upload|\n|---|---|\n|https://figma.com/design/abc|" + paired + "|", true},
		{"same table cell", "|Source|\n|---|\n|https://figma.com/design/abc" + paired + "|", false},
		{"two sources", "https://figma.com/design/abc https://example.test/image.png" + paired, true},
		{"split sources", "https://figma.com/design/abc" + paired + "\n\nhttps://example.test/image.png" + paired, false},
		{"attachment only", paired, false},
		{"fabricated", "https://example.test/image.png [upload](attachment:att_fake)", true},
		{"html image", `<img src="https://example.test/view">`, true},
		{"html paired", `<img src="https://example.test/view">` + paired, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateVisualReferences(test.body, "org_test", "prj_test", func(got string) (Metadata, error) {
				if got == id {
					return image, nil
				}
				return Metadata{}, nil
			})
			var refusal *VisualReferenceError
			if errors.As(err, &refusal) != test.reject || err != nil && refusal == nil {
				t.Fatalf("error=%v reject=%v", err, test.reject)
			}
		})
	}
	for _, suffix := range []string{"png", "jpg", "jpeg", "gif", "webp", "avif", "svg"} {
		t.Run(suffix, func(t *testing.T) {
			err := ValidateVisualReferences("[source](https://example.test/image."+suffix+"?secret=value#fragment)", "org_test", "prj_test", func(string) (Metadata, error) { t.Fatal("unexpected resolver call"); return Metadata{}, nil })
			var refusal *VisualReferenceError
			if !errors.As(err, &refusal) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	for _, label := range []string{"Screenshot", "Screenshots", "Design reference", "Design board", "Mockup", "Wireframe"} {
		for _, body := range []string{label + ": https://example.test/redirect", "[" + label + "](https://example.test/redirect)", "[" + strings.ToUpper(label) + ":](https://example.test/redirect)"} {
			t.Run(body, func(t *testing.T) {
				err := ValidateVisualReferences(body, "org_test", "prj_test", func(string) (Metadata, error) { return image, nil })
				var refusal *VisualReferenceError
				if !errors.As(err, &refusal) {
					t.Fatalf("error=%v", err)
				}
			})
		}
	}
	for _, kind := range []string{"text/plain", "deleted", "cross-project", "bad-hash", "bad-dimensions", "pdf"} {
		t.Run(kind, func(t *testing.T) {
			metadata := image
			switch kind {
			case "text/plain":
				metadata.ContentType = "text/plain"
				metadata.Width = 0
				metadata.Height = 0
			case "deleted":
				metadata.DeletedAt = &image.CreatedAt
			case "cross-project":
				metadata.ProjectID = "prj_other"
			case "bad-hash":
				metadata.SHA256 = "invalid"
			case "bad-dimensions":
				metadata.Width = 0
			case "pdf":
				metadata.ContentType = "application/pdf"
				metadata.Width = 0
				metadata.Height = 0
			}
			err := ValidateVisualReferences("https://example.test/image.png"+paired, "org_test", "prj_test", func(string) (Metadata, error) { return metadata, nil })
			if (err == nil) != (kind == "pdf") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
