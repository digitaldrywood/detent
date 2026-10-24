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

type Status struct {
	Label    string
	Resolved bool
}

type Content struct {
	Title       string
	Heading     string
	Note        string
	Subtitle    string
	Status      *Status
	Summary     string
	NextAction  string
	Details     []Detail
	Evidence    []Detail
	ActionLabel string
	ActionURL   string
	Footer      string
	Issue       *Issue
}

type Issue struct {
	URL            string
	Verify         bool
	Notices        []Notice
	UnsubscribeURL string
}

type Notice struct {
	StateFrom  string
	StateTo    string
	Author     string
	CommentURL string
	Related    bool
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

func laneColor(lane string) string {
	switch strings.ToLower(lane) {
	case "in progress":
		return "#2b7fff"
	case "done":
		return "#00bc7d"
	case "rework", "human review":
		return "#fe9a00"
	case "blocked":
		return "#fb2c36"
	case "merging":
		return "#346bf1"
	default:
		return "#71717b"
	}
}

var layout = template.Must(template.New("email").Funcs(template.FuncMap{"laneColor": laneColor}).Parse(`<!doctype html>
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
<<<<<<< HEAD
{{if .Status}}<p style="margin:0 0 8px;font-weight:600;"><span aria-hidden="true" style="display:inline-block;width:8px;height:8px;margin-right:6px;border-radius:50%;background-color:{{if .Status.Resolved}}#00bc7d{{else}}#fe9a00{{end}};"></span>{{.Status.Label}}</p>
<h1 style="margin:0 0 6px;color:#27272a;font-size:20px;line-height:1.4;font-weight:600;overflow-wrap:anywhere;word-break:break-word;">{{.Heading}}</h1>
{{else if .Heading}}
<h1 style="margin:0 0 6px;color:#71717b;font-size:12px;font-weight:600;letter-spacing:0.6px;text-transform:uppercase;">{{.Title}}</h1>
<p style="margin:0 0 16px;color:#27272a;font-size:20px;line-height:1.4;font-weight:600;overflow-wrap:anywhere;word-break:break-word;">{{.Heading}}</p>
{{else}}<h1 style="margin:0 0 16px;color:#27272a;font-size:20px;line-height:1.4;font-weight:600;">{{.Title}}</h1>{{end}}
{{if .Note}}<p style="margin:0 0 16px;color:#71717b;">{{.Note}}</p>{{end}}
{{if .Subtitle}}<p style="margin:0 0 20px;color:#71717b;overflow-wrap:anywhere;word-break:break-word;">{{.Subtitle}}</p>{{end}}
{{if .Summary}}<p style="margin:20px 0;overflow-wrap:anywhere;word-break:break-word;">{{.Summary}}</p>{{end}}
{{if .NextAction}}<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin:20px 0;"><tr><td bgcolor="#fafafa" style="padding:14px 16px;background-color:#fafafa;border:1px solid #e4e4e7;border-left:3px solid #fe9a00;border-radius:8px;">
<p style="margin:0 0 6px;color:#71717b;font-size:12px;font-weight:600;letter-spacing:0.6px;text-transform:uppercase;">Next action</p>
<p style="margin:0;overflow-wrap:anywhere;word-break:break-word;">{{.NextAction}}</p>
</td></tr></table>{{end}}
{{if .Details}}<table aria-label="Details" width="100%" cellpadding="0" cellspacing="0" border="0" style="border-top:1px solid #e4e4e7;border-collapse:collapse;table-layout:fixed;">
=======
<h1 style="margin:0 0 6px;color:#71717b;font-size:12px;font-weight:600;letter-spacing:0.6px;text-transform:uppercase;">{{if .Issue}}<a href="{{.Issue.URL}}" style="color:#71717b;text-decoration:none;">{{.Title}}</a>{{else}}{{.Title}}{{end}}</h1>
<p style="margin:0 0 16px;color:#27272a;font-size:20px;line-height:1.4;font-weight:600;overflow-wrap:anywhere;word-break:break-word;">{{if .Issue}}<a href="{{.Issue.URL}}" style="color:#27272a;text-decoration:none;">{{.Heading}}</a>{{else}}{{.Heading}}{{end}}</p>
{{if .Issue}}
{{with .Issue}}
{{if .Verify}}
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin:24px 0;border:1px solid #e4e4e7;border-radius:10px;background-color:#fafafa;"><tr><td style="padding:20px;">
<p style="margin:0 0 8px;color:#27272a;font-size:16px;font-weight:600;"><span aria-hidden="true" style="color:#00bc7d;">&#9679;</span> Your issue is Done. Please verify it.</p>
<p style="margin:0;color:#71717b;">Open it and choose Verified, or Still broken with a comment so it goes back to work.</p>
{{template "action" .URL}}
</td></tr></table>
{{end}}
<table aria-label="Issue updates" width="100%" cellpadding="0" cellspacing="0" border="0" style="border-top:1px solid #e4e4e7;border-collapse:collapse;table-layout:fixed;">
{{range .Notices}}<tr><td style="padding:14px 0;border-bottom:1px solid #e4e4e7;overflow-wrap:anywhere;word-break:break-word;">
{{if .CommentURL}}<strong>{{.Author}}</strong> commented <a href="{{.CommentURL}}" style="color:#346bf1;text-decoration:none;">View comment</a>
{{else if .Related}}A related issue link was added.
{{else}}{{template "lane" .StateFrom}} <span style="color:#71717b;">&rarr;</span> {{template "lane" .StateTo}}{{end}}
</td></tr>{{end}}
</table>
{{end}}
{{else}}
<table aria-label="Details" width="100%" cellpadding="0" cellspacing="0" border="0" style="border-top:1px solid #e4e4e7;border-collapse:collapse;table-layout:fixed;">
>>>>>>> 56c8a281d (fix: complete prj_6d4919bebd73446798e6cd807feda10e#1622)
{{range .Details}}<tr>
<th scope="row" width="112" align="left" valign="top" style="width:112px;padding:10px 8px 10px 0;border-bottom:1px solid #e4e4e7;color:#71717b;font-weight:400;overflow-wrap:break-word;">{{.Label}}</th>
<td valign="top" style="padding:10px 0;border-bottom:1px solid #e4e4e7;color:#27272a;overflow-wrap:anywhere;word-break:break-word;">{{.Value}}</td>
</tr>{{end}}
<<<<<<< HEAD
</table>{{end}}
{{if .Evidence}}<p style="margin:24px 0 6px;color:#71717b;font-size:12px;font-weight:600;letter-spacing:0.6px;text-transform:uppercase;">Evidence</p>
<table aria-label="Evidence counts" width="100%" cellpadding="0" cellspacing="0" border="0" style="border-top:1px solid #e4e4e7;border-collapse:collapse;table-layout:fixed;">
{{range .Evidence}}<tr>
<th scope="row" align="left" valign="top" style="padding:8px 8px 8px 0;border-bottom:1px solid #e4e4e7;color:#71717b;font-weight:400;overflow-wrap:anywhere;word-break:break-word;">{{.Label}}</th>
<td align="right" valign="top" style="padding:8px 0;border-bottom:1px solid #e4e4e7;color:#27272a;">{{.Value}}</td>
</tr>{{end}}
</table>{{end}}
=======
</table>
{{end}}
>>>>>>> 56c8a281d (fix: complete prj_6d4919bebd73446798e6cd807feda10e#1622)
<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="margin-top:24px;"><tr>
<td align="center" bgcolor="#346bf1" style="background-color:#346bf1;border-radius:8px;mso-padding-alt:12px 18px;">
<a href="{{.ActionURL}}" style="display:inline-block;padding:12px 18px;border:1px solid #346bf1;border-radius:8px;color:#ffffff;font-size:14px;font-weight:600;line-height:1.2;text-decoration:none;">{{.ActionLabel}}</a>
</td></tr></table>
{{if not .Issue}}<p style="margin:18px 0 4px;color:#71717b;font-size:12px;overflow-wrap:anywhere;word-break:break-word;">Or open this link: <a href="{{.ActionURL}}" style="color:#346bf1;text-decoration:none;">{{.ActionURL}}</a></p>{{end}}
</td></tr>
</table>
</td></tr>
<tr><td style="padding:16px 28px 0;color:#71717b;font-size:12px;">{{.Footer}}{{with .Issue}} <a href="{{.UnsubscribeURL}}" style="color:#71717b;">Unsubscribe from this issue</a>{{end}}</td></tr>
</table>
</td></tr>
</table>
</body>
</html>
{{define "lane"}}<span style="display:inline-block;padding:3px 8px;border:1px solid #e4e4e7;border-radius:6px;background-color:#fafafa;color:#27272a;"><span aria-hidden="true" style="color:{{laneColor .}};">&#9679;</span> {{.}}</span>{{end}}
{{define "action"}}<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="margin-top:18px;"><tr><td align="center" bgcolor="#346bf1" style="background-color:#346bf1;border-radius:8px;mso-padding-alt:12px 18px;"><a href="{{.}}" style="display:inline-block;padding:12px 18px;border:1px solid #346bf1;border-radius:8px;color:#ffffff;font-size:14px;font-weight:600;line-height:1.2;text-decoration:none;">Verify</a></td></tr></table>{{end}}`))
