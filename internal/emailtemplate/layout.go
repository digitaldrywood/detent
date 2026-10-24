package emailtemplate

import (
	"fmt"
	"html/template"
	"io/fs"
	"strings"

	"github.com/digitaldrywood/detent"
)

type Message struct {
	To           string
	Subject      string
	Body         string
	HTMLBody     string
	InlineImages []InlineImage
}

type InlineImage struct {
	ContentID   string
	ContentType string
	Data        []byte
}

type Detail struct {
	Label string
	Value string
}

type Content struct {
	Title       string
	Heading     string
	Note        string
	Details     []Detail
	ActionLabel string
	ActionURL   string
	Footer      string
}

func Render(content Content) (Message, error) {
	var html strings.Builder
	if err := layout.Execute(&html, content); err != nil {
		return Message{}, fmt.Errorf("render email layout: %w", err)
	}
	mark, err := fs.ReadFile(detent.StaticFS(), "img/detent-mark.png")
	if err != nil {
		return Message{}, fmt.Errorf("read email mark: %w", err)
	}
	var text strings.Builder
	for _, detail := range content.Details {
		fmt.Fprintf(&text, "%s: %s\n", detail.Label, detail.Value)
	}
	if content.Note != "" {
		fmt.Fprintf(&text, "%s\n\n", content.Note)
	}
	fmt.Fprintf(&text, "%s\n\n%s\n", content.ActionURL, content.Footer)
	return Message{
		Body: text.String(), HTMLBody: html.String(),
		InlineImages: []InlineImage{{ContentID: "detent-mark", ContentType: "image/png", Data: mark}},
	}, nil
}

var layout = template.Must(template.New("email").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light">
<meta name="supported-color-schemes" content="light">
<title>{{.Title}}</title>
</head>
<body bgcolor="#f4f4f5" style="margin:0;padding:0;background-color:#f4f4f5;color:#27272a;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',system-ui,'Geist',sans-serif;font-size:14px;line-height:1.5;color-scheme:light;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" bgcolor="#f4f4f5" style="background-color:#f4f4f5;">
<tr><td align="center" style="padding:32px 16px;">
<table role="presentation" width="600" cellpadding="0" cellspacing="0" border="0" style="width:100%;max-width:600px;">
<tr><td bgcolor="#ffffff" style="background-color:#ffffff;border:1px solid #e4e4e7;border-radius:10px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">
<tr><td style="padding:20px 28px;border-bottom:1px solid #e4e4e7;">
<table role="presentation" cellpadding="0" cellspacing="0" border="0"><tr>
<td style="vertical-align:middle;"><img src="cid:detent-mark" width="24" height="24" alt="" style="display:block;border:0;"></td>
<td style="padding-left:8px;vertical-align:middle;color:#27272a;font-size:16px;font-weight:600;">Detent</td>
</tr></table>
</td></tr>
<tr><td style="padding:28px;">
{{if .Heading}}<h1 style="margin:0 0 6px;color:#71717b;font-size:12px;font-weight:600;letter-spacing:0.6px;text-transform:uppercase;">{{.Title}}</h1>
<p style="margin:0 0 16px;color:#27272a;font-size:20px;line-height:1.4;font-weight:600;overflow-wrap:anywhere;word-break:break-word;">{{.Heading}}</p>
{{else}}<h1 style="margin:0 0 16px;color:#27272a;font-size:20px;line-height:1.4;font-weight:600;">{{.Title}}</h1>{{end}}
{{if .Note}}<p style="margin:0 0 16px;color:#71717b;">{{.Note}}</p>{{end}}
{{if .Details}}<table aria-label="Details" width="100%" cellpadding="0" cellspacing="0" border="0" style="border-top:1px solid #e4e4e7;border-collapse:collapse;table-layout:fixed;">
{{range .Details}}<tr>
<th scope="row" width="112" align="left" valign="top" style="width:112px;padding:10px 8px 10px 0;border-bottom:1px solid #e4e4e7;color:#71717b;font-weight:400;overflow-wrap:break-word;">{{.Label}}</th>
<td valign="top" style="padding:10px 0;border-bottom:1px solid #e4e4e7;color:#27272a;overflow-wrap:anywhere;word-break:break-word;">{{.Value}}</td>
</tr>{{end}}
</table>{{end}}
<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="margin-top:24px;"><tr>
<td align="center" bgcolor="#346bf1" style="background-color:#346bf1;border-radius:8px;mso-padding-alt:12px 18px;">
<a href="{{.ActionURL}}" style="display:inline-block;padding:12px 18px;border:1px solid #346bf1;border-radius:8px;color:#ffffff;font-size:14px;font-weight:600;line-height:1.2;text-decoration:none;">{{.ActionLabel}}</a>
</td></tr></table>
<p style="margin:18px 0 4px;color:#71717b;font-size:12px;overflow-wrap:anywhere;word-break:break-word;">Or open this link: <a href="{{.ActionURL}}" style="color:#346bf1;text-decoration:none;">{{.ActionURL}}</a></p>
</td></tr>
</table>
</td></tr>
<tr><td style="padding:16px 28px 0;color:#71717b;font-size:12px;">{{.Footer}}</td></tr>
</table>
</td></tr>
</table>
</body>
</html>`))
