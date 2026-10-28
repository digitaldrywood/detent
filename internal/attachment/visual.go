package attachment

import (
	"fmt"
	"html"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	nethtml "golang.org/x/net/html"
)

const VisualReferenceMessage = "This project requires uploaded screenshots and design references. Upload the file to Detent and include its attachment reference beside each visual source."

type VisualReferenceError struct{ Block int }

func (e *VisualReferenceError) Error() string {
	return fmt.Sprintf("block %d: %s", e.Block, VisualReferenceMessage)
}

var visualBareURL = regexp.MustCompile("(?i)https?://[^\\s<>`\\[\\]]+")

var visualLabelPrefix = regexp.MustCompile(`(?i)(?:^|[\s[:punct:]])(?:screenshots?|design reference|design board|mockup|wireframe):\s*$`)
var visualSourceSeparator = regexp.MustCompile(`(?i)^\s*(?:(?:[,;&]|and\b|or\b)\s*)*$`)

type visualBlock struct {
	number      int
	candidates  int
	attachments []Reference
	prefix      strings.Builder
	introduced  bool
}

func visualReferences(body, organization, project string) ([]*visualBlock, error) {
	source := []byte(body)
	document := goldmark.New(goldmark.WithExtensions(extension.Table, extension.Linkify)).Parser().Parse(text.NewReader(source))
	blocks := map[ast.Node]*visualBlock{}
	ordered := []*visualBlock{}
	blockFor := func(node ast.Node) *visualBlock {
		owner := visualBlockOwner(node)
		block := blocks[owner]
		if block == nil {
			block = &visualBlock{number: len(ordered) + 1}
			ordered = append(ordered, block)
			blocks[owner] = block
		}
		return block
	}
	add := func(block *visualBlock, destination, label string, image bool) {
		destination = html.UnescapeString(string(util.UnescapePunctuations([]byte(destination))))
		prefix := block.prefix.String()
		block.introduced = visualLabelPrefix.MatchString(prefix) || block.introduced && visualSourceSeparator.MatchString(prefix)
		block.prefix.Reset()
		refs := References("[]("+destination+")", organization, project)
		if len(refs) == 1 && refs[0].URL == destination {
			reference := refs[0]
			reference.Image = image
			block.attachments = append(block.attachments, reference)
			block.introduced = false
			return
		}
		if strings.HasPrefix(destination, "attachment:") {
			block.attachments = append(block.attachments, Reference{URL: destination, ID: strings.TrimPrefix(destination, "attachment:"), Image: image})
			block.introduced = false
			return
		}
		if visualDestination(destination, image, visualLabel(label) || block.introduced) {
			block.candidates++
		}
	}
	walkErr := ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch value := node.(type) {
		case *ast.FencedCodeBlock, *ast.CodeBlock:
			return ast.WalkSkipChildren, nil
		case *ast.CodeSpan:
			block := blockFor(node)
			block.prefix.Reset()
			block.introduced = false
			return ast.WalkSkipChildren, nil
		case *ast.Link:
			add(blockFor(node), string(value.Destination), visualLinkLabel(node, source), false)
			return ast.WalkContinue, nil
		case *ast.Image:
			add(blockFor(node), string(value.Destination), visualLinkLabel(node, source), true)
			return ast.WalkSkipChildren, nil
		case *ast.AutoLink:
			add(blockFor(node), string(value.URL(source)), "", false)
			return ast.WalkSkipChildren, nil
		case *ast.Text:
			for parent := node.Parent(); parent != nil; parent = parent.Parent() {
				if parent.Kind() == ast.KindLink || parent.Kind() == ast.KindImage {
					return ast.WalkContinue, nil
				}
			}
			block := blockFor(node)
			content := string(value.Value(source))
			offset := 0
			for _, match := range visualBareURL.FindAllStringIndex(content, -1) {
				block.prefix.WriteString(content[offset:match[0]])
				destination := strings.TrimRight(content[match[0]:match[1]], ".,;:!?")
				extraClosing := strings.Count(destination, ")") - strings.Count(destination, "(")
				for extraClosing > 0 && strings.HasSuffix(destination, ")") {
					destination = strings.TrimSuffix(destination, ")")
					extraClosing--
				}
				add(block, destination, "", false)
				offset = match[1]
			}
			block.prefix.WriteString(content[offset:])
			if value.SoftLineBreak() || value.HardLineBreak() {
				block.prefix.WriteByte('\n')
			}
		case *ast.RawHTML:
			var raw strings.Builder
			for i := range value.Segments.Len() {
				segment := value.Segments.At(i)
				raw.Write(segment.Value(source))
			}
			visualHTMLImages(raw.String(), func(destination string) { add(blockFor(node), destination, "", true) })
		case *ast.HTMLBlock:
			var raw strings.Builder
			for i := range value.Lines().Len() {
				segment := value.Lines().At(i)
				raw.Write(segment.Value(source))
			}
			visualHTMLImages(raw.String(), func(destination string) { add(blockFor(node), destination, "", true) })
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("parse visual references: %w", walkErr)
	}
	return ordered, nil
}

func MarkdownReferences(body, organization, project string) ([]Reference, error) {
	blocks, err := visualReferences(body, organization, project)
	if err != nil {
		return nil, err
	}
	var references []Reference
	for _, block := range blocks {
		references = append(references, block.attachments...)
	}
	return references, nil
}

func ValidateVisualReferences(body, organization, project string, resolve func(string) (Metadata, error)) error {
	ordered, err := visualReferences(body, organization, project)
	if err != nil {
		return err
	}
	cache := map[string]Metadata{}
	for _, block := range ordered {
		if block.candidates == 0 {
			continue
		}
		if block.candidates != 1 {
			return &VisualReferenceError{Block: block.number}
		}
		valid := false
		for _, reference := range block.attachments {
			id := reference.ID
			metadata, found := cache[id]
			if !found {
				var err error
				metadata, err = resolve(id)
				if err != nil {
					return err
				}
				cache[id] = metadata
			}
			if metadata.ProjectID == project && metadata.DeletedAt == nil && metadata.Validate() == nil && (metadata.ContentType == "image/png" || metadata.ContentType == "image/jpeg" || metadata.ContentType == "application/pdf") {
				valid = true
			}
		}
		if !valid {
			return &VisualReferenceError{Block: block.number}
		}
	}
	return nil
}

func visualBlockOwner(node ast.Node) ast.Node {
	var paragraph ast.Node
	for current := node; current != nil; current = current.Parent() {
		switch current.(type) {
		case *extensionast.TableCell, *ast.ListItem:
			return current
		case *ast.Paragraph, *ast.TextBlock, *ast.Heading, *ast.HTMLBlock:
			if paragraph == nil {
				paragraph = current
			}
		}
	}
	if paragraph != nil {
		return paragraph
	}
	return node
}

func visualLinkLabel(node ast.Node, source []byte) string {
	var label strings.Builder
	var appendText func(ast.Node)
	appendText = func(parent ast.Node) {
		for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
			if value, ok := child.(*ast.Text); ok {
				label.Write(value.Value(source))
			}
			appendText(child)
		}
	}
	appendText(node)
	return label.String()
}

func visualLabel(label string) bool {
	switch strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(label), ":"))) {
	case "screenshot", "screenshots", "design reference", "design board", "mockup", "wireframe":
		return true
	default:
		return false
	}
}

func visualDestination(destination string, image, explicit bool) bool {
	value, err := url.Parse(destination)
	if err != nil || value.Host == "" || !strings.EqualFold(value.Scheme, "https") && !strings.EqualFold(value.Scheme, "http") {
		return false
	}
	if image || explicit {
		return true
	}
	switch strings.ToLower(path.Ext(value.Path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".svg":
		return true
	}
	host := strings.ToLower(value.Hostname())
	if host == "claude.ai" {
		return strings.HasPrefix(value.Path, "/artifacts/") || strings.HasPrefix(value.Path, "/public/artifacts/")
	}
	if host == "figma.com" || host == "www.figma.com" {
		for _, prefix := range []string{"/design/", "/file/", "/board/", "/proto/"} {
			if strings.HasPrefix(value.Path, prefix) {
				return true
			}
		}
	}
	return false
}

func visualHTMLImages(raw string, add func(string)) {
	tokenizer := nethtml.NewTokenizer(strings.NewReader(raw))
	for {
		kind := tokenizer.Next()
		if kind == nethtml.ErrorToken {
			return
		}
		if kind != nethtml.StartTagToken && kind != nethtml.SelfClosingTagToken {
			continue
		}
		token := tokenizer.Token()
		if token.Data != "img" {
			continue
		}
		for _, attribute := range token.Attr {
			if attribute.Key == "src" {
				add(attribute.Val)
			}
		}
	}
}
