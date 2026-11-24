package mail

import (
	"bytes"
	htmltemplate "html/template"
	"strings"
	texttemplate "text/template"
	"unicode"
)

// NoteItem is one action item as an email shows it.
type NoteItem struct {
	Text     string `json:"text"`
	Assignee string `json:"assignee"`
	Due      string `json:"due"`
}

// Notes is one recipient's meeting notes (the email format, decisions 1–3:
// thanks, YOUR action items in a styled block, everyone else's muted, the
// transcript attached and the video as a link).
type Notes struct {
	OwnerName string
	Title     string // the meeting's title, or its date
	When      string // e.g. 24 Nov 2026
	// RecipientName is the name the recipient spoke under (their knock
	// name, or the owner's display name): the YOUR section is the items
	// assigned to it (Yours).
	RecipientName string
	ForOwner      bool
	Items         []NoteItem
	// Names is every speaker name in the meeting, for Yours' first-name
	// rule.
	Names      []string
	VideoURL   string
	VideoUntil string // e.g. "until 24 Dec 2026"; "" for no end
	// Attached says the transcript is attached; false when it was too
	// large (the email says so).
	Attached bool
}

func words(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }), " ")
}

func first(s string) string {
	if f := strings.Fields(words(s)); len(f) > 0 {
		return f[0]
	}
	return ""
}

// FromName is the notes' sender name: "<owner> via Meetings" (decision 7),
// "Meetings" when muster gave no name.
func FromName(owner string) string {
	if o := clean(owner); o != "" {
		return o + " via Meetings"
	}
	return "Meetings"
}

// Yours reports whether an item assigned to assignee is the recipient's:
// the assignee is their whole name (case and punctuation aside), or their
// first name alone when no other name in names starts with it. An empty
// assignee is nobody's.
//
// 🔴 This is the one rule that decides what a recipient reads under "Your
// action items"; it errs to "not yours" (the item then shows, muted, under
// everyone else's).
func Yours(assignee, recipient string, names []string) bool {
	a, r := words(assignee), words(recipient)
	if a == "" || r == "" {
		return false
	}
	if a == r {
		return true
	}
	if strings.Contains(a, " ") || a != first(recipient) {
		return false
	}
	for _, n := range names {
		if words(n) != r && first(n) == a {
			return false
		}
	}
	return true
}

// Split is items as one recipient reads them: theirs, and everyone else's,
// each in order.
func Split(items []NoteItem, recipient string, names []string) (yours, others []NoteItem) {
	for _, it := range items {
		if Yours(it.Assignee, recipient, names) {
			yours = append(yours, it)
		} else {
			others = append(others, it)
		}
	}
	return yours, others
}

type notesView struct {
	Notes
	Greeting string
	Yours    []NoteItem
	Others   []NoteItem
}

// Render is the email's subject, plain text and HTML.
func Render(n Notes) (subject, text, html string) {
	v := notesView{Notes: n}
	v.Yours, v.Others = Split(n.Items, n.RecipientName, n.Names)
	v.Greeting = "Hi " + firstName(n.RecipientName) + ","
	if strings.TrimSpace(n.RecipientName) == "" {
		v.Greeting = "Hi,"
	}
	subject = "Notes: " + n.Title
	var t, h bytes.Buffer
	if err := textNotes.Execute(&t, v); err != nil {
		panic(err) // the templates are constants: TestRender* runs them
	}
	if err := htmlNotes.Execute(&h, v); err != nil {
		panic(err)
	}
	return subject, t.String(), h.String()
}

func firstName(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

var funcs = map[string]any{
	"due": func(d string) string {
		if strings.TrimSpace(d) == "" {
			return ""
		}
		return " (due " + d + ")"
	},
	"who": func(a string) string {
		if strings.TrimSpace(a) == "" {
			return "Unassigned"
		}
		return a
	},
}

var textNotes = texttemplate.Must(texttemplate.New("t").Funcs(funcs).Parse(`{{.Greeting}}

{{if .ForOwner}}Here are the notes from "{{.Title}}" on {{.When}}, as you sent them to everyone.{{else}}Thanks for joining "{{.Title}}" on {{.When}}. Here are the meeting notes from {{.OwnerName}}.{{end}}

YOUR ACTION ITEMS
{{range .Yours}}  * {{.Text}}{{due .Due}}
{{else}}  You have no action items from this meeting.
{{end}}{{if .Others}}
EVERYONE ELSE'S ACTION ITEMS
{{range .Others}}  - {{who .Assignee}}: {{.Text}}{{due .Due}}
{{end}}{{end}}{{if .VideoURL}}
Watch the recording{{if .VideoUntil}} ({{.VideoUntil}}){{end}}:
{{.VideoURL}}
{{end}}
{{if .Attached}}The full transcript is attached, as text and as WebVTT.{{else}}The transcript was too long to attach; ask {{.OwnerName}} for it.{{end}}

--
Sent by {{.OwnerName}} via Meetings. Reply to this email to reach {{.OwnerName}}.
`))

var htmlNotes = htmltemplate.Must(htmltemplate.New("h").Funcs(funcs).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="color-scheme" content="light dark"><meta name="supported-color-schemes" content="light dark"><title>Notes: {{.Title}}</title></head>
<body style="margin:0;padding:0;background-color:#f3f4f6;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#f3f4f6;"><tr><td align="center" style="padding:24px 12px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="max-width:560px;background-color:#ffffff;border:1px solid #e1e4e8;border-radius:12px;"><tr><td style="padding:24px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;font-size:15px;line-height:1.5;color:#1f2328;">
<p style="margin:0 0 12px;">{{.Greeting}}</p>
<p style="margin:0 0 20px;">{{if .ForOwner}}Here are the notes from <strong>{{.Title}}</strong> on {{.When}}, as you sent them to everyone.{{else}}Thanks for joining <strong>{{.Title}}</strong> on {{.When}}. Here are the meeting notes from {{.OwnerName}}.{{end}}</p>
<table role="presentation" id="yours" width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#eaf1fe;border-left:4px solid #2f6fdf;border-radius:8px;"><tr><td style="padding:14px 16px;">
<p style="margin:0 0 8px;font-size:13px;font-weight:700;letter-spacing:0.04em;text-transform:uppercase;color:#1f55b8;">&#10003;&nbsp; Your action items</p>
{{if .Yours}}<ul style="margin:0;padding-left:20px;color:#1f2328;">{{range .Yours}}<li style="margin:4px 0;font-weight:600;">{{.Text}}{{if .Due}}<span style="font-weight:400;color:#57606a;">{{due .Due}}</span>{{end}}</li>{{end}}</ul>{{else}}<p style="margin:0;color:#57606a;">You have no action items from this meeting.</p>{{end}}
</td></tr></table>
{{if .Others}}<div id="others" style="margin-top:20px;color:#6e7781;font-size:14px;">
<p style="margin:0 0 6px;font-size:12px;font-weight:700;letter-spacing:0.04em;text-transform:uppercase;color:#8c959f;">Everyone else&#39;s action items</p>
<ul style="margin:0;padding-left:20px;">{{range .Others}}<li style="margin:3px 0;">{{who .Assignee}}: {{.Text}}{{due .Due}}</li>{{end}}</ul>
</div>{{end}}
{{if .VideoURL}}<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="margin-top:24px;"><tr><td style="background-color:#2f6fdf;border-radius:8px;"><a href="{{.VideoURL}}" style="display:inline-block;padding:10px 18px;font-weight:600;color:#ffffff;text-decoration:none;">&#9654;&nbsp; Watch the recording</a></td></tr></table>
{{if .VideoUntil}}<p style="margin:6px 0 0;font-size:13px;color:#6e7781;">The link works {{.VideoUntil}}.</p>{{end}}{{end}}
<p style="margin:24px 0 0;font-size:13px;color:#6e7781;">{{if .Attached}}The full transcript is attached, as text and as WebVTT.{{else}}The transcript was too long to attach; ask {{.OwnerName}} for it.{{end}}</p>
<p style="margin:16px 0 0;padding-top:12px;border-top:1px solid #e1e4e8;font-size:12px;color:#8c959f;">Sent by {{.OwnerName}} via Meetings. Reply to this email to reach {{.OwnerName}}.</p>
</td></tr></table>
</td></tr></table>
</body></html>
`))

// Confirm is the owner's confirmation email: subject, text, HTML.
func Confirm(ownerName, url, until string) (subject, text, html string) {
	subject = "Confirm your address for Meetings"
	var t, h bytes.Buffer
	v := map[string]string{"Name": firstName(ownerName), "URL": url, "Until": until}
	if err := textConfirm.Execute(&t, v); err != nil {
		panic(err)
	}
	if err := htmlConfirm.Execute(&h, v); err != nil {
		panic(err)
	}
	return subject, t.String(), h.String()
}

var textConfirm = texttemplate.Must(texttemplate.New("c").Parse(`Hi{{if .Name}} {{.Name}}{{end}},

Open this link to confirm the address Meetings sends your meeting notes from (as Reply-To) and to:
{{.URL}}

It works until {{.Until}}. If you did not ask for this, ignore this email: nothing changes.
`))

var htmlConfirm = htmltemplate.Must(htmltemplate.New("c").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="color-scheme" content="light dark"><title>Confirm your address</title></head>
<body style="margin:0;padding:24px 12px;background-color:#f3f4f6;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;color:#1f2328;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="max-width:520px;margin:0 auto;background-color:#ffffff;border:1px solid #e1e4e8;border-radius:12px;"><tr><td style="padding:24px;font-size:15px;line-height:1.5;">
<p style="margin:0 0 12px;">Hi{{if .Name}} {{.Name}}{{end}},</p>
<p style="margin:0 0 20px;">Confirm the address Meetings sends your meeting notes from (as Reply-To) and to.</p>
<table role="presentation" cellpadding="0" cellspacing="0" border="0"><tr><td style="background-color:#2f6fdf;border-radius:8px;"><a href="{{.URL}}" style="display:inline-block;padding:10px 18px;font-weight:600;color:#ffffff;text-decoration:none;">Confirm this address</a></td></tr></table>
<p style="margin:20px 0 0;font-size:13px;color:#6e7781;">The link works until {{.Until}}. If you did not ask for this, ignore this email: nothing changes.</p>
</td></tr></table>
</body></html>
`))
