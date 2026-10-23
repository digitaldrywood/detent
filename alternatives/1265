package conversation

import (
	"regexp"
	"strconv"
)

// ReferenceKind names what a message reference points at.
type ReferenceKind string

// ReferenceKind values.
const (
	ReferenceIssue        ReferenceKind = "issue"
	ReferenceConversation ReferenceKind = "conversation"
)

// Valid reports whether the reference kind is a known value.
func (k ReferenceKind) Valid() bool {
	return k == ReferenceIssue || k == ReferenceConversation
}

// MaxReferences bounds how many references one message may carry. Extraction
// runs on user text, so the bound is what keeps a pasted document from
// writing thousands of rows for a single message.
const MaxReferences = 20

// maxReferenceNumber bounds an issue number so a long digit run cannot
// overflow the parse or reach the database as a meaningless target.
const maxReferenceNumber = 1_000_000_000

// Reference is one reference extracted from message text, before the hub
// resolves it against the project. Exactly one of Number (an issue) and
// ConversationID is set, matching Kind.
type Reference struct {
	Kind ReferenceKind
	// Organization and Project qualify an issue reference. Both are empty
	// for a bare "#123", which resolves inside the conversation's project.
	Organization string
	Project      string
	Number       int64
	// ConversationID is the target of a conversation reference.
	ConversationID string
	// Text is the token as it appeared, which becomes the reference label.
	Text string
}

// referencePattern matches "#123", "project#123" and "org/project#123". The
// leading group is the character before the token rather than a look-behind,
// which Go's regexp does not have: it keeps "#ff00aa" and a trailing "/1" of
// a path from being read as an issue number.
var referencePattern = regexp.MustCompile(`(^|[^0-9A-Za-z_/#-])(?:([0-9A-Za-z][0-9A-Za-z._-]*)/)?([0-9A-Za-z][0-9A-Za-z._-]*)?#([0-9]{1,10})([^0-9A-Za-z_]|$)`)

// conversationReferencePattern matches a conversation identifier in text.
var conversationReferencePattern = regexp.MustCompile(`\bconv_[0-9a-f]{32}\b`)

// ExtractReferences returns the references a message's text carries, in the
// order they appear, without duplicates and bounded by MaxReferences. The
// references are unresolved: the hub decides which ones name something it
// can see and stores only those.
func ExtractReferences(text string) []Reference {
	references := make([]Reference, 0, MaxReferences)
	seen := map[string]struct{}{}
	add := func(reference Reference, key string) bool {
		if _, duplicate := seen[key]; duplicate {
			return true
		}
		seen[key] = struct{}{}
		references = append(references, reference)
		return len(references) < MaxReferences
	}
	for _, match := range referenceMatches(text) {
		if !add(match, referenceKey(match)) {
			return references
		}
	}
	for _, id := range conversationReferencePattern.FindAllString(text, -1) {
		reference := Reference{Kind: ReferenceConversation, ConversationID: id, Text: id}
		if !add(reference, referenceKey(reference)) {
			return references
		}
	}
	return references
}

// referenceMatches finds the issue tokens. The pattern consumes a trailing
// delimiter, so the scan restarts just after the digits rather than after
// the whole match: "#1,#2" would otherwise lose the second token.
func referenceMatches(text string) []Reference {
	var matches []Reference
	for offset := 0; offset < len(text); {
		found := referencePattern.FindStringSubmatchIndex(text[offset:])
		if found == nil {
			return matches
		}
		group := func(index int) string {
			start, end := found[2*index], found[2*index+1]
			if start < 0 {
				return ""
			}
			return text[offset+start : offset+end]
		}
		number, err := strconv.ParseInt(group(4), 10, 64)
		if err == nil && number > 0 && number < maxReferenceNumber {
			organization, project := group(2), group(3)
			if project == "" {
				// "org/#12" names no project, so it names nothing.
				organization = ""
			}
			matches = append(matches, Reference{
				Kind: ReferenceIssue, Organization: organization, Project: project, Number: number,
				Text: organizationPrefix(organization) + project + "#" + group(4),
			})
		}
		// found[9] is the end of the digits; the trailing delimiter is left
		// for the next token to use as its leading delimiter.
		offset += found[9]
	}
	return matches
}

func organizationPrefix(organization string) string {
	if organization == "" {
		return ""
	}
	return organization + "/"
}

func referenceKey(reference Reference) string {
	if reference.Kind == ReferenceConversation {
		return string(reference.Kind) + ":" + reference.ConversationID
	}
	return string(reference.Kind) + ":" + reference.Organization + "/" + reference.Project + "#" + strconv.FormatInt(reference.Number, 10)
}
