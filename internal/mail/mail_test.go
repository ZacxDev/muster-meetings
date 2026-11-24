package mail_test

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	netmail "net/mail"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/mail"
	"github.com/ZacxDev/muster-meetings/internal/mailtest"
)

func fake(t *testing.T) *mailtest.Fake {
	t.Helper()
	f, err := mailtest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	return f
}

func sender(t *testing.T, f *mailtest.Fake) *mail.Sender {
	t.Helper()
	s, err := mail.New(mail.Config{Addr: f.Addr(), User: "meetings", Pass: "fixture-password", From: "meetings@mail.example.test", ServerName: "127.0.0.1", RootCAs: f.Pool(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var names = []string{"Ada L.", "Grace H.", "Grace K.", "Linus T."}

func TestYoursIsTheRecipientsWholeNameOrUniqueFirstName(t *testing.T) {
	for _, c := range []struct {
		assignee, recipient string
		want                bool
	}{
		{"Grace H.", "Grace H.", true},
		{"grace h", "Grace H.", true},
		{"Grace H.", "Grace K.", false},
		{"Grace", "Grace H.", false}, // two Graces: nobody's
		{"Linus", "Linus T.", true},  // one Linus
		{"Linus", "Ada L.", false},
		{"", "Ada L.", false},
		{"Ada L.", "", false},
		{"Ada Lovelace", "Ada L.", false},
	} {
		if got := mail.Yours(c.assignee, c.recipient, names); got != c.want {
			t.Errorf("Yours(%q, %q) = %v, want %v", c.assignee, c.recipient, got, c.want)
		}
	}
}

func fixtureNotes() mail.Notes {
	return mail.Notes{OwnerName: "Ada L.", Title: "Q3 planning", When: "24 Nov 2026", RecipientName: "Grace H.",
		Items: []mail.NoteItem{
			{Text: "Send the venue shortlist", Assignee: "Grace H.", Due: "Friday"},
			{Text: "Book the caterer", Assignee: "Ada L."},
			{Text: "Draft the agenda <b>now</b>", Assignee: "Grace K."},
		},
		Names:    names,
		VideoURL: "https://meet.example.test/r/TOKEN", VideoUntil: "until 24 Dec 2026", Attached: true}
}

func TestRenderTextIsTheFormat(t *testing.T) {
	subj, text, _ := mail.Render(fixtureNotes())
	if subj != "Notes: Q3 planning" {
		t.Errorf("subject = %q", subj)
	}
	want := `Hi Grace,

Thanks for joining "Q3 planning" on 24 Nov 2026. Here are the meeting notes from Ada L..

YOUR ACTION ITEMS
  * Send the venue shortlist (due Friday)

EVERYONE ELSE'S ACTION ITEMS
  - Ada L.: Book the caterer
  - Grace K.: Draft the agenda <b>now</b>

Watch the recording (until 24 Dec 2026):
https://meet.example.test/r/TOKEN

The full transcript is attached, as text and as WebVTT.

--
Sent by Ada L. via Meetings. Reply to this email to reach Ada L..
`
	if text != want {
		t.Errorf("text =\n%s\nwant\n%s", text, want)
	}
}

// section is the HTML between start and the next "</table>" or "</div>".
func section(t *testing.T, html, start string) string {
	t.Helper()
	i := strings.Index(html, start)
	if i < 0 {
		return ""
	}
	rest := html[i:]
	j := strings.Index(rest, "</table>")
	if k := strings.Index(rest, "</div>"); k >= 0 && (j < 0 || k < j) {
		j = k
	}
	return rest[:j]
}

func TestRenderHTMLStylesYoursAndMutesOthers(t *testing.T) {
	_, _, html := mail.Render(fixtureNotes())
	yours := section(t, html, `id="yours"`)
	others := section(t, html, `id="others"`)
	if !strings.Contains(yours, `background-color:#eaf1fe;border-left:4px solid #2f6fdf`) || !strings.Contains(yours, "Send the venue shortlist") {
		t.Errorf("YOUR section is not the styled block with the recipient's item:\n%s", yours)
	}
	for _, other := range []string{"Book the caterer", "Draft the agenda"} {
		if strings.Contains(yours, other) {
			t.Errorf("YOUR section shows another person's item %q:\n%s", other, yours)
		}
		if !strings.Contains(others, other) {
			t.Errorf("the muted section lacks %q:\n%s", other, others)
		}
	}
	if !strings.Contains(others, `color:#6e7781`) {
		t.Errorf("others are not muted:\n%s", others)
	}
	if strings.Contains(html, "<b>now</b>") || !strings.Contains(html, "&lt;b&gt;now&lt;/b&gt;") {
		t.Error("an item's text was not escaped in the HTML")
	}
	if strings.Contains(html, "<img") || strings.Contains(html, "<link") || strings.Contains(html, "<style") || strings.Contains(html, "<script") {
		t.Error("the HTML loads something or carries a style sheet: inline styles only, nothing remote")
	}
}

func TestBuildIsMixedWithAlternativeAndTwoAttachments(t *testing.T) {
	_, text, html := mail.Render(fixtureNotes())
	raw, err := mail.Build(mail.Message{FromName: "Ada L. via Meetings", FromAddr: "meetings@mail.example.test", ReplyTo: "ada@example.test",
		ToName: "Grace H.", ToAddr: "grace@example.test", Subject: "Notes: Q3 planning — draft", MessageID: "draft-1",
		Date: time.Date(2026, 11, 24, 15, 4, 5, 0, time.UTC), Text: text, HTML: html,
		Attachments: []mail.Attachment{{Name: "meeting.txt", ContentType: "text/plain; charset=utf-8", Data: []byte("0:00 Ada L.: hello\n")},
			{Name: "meeting.vtt", ContentType: "text/vtt; charset=utf-8", Data: []byte("WEBVTT\n")}}})
	if err != nil {
		t.Fatal(err)
	}
	head := string(raw[:bytes.Index(raw, []byte("\r\n\r\n"))])
	wantHead := "From: \"Ada L. via Meetings\" <meetings@mail.example.test>\r\n" +
		"Reply-To: <ada@example.test>\r\n" +
		"To: \"Grace H.\" <grace@example.test>\r\n" +
		"Subject: =?utf-8?q?Notes:_Q3_planning_=E2=80=94_draft?=\r\n" +
		"Date: Tue, 24 Nov 2026 15:04:05 +0000\r\n" +
		"Message-ID: <draft-1@mail.example.test>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"mixed-"
	if !strings.HasPrefix(head, wantHead) || !regexp.MustCompile(`boundary="mixed-[0-9a-f]{16}"$`).MatchString(head) {
		t.Errorf("headers =\n%s\nwant\n%s<16 hex>\"", head, wantHead)
	}
	m, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	mt, p, _ := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if mt != "multipart/mixed" {
		t.Fatalf("top = %s", mt)
	}
	mr := multipart.NewReader(m.Body, p["boundary"])
	var shape []string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		ct, pp, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if ct == "multipart/alternative" {
			ar := multipart.NewReader(part, pp["boundary"])
			for {
				ap, err := ar.NextPart()
				if err == io.EOF {
					break
				}
				act, _, _ := mime.ParseMediaType(ap.Header.Get("Content-Type"))
				body, _ := io.ReadAll(ap) // quoted-printable is decoded by the reader
				shape = append(shape, "alt:"+act)
				if act == "text/plain" && string(body) != strings.ReplaceAll(text, "\n", "\r\n") {
					t.Errorf("text part does not round-trip")
				}
			}
			continue
		}
		_, dp, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		shape = append(shape, "att:"+ct+":"+dp["filename"])
	}
	want := "alt:text/plain alt:text/html att:text/plain:meeting.txt att:text/vtt:meeting.vtt"
	if strings.Join(shape, " ") != want {
		t.Errorf("MIME shape = %s, want %s", strings.Join(shape, " "), want)
	}
}

func TestBuildRefusesAHeaderInAName(t *testing.T) {
	raw, err := mail.Build(mail.Message{FromName: "Ada\r\nBcc: evil@example.test", FromAddr: "meetings@mail.example.test", ToAddr: "grace@example.test", MessageID: "x", Date: time.Unix(0, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("\r\nBcc:")) {
		t.Errorf("a name started a header:\n%s", raw)
	}
	if _, err := mail.Build(mail.Message{FromAddr: "meetings@mail.example.test", ToAddr: "a@example.test, b@example.test", MessageID: "x"}); err == nil {
		t.Error("a list of recipients was accepted as one")
	}
}

func TestBuildRefusesAMessageOverTheLimit(t *testing.T) {
	_, err := mail.Build(mail.Message{FromAddr: "meetings@mail.example.test", ToAddr: "grace@example.test", MessageID: "x",
		Attachments: []mail.Attachment{{Name: "big.txt", ContentType: "text/plain", Data: bytes.Repeat([]byte("x"), mail.MaxMessageBytes)}}})
	if err != mail.ErrTooLarge {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}

func TestSendRequiresStartTLS(t *testing.T) {
	f := fake(t)
	f.SetNoTLS(true)
	err := sender(t, f).Send(context.Background(), "grace@example.test", []byte("Subject: x\r\n\r\nhi\r\n"))
	if mail.ClassOf(err) != mail.Temporary || !strings.Contains(err.Error(), mail.ErrNoStartTLS) {
		t.Errorf("err = %v, want temporary %q", err, mail.ErrNoStartTLS)
	}
	if n := len(f.Messages()); n != 0 {
		t.Errorf("a server without STARTTLS received %d messages", n)
	}
}

func TestSendOverTLSWithAuth(t *testing.T) {
	f := fake(t)
	if err := sender(t, f).Send(context.Background(), "grace@example.test", []byte("Subject: x\r\n\r\n.hi\r\n")); err != nil {
		t.Fatal(err)
	}
	got := f.Messages()
	if len(got) != 1 || !got[0].TLS || got[0].User != "meetings" || got[0].From != "meetings@mail.example.test" || strings.Join(got[0].To, ",") != "grace@example.test" {
		t.Fatalf("got %+v", got)
	}
	if string(got[0].Data) != "Subject: x\r\n\r\n.hi\r\n" {
		t.Errorf("data = %q (dot-stuffing must round-trip)", got[0].Data)
	}
}

func TestSendClassifiesFailures(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(*mailtest.Fake)
		want  mail.Class
	}{
		{"recipient refused", func(f *mailtest.Fake) { f.Reply("RCPT", 550) }, mail.Permanent},
		{"message deferred", func(f *mailtest.Fake) { f.Reply("END", 451) }, mail.Temporary},
		{"message refused", func(f *mailtest.Fake) { f.Reply("END", 554) }, mail.Permanent},
		{"no answer after the message", func(f *mailtest.Fake) { f.Hang(true) }, mail.Unknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := fake(t)
			c.setup(f)
			err := sender(t, f).Send(context.Background(), "grace@example.test", []byte("Subject: x\r\n\r\nhi\r\n"))
			if mail.ClassOf(err) != c.want {
				t.Errorf("err = %v, want class %v", err, c.want)
			}
			if strings.Contains(err.Error(), "grace@") {
				t.Errorf("the error names the recipient: %v", err)
			}
		})
	}
}
