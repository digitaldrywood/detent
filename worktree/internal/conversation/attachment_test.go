package conversation

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNewAttachmentID(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 32 {
		id := NewAttachmentID()
		if !strings.HasPrefix(id, "att_") {
			t.Fatalf("NewAttachmentID() = %q, want an att_ prefix", id)
		}
		if err := ValidateAttachmentID(id); err != nil {
			t.Fatalf("ValidateAttachmentID(%q) = %v, want nil", id, err)
		}
		if seen[id] {
			t.Fatalf("NewAttachmentID() repeated %q", id)
		}
		seen[id] = true
	}
}

func TestValidateAttachmentID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value string
		valid bool
	}{
		{name: "well formed", value: "att_0123456789abcdef0123456789abcdef", valid: true},
		{name: "wrong prefix", value: "msg_0123456789abcdef0123456789abcdef"},
		{name: "too short", value: "att_0123456789abcdef"},
		{name: "uppercase hex", value: "att_0123456789ABCDEF0123456789abcdef"},
		{name: "empty", value: ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateAttachmentID(test.value)
			if test.valid && err != nil {
				t.Fatalf("ValidateAttachmentID(%q) = %v, want nil", test.value, err)
			}
			if !test.valid {
				if err == nil {
					t.Fatalf("ValidateAttachmentID(%q) = nil, want an error", test.value)
				}
				if !errors.Is(err, ErrInvalidID) {
					t.Fatalf("ValidateAttachmentID(%q) = %v, want ErrInvalidID", test.value, err)
				}
			}
		})
	}
}

func TestAttachmentMediaTypes(t *testing.T) {
	t.Parallel()
	want := []string{"image/png", "image/jpeg", "image/gif", "image/webp", "text/plain", "text/markdown", "text/csv", "application/json"}
	got := AttachmentMediaTypes()
	if len(got) != len(want) {
		t.Fatalf("AttachmentMediaTypes() = %v, want %v", got, want)
	}
	for i, media := range want {
		if got[i] != media {
			t.Fatalf("AttachmentMediaTypes()[%d] = %q, want %q", i, got[i], media)
		}
	}
	// The returned slice is a copy: a caller cannot rewrite the allow list.
	got[0] = "application/x-evil"
	if AttachmentMediaTypes()[0] != "image/png" {
		t.Fatal("AttachmentMediaTypes() returned the shared allow list")
	}
}

func TestNormalizeMediaType(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, value, want string }{
		{name: "plain", value: "image/png", want: "image/png"},
		{name: "parameters", value: "text/plain; charset=utf-8", want: "text/plain"},
		{name: "uppercase", value: "IMAGE/PNG", want: "image/png"},
		{name: "spaced", value: "  text/csv  ", want: "text/csv"},
		{name: "empty", value: "", want: ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := NormalizeMediaType(test.value); got != test.want {
				t.Fatalf("NormalizeMediaType(%q) = %q, want %q", test.value, got, test.want)
			}
		})
	}
}

// pngContent is the smallest byte prefix http.DetectContentType needs to
// report image/png; the tail is padding so the file is not empty.
func pngContent() []byte {
	return append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 16)...)
}

func gifContent() []byte { return append([]byte("GIF89a"), bytes.Repeat([]byte{0x21}, 16)...) }

func TestSniffAttachment(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		declared string
		content  []byte
		want     string
		wantErr  bool
	}{
		{name: "png", declared: "image/png", content: pngContent(), want: "image/png"},
		{name: "png with parameters", declared: "image/png; name=shot.png", content: pngContent(), want: "image/png"},
		{name: "gif", declared: "image/gif", content: gifContent(), want: "image/gif"},
		{name: "text", declared: "text/plain", content: []byte("a plain note\n"), want: "text/plain"},
		{name: "markdown", declared: "text/markdown", content: []byte("# Title\n\nBody\n"), want: "text/markdown"},
		{name: "markdown opening with html", declared: "text/markdown", content: []byte("<!-- a comment -->\n# Title\n"), want: "text/markdown"},
		{name: "csv", declared: "text/csv", content: []byte("a,b\n1,2\n"), want: "text/csv"},
		{name: "json", declared: "application/json", content: []byte(`{"a": 1}`), want: "application/json"},
		{name: "empty declaration falls back to the sniff", declared: "", content: pngContent(), want: "image/png"},
		{name: "empty content", declared: "text/plain", content: nil, wantErr: true},
		{name: "unsupported type", declared: "application/pdf", content: []byte("%PDF-1.7\n"), wantErr: true},
		{name: "png declared as text", declared: "text/plain", content: pngContent(), wantErr: true},
		{name: "text declared as png", declared: "image/png", content: []byte("not an image at all"), wantErr: true},
		{name: "gif declared as png", declared: "image/png", content: gifContent(), wantErr: true},
		{name: "invalid json", declared: "application/json", content: []byte("{not json"), wantErr: true},
		{name: "binary declared as csv", declared: "text/csv", content: []byte("a,b\n\x00\x01\x02\x03"), wantErr: true},
		{name: "invalid utf-8 text", declared: "text/plain", content: []byte{0xff, 0xfe, 'a', 'b'}, wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := SniffAttachment(test.declared, test.content)
			if test.wantErr {
				if err == nil {
					t.Fatalf("SniffAttachment(%q, %d bytes) = %q, want an error", test.declared, len(test.content), got)
				}
				if !errors.Is(err, ErrInvalidAttachment) {
					t.Fatalf("SniffAttachment(%q, ...) = %v, want ErrInvalidAttachment", test.declared, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("SniffAttachment(%q, ...) = %v, want nil", test.declared, err)
			}
			if got != test.want {
				t.Fatalf("SniffAttachment(%q, ...) = %q, want %q", test.declared, got, test.want)
			}
		})
	}
}

func TestSanitizeAttachmentName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "plain", value: "notes.md", want: "notes.md"},
		{name: "path stripped", value: "/Users/m/pictures/shot.png", want: "shot.png"},
		{name: "windows path stripped", value: `C:\Users\m\shot.png`, want: "shot.png"},
		{name: "spaces trimmed", value: "  shot.png  ", want: "shot.png"},
		{name: "control characters removed", value: "sh\x00ot\n.png", want: "shot.png"},
		{name: "empty", value: "", wantErr: true},
		{name: "only separators", value: "///", wantErr: true},
		{name: "dot", value: ".", wantErr: true},
		{name: "parent", value: "..", wantErr: true},
		{name: "too long", value: strings.Repeat("a", MaxAttachmentNameBytes+1), wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := SanitizeAttachmentName(test.value)
			if test.wantErr {
				if err == nil {
					t.Fatalf("SanitizeAttachmentName(%q) = %q, want an error", test.value, got)
				}
				if !errors.Is(err, ErrInvalidAttachment) {
					t.Fatalf("SanitizeAttachmentName(%q) = %v, want ErrInvalidAttachment", test.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("SanitizeAttachmentName(%q) = %v, want nil", test.value, err)
			}
			if got != test.want {
				t.Fatalf("SanitizeAttachmentName(%q) = %q, want %q", test.value, got, test.want)
			}
		})
	}
}

func TestValidateAttachmentSelection(t *testing.T) {
	t.Parallel()
	id := func(n int) string {
		return "att_" + strings.Repeat("0", 30) + fmt.Sprintf("%02x", n)
	}
	many := make([]string, 0, MaxMessageAttachments+1)
	for i := range MaxMessageAttachments + 1 {
		many = append(many, id(i))
	}
	cases := []struct {
		name    string
		ids     []string
		wantErr bool
	}{
		{name: "none", ids: nil},
		{name: "one", ids: []string{id(0)}},
		{name: "at the limit", ids: many[:MaxMessageAttachments]},
		{name: "over the limit", ids: many, wantErr: true},
		{name: "repeated", ids: []string{id(0), id(0)}, wantErr: true},
		{name: "malformed", ids: []string{"att_nope"}, wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateAttachmentSelection(test.ids)
			if test.wantErr != (err != nil) {
				t.Fatalf("ValidateAttachmentSelection(%v) = %v, want error %t", test.ids, err, test.wantErr)
			}
			if test.wantErr && !errors.Is(err, ErrInvalidAttachment) {
				t.Fatalf("ValidateAttachmentSelection(%v) = %v, want ErrInvalidAttachment", test.ids, err)
			}
		})
	}
}

func TestValidateAttachmentBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		size    int64
		wantErr bool
	}{
		{name: "one byte", size: 1},
		{name: "at the limit", size: MaxAttachmentBytes},
		{name: "over the limit", size: MaxAttachmentBytes + 1, wantErr: true},
		{name: "empty", size: 0, wantErr: true},
		{name: "negative", size: -1, wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateAttachmentBytes(test.size)
			if test.wantErr != (err != nil) {
				t.Fatalf("ValidateAttachmentBytes(%d) = %v, want error %t", test.size, err, test.wantErr)
			}
		})
	}
}

func TestAttachmentTTLIsSevenDays(t *testing.T) {
	t.Parallel()
	if AttachmentTTL != 7*24*time.Hour {
		t.Fatalf("AttachmentTTL = %v, want 168h", AttachmentTTL)
	}
}

// A message command carries the attachment identifiers, and the request hash
// tells two different selections under one key apart.
func TestCommandAttachments(t *testing.T) {
	t.Parallel()
	first := "att_" + strings.Repeat("a", 32)
	second := "att_" + strings.Repeat("b", 32)
	base := Command{Key: "cmd_1", Kind: CommandMessage, Text: "look at this"}
	withFirst := base
	withFirst.Attachments = []string{first}
	withSecond := base
	withSecond.Attachments = []string{second}
	if err := ValidateCommand(withFirst); err != nil {
		t.Fatalf("ValidateCommand(message with one attachment) = %v, want nil", err)
	}
	tooMany := base
	for range MaxMessageAttachments + 1 {
		tooMany.Attachments = append(tooMany.Attachments, NewAttachmentID())
	}
	if err := ValidateCommand(tooMany); err == nil {
		t.Fatal("ValidateCommand(message over the attachment limit) = nil, want an error")
	}
	// A message with attachments needs no text: the file is the message.
	onlyAttachment := Command{Key: "cmd_2", Kind: CommandMessage, Attachments: []string{first}}
	if err := ValidateCommand(onlyAttachment); err != nil {
		t.Fatalf("ValidateCommand(attachment without text) = %v, want nil", err)
	}
	if err := ValidateCommand(Command{Key: "cmd_3", Kind: CommandMessage}); err == nil {
		t.Fatal("ValidateCommand(message with neither text nor attachments) = nil, want an error")
	}
	// Attachments are part of the payload identity.
	if RequestHash(withFirst) == RequestHash(withSecond) {
		t.Fatal("RequestHash ignores attachments")
	}
	if RequestHash(withFirst) == RequestHash(base) {
		t.Fatal("RequestHash ignores an added attachment")
	}
	// An interrupt carries no attachments.
	interrupt := Command{Key: "cmd_4", Kind: CommandInterrupt, Expected: Expected{AttemptID: "att_1"}, Attachments: []string{first}}
	if err := ValidateCommand(interrupt); err == nil {
		t.Fatal("ValidateCommand(interrupt with attachments) = nil, want an error")
	}
}

// The per-message total and the provider-input split are the two rules the
// hub and the runner both read off an attachment (decisions section 17.1).
func TestValidateAttachmentTotal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		total   int64
		wantErr bool
	}{
		{name: "nothing", total: 0},
		{name: "at the limit", total: MaxAttachmentBytes},
		{name: "one byte over", total: MaxAttachmentBytes + 1, wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateAttachmentTotal(test.total)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateAttachmentTotal(%d) = %v, want an error = %t", test.total, err, test.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidAttachment) {
				t.Fatalf("ValidateAttachmentTotal(%d) = %v, want ErrInvalidAttachment", test.total, err)
			}
		})
	}
}

func TestAttachmentImage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		mime  string
		image bool
	}{
		{name: "png", mime: "image/png", image: true},
		{name: "webp", mime: "image/webp", image: true},
		{name: "markdown", mime: "text/markdown"},
		{name: "json", mime: "application/json"},
		{name: "none", mime: ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := (Attachment{MIME: test.mime}).Image(); got != test.image {
				t.Fatalf("Attachment{MIME: %q}.Image() = %t, want %t", test.mime, got, test.image)
			}
		})
	}
}
