package server_test

// The post-call feature, end to end through the server, against a fake
// OpenRouter (items, vision, summary) and a fake SMTP server: the live
// items, the post run at End, the drafts, the owner's confirmed address,
// Send and the exact message each recipient gets, reprocess and revert,
// owner scoping on every new route, and the log.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	netmail "net/mail"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/contacts"
	"github.com/ZacxDev/muster-meetings/internal/contactstest"
	"github.com/ZacxDev/muster-meetings/internal/items"
	"github.com/ZacxDev/muster-meetings/internal/mail"
	"github.com/ZacxDev/muster-meetings/internal/mailtest"
	"github.com/ZacxDev/muster-meetings/internal/openroutertest"
	"github.com/ZacxDev/muster-meetings/internal/server"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

const (
	itemsModel  = "deepseek/deepseek-v4.1-flash-1017"
	itemsPrefs  = `{"data_collection":"deny","order":["deepinfra/fp8"],"allow_fallbacks":false}`
	visionModel = "anthropic/claude-haiku-5.5"
	visionPrefs = `{"data_collection":"deny","order":["anthropic"],"allow_fallbacks":false}`
	mailFrom    = "meetings@mail.example.test"
	adaAddr     = "ada.owner@example.test"
	graceAddr   = "grace.guest@example.test"
)

type pc struct {
	*env
	or   *openroutertest.Fake
	smtp *mailtest.Fake
}

// aiConfig is the analysis against the fake.
func aiConfig(t *testing.T, or *openroutertest.Fake) *server.AIConfig {
	t.Helper()
	ip, err := items.ParsePrefs(itemsPrefs, items.AdapterChatJSON)
	if err != nil {
		t.Fatal(err)
	}
	vp, _ := items.ParsePrefs(visionPrefs, items.AdapterChatJSON)
	return &server.AIConfig{
		Items:  items.Config{Model: itemsModel, Adapter: items.AdapterChatJSON, Prefs: ip, APIKey: "or-fixture-not-a-key", BaseURL: or.URL()},
		Vision: items.Config{Model: visionModel, Adapter: items.AdapterChatJSON, Prefs: vp, APIKey: "or-fixture-not-a-key", BaseURL: or.URL()},
		// The live worker's own ticker never fires in a test: LiveTick does.
		Every: time.Hour, RunRetry: time.Millisecond, RecordingWait: 10 * time.Second,
	}
}

// pcEnv is an env with the analysis, the mail server and contacts
// configured, and a bucket when rec.
func pcEnv(t *testing.T, rec bool) *pc {
	t.Helper()
	or := openroutertest.New()
	t.Cleanup(or.Close)
	sm, err := mailtest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sm.Close)
	sender, err := mail.New(mail.Config{Addr: sm.Addr(), From: mailFrom, ServerName: "127.0.0.1", RootCAs: sm.Pool(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	f := contactstest.New(cToken)
	t.Cleanup(f.Close)
	opt := func(c *server.Config) {
		c.Contacts = contacts.New(f.URL, cToken)
		c.AI = aiConfig(t, or)
		c.Mail = sender
		c.MailBackoff = []time.Duration{time.Millisecond}
	}
	var e *env
	if rec {
		e, _ = recEnv(t, opt)
	} else {
		e = newEnvOpts(t, nil, opt)
	}
	return &pc{env: e, or: or, smtp: sm}
}

// say stores a final line by speaker (a guest when guest) at start..end ms.
func (e *env) say(c call, identity, speaker string, start, end int64, text string) int64 {
	e.t.Helper()
	ctx := context.Background()
	track := "TR_" + c.ID + "_" + strings.ReplaceAll(identity, "-", "")
	if _, err := e.store.UpsertTrack(ctx, c.Room, track, identity, speaker, e.clock.Now()); err != nil {
		e.t.Fatal(err)
	}
	id, err := e.store.AddSegment(ctx, track, store.SegText, start, end, text, "fake")
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

type itemsEv struct {
	Items []struct {
		Text     string  `json:"text"`
		Assignee string  `json:"assignee"`
		Segments []int64 `json:"segments"`
	} `json:"items"`
	Paused bool `json:"paused"`
}

// itemsOf reads items events until one with an item, or paused.
func itemsOf(t *testing.T, s *stream) itemsEv {
	t.Helper()
	for {
		var v itemsEv
		if err := json.Unmarshal([]byte(s.want("items").Data), &v); err != nil {
			t.Fatal(err)
		}
		if len(v.Items) > 0 || v.Paused {
			return v
		}
	}
}

// 🔴 4b live: a window of the meeting's lines, its items on the owner's AND
// an admitted guest's stream; the AI switch off is no call at all.
func TestLiveItemsReachTheOwnerAndAnAdmittedGuest(t *testing.T) {
	e := pcEnv(t, false)
	c := e.meeting(ada)
	e.started(c)
	tk := e.knock(c.Code, grace)
	e.admit(c, grace)
	guest := e.guestEvents(c.Code, tk)
	owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	e.clock.Add(10 * time.Second)
	id := e.say(c, store.OwnerIdentity, "Ada L.", 1_000, 3_000, "I'll send the deck by Friday.")
	e.server.LiveTick(context.Background())
	for name, s := range map[string]*stream{"owner": owner, "guest": guest} {
		v := itemsOf(t, s)
		if len(v.Items) != 1 || v.Items[0].Text != "Send the deck by Friday" || v.Items[0].Assignee != "Ada L." || v.Items[0].Segments[0] != id || v.Paused {
			t.Errorf("%s's items = %+v", name, v)
		}
	}
	if n := e.or.Count(openroutertest.KindItems); n != 1 {
		t.Fatalf("%d item calls, want 1", n)
	}
	body := string(e.or.Requests()[0].Body)
	if !strings.Contains(body, `"provider":{"allow_fallbacks":false,"data_collection":"deny","order":["deepinfra/fp8"]}`) || !strings.Contains(body, `"model":"`+itemsModel+`"`) {
		t.Errorf("the request's model or provider pin: %s", body)
	}
	// No new line: no call. The switch off (a new meeting, switched off
	// before anyone knocked): no call either.
	e.server.LiveTick(context.Background())
	c2 := e.meeting(bea)
	e.started(c2)
	if resp, raw := e.do("POST", "/api/meetings/"+c2.ID+"/ai", c2.Sess, map[string]bool{"on": false}); resp.StatusCode != 200 {
		t.Fatalf("ai off = %d %s", resp.StatusCode, raw)
	}
	e.say(c2, store.OwnerIdentity, "Bea", 1_000, 2_000, "I'll book the room.")
	e.clock.Add(10 * time.Second)
	e.server.LiveTick(context.Background())
	if n := e.or.Count(openroutertest.KindItems); n != 1 {
		t.Errorf("%d item calls after a tick with nothing new and a meeting switched off, want still 1", n)
	}
}

// 🔴 "Action items paused" after 3 failures in a row, on both streams.
func TestLiveItemsPauseAfterThreeFailures(t *testing.T) {
	e := pcEnv(t, false)
	e.or.Script(openroutertest.KindItems, func(openroutertest.Doc) openroutertest.Reply { return openroutertest.Reply{Status: 503} })
	c := e.meeting(ada)
	e.started(c)
	owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	e.say(c, store.OwnerIdentity, "Ada L.", 1_000, 3_000, "I'll send the deck.")
	for i := 0; i < 3; i++ {
		e.clock.Add(time.Minute) // past each backoff
		e.server.LiveTick(context.Background())
	}
	if v := itemsOf(t, owner); !v.Paused || len(v.Items) != 0 {
		t.Errorf("items after 3 failures = %+v, want paused", v)
	}
	if n := e.or.Count(openroutertest.KindItems); n != 3 {
		t.Errorf("%d calls, want 3", n)
	}
}

// cardFrom has name knock, be admitted, see the meeting end and send the
// card with addr; it answers the ended event's card.
func (e *pc) cardFrom(c call, name, addr string, end bool) {
	e.t.Helper()
	_, s := e.inCall(c, name)
	if end {
		e.end(c)
	}
	ev := endedOf(e.t, s)
	if ev.Card == nil {
		e.t.Fatalf("%s got no card", name)
	}
	if resp, raw := e.do("POST", "/card", "", map[string]string{"id": ev.Card.ID, "name": name, "email": addr}); resp.StatusCode != 200 {
		e.t.Fatalf("card = %d %s", resp.StatusCode, raw)
	}
}

type analysisAns struct {
	Runs []struct {
		ID          int64  `json:"id"`
		Kind        string `json:"kind"`
		State       string `json:"state"`
		Current     bool   `json:"current"`
		ItemsModel  string `json:"items_model"`
		ItemsPrefs  string `json:"items_prefs"`
		VisionModel string `json:"vision_model"`
	} `json:"runs"`
	Current *int64 `json:"current"`
	Output  *struct {
		Items []struct {
			Source, Text, Assignee string
			Segments               []int64
			Frames                 []int64
		} `json:"items"`
		Notes []struct {
			FrameMS  int64   `json:"frame_ms"`
			Text     string  `json:"text"`
			Segments []int64 `json:"segments"`
		} `json:"notes"`
		Summary *struct {
			Points []struct{ Text string } `json:"points"`
		} `json:"summary"`
	} `json:"output"`
}

func (e *pc) analysis(c call, q string) analysisAns {
	e.t.Helper()
	resp, raw := e.do("GET", "/api/meetings/"+c.ID+"/analysis"+q, c.Sess, nil)
	var a analysisAns
	if resp.StatusCode != 200 || json.Unmarshal(raw, &a) != nil {
		e.t.Fatalf("analysis = %d %s", resp.StatusCode, raw)
	}
	return a
}

// postDone waits for the meeting's n-th post run to finish.
func (e *pc) postDone(c call, n int) analysisAns {
	e.t.Helper()
	var a analysisAns
	eventually(e.t, 10*time.Second, "post run done", func() bool {
		a = e.analysis(c, "")
		done := 0
		for _, r := range a.Runs {
			if r.Kind == "post" && r.State == "done" {
				done++
			}
		}
		return done >= n
	})
	return a
}

type draftAns struct {
	ID        string `json:"id"`
	RunID     int64  `json:"run_id"`
	Recipient string `json:"recipient"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	State     string `json:"state"`
	Items     []struct {
		Text, Assignee, Due string
	} `json:"items"`
}

func (e *pc) drafts(c call) []draftAns {
	e.t.Helper()
	resp, raw := e.do("GET", "/api/meetings/"+c.ID+"/drafts", c.Sess, nil)
	var v struct {
		Drafts []draftAns `json:"drafts"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(raw, &v) != nil {
		e.t.Fatalf("drafts = %d %s", resp.StatusCode, raw)
	}
	return v.Drafts
}

// fixture is the meeting the mail tests read: Ada (the owner) and Grace (a
// guest who sends the card) each take one item; Linus (a guest) skips the
// card and gets nothing.
func (e *pc) fixture() call {
	e.t.Helper()
	c := e.meeting(ada)
	e.started(c)
	e.say(c, store.OwnerIdentity, "Ada L.", 1_000, 3_000, "I'll book the caterer.")
	e.say(c, "guest-grace", grace+" (guest)", 4_000, 6_000, "I'll send the venue shortlist.")
	_, ls := e.inCall(c, linus)
	e.cardFrom(c, grace, graceAddr, true)
	if ev := endedOf(e.t, ls); ev.Card != nil {
		if resp, _ := e.do("POST", "/card/skip", "", map[string]string{"id": ev.Card.ID}); resp.StatusCode != 204 {
			e.t.Fatalf("skip = %d", resp.StatusCode)
		}
	}
	return c
}

func (e *pc) confirmOwner(c call) {
	e.t.Helper()
	resp, raw := e.do("PUT", "/api/settings/email", c.Sess, map[string]string{"email": adaAddr})
	if resp.StatusCode != 202 {
		e.t.Fatalf("set email = %d %s", resp.StatusCode, raw)
	}
	if !e.smtp.Wait(5 * time.Second) {
		e.t.Fatal("no confirmation email")
	}
	msgs := e.smtp.Messages()
	link := regexp.MustCompile(regexp.QuoteMeta(publicURL) + `(/e/[A-Za-z0-9_-]{43})`).FindStringSubmatch(decodedText(e.t, msgs[len(msgs)-1].Data))
	if link == nil {
		e.t.Fatalf("no link in the confirmation:\n%s", msgs[len(msgs)-1].Data)
	}
	if resp, _ := e.do("GET", link[1], "", nil); resp.StatusCode != 200 {
		e.t.Fatalf("confirm page = %d", resp.StatusCode)
	}
	if resp, body := e.do("POST", link[1], "", nil); resp.StatusCode != 200 || !strings.Contains(string(body), "Confirmed.") {
		e.t.Fatalf("confirm = %d %s", resp.StatusCode, body)
	}
}

// decodedText is a message's text/plain part, decoded.
func decodedText(t *testing.T, raw []byte) string {
	t.Helper()
	for _, p := range parts(t, raw) {
		if p.ctype == "text/plain" && p.name == "" {
			return p.body
		}
	}
	return ""
}

type mimePart struct{ ctype, name, body string }

// parts is a message's leaves in order (alternative's parts flattened).
func parts(t *testing.T, raw []byte) []mimePart {
	t.Helper()
	m, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var out []mimePart
	var walk func(ctype string, r io.Reader)
	walk = func(ctype string, r io.Reader) {
		mt, p, _ := mime.ParseMediaType(ctype)
		mr := multipart.NewReader(r, p["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				return
			}
			ct := part.Header.Get("Content-Type")
			pt, _, _ := mime.ParseMediaType(ct)
			if strings.HasPrefix(pt, "multipart/") {
				walk(ct, part)
				continue
			}
			_, dp, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
			body, _ := io.ReadAll(part)
			if part.Header.Get("Content-Transfer-Encoding") == "base64" {
				body = []byte(decodeB64(string(body)))
			}
			out = append(out, mimePart{pt + "@" + mt, dp["filename"], string(body)})
		}
	}
	walk(m.Header.Get("Content-Type"), m.Body)
	for i := range out {
		out[i].ctype = out[i].ctype[:strings.Index(out[i].ctype, "@")]
	}
	return out
}

func TestEndRunsThePostCallPassAndDraftsTheOwnerAndTheCardGuest(t *testing.T) {
	e := pcEnv(t, false)
	c := e.fixture()
	a := e.postDone(c, 1)
	var post int64
	for _, r := range a.Runs {
		if r.Kind == "post" {
			post = r.ID
			if !r.Current || r.ItemsModel != itemsModel || r.ItemsPrefs != `{"allow_fallbacks":false,"data_collection":"deny","order":["deepinfra/fp8"]}` || r.VisionModel != "" {
				t.Errorf("post run = %+v (no bucket: no vision model)", r)
			}
		}
	}
	if a.Output == nil || len(a.Output.Items) != 2 || a.Output.Items[0].Assignee != "Ada L." || a.Output.Items[1].Assignee != grace || a.Output.Items[1].Source != "transcript" {
		t.Fatalf("output = %+v", a.Output)
	}
	if a.Output.Summary == nil || len(a.Output.Summary.Points) != 2 {
		t.Errorf("summary = %+v", a.Output.Summary)
	}
	ds := e.drafts(c)
	var got []string
	for _, d := range ds {
		got = append(got, d.Recipient+"|"+d.Name+"|"+d.Email+"|"+d.State)
		if d.RunID != post || len(d.Items) != 2 {
			t.Errorf("draft %+v", d)
		}
	}
	if len(ds) != 2 || got[0] != "owner|Ada L.||draft" || !strings.HasSuffix(got[1], "|"+grace+"|"+graceAddr+"|draft") {
		t.Errorf("drafts = %v: want the owner and Grace (Linus skipped the card)", got)
	}
	// 🔴 Nothing is sent without Send, even with the owner's address
	// confirmed and the outbox run.
	e.confirmOwner(c)
	before := len(e.smtp.Messages())
	e.server.MailPass(context.Background())
	time.Sleep(50 * time.Millisecond)
	if n := len(e.smtp.Messages()) - before; n != 0 {
		t.Errorf("%d messages sent with no Send", n)
	}
}

// 🔴 Send refuses until the owner's address is confirmed; nothing is sent
// without Send.
func TestSendNeedsTheOwnersConfirmedAddress(t *testing.T) {
	e := pcEnv(t, false)
	c := e.fixture()
	e.postDone(c, 1)
	resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/drafts/send", c.Sess, map[string]any{})
	if resp.StatusCode != 409 || !strings.Contains(string(raw), server.NotesNeedAddress) {
		t.Fatalf("send with no address = %d %s (an unconfirmed owner address was used)", resp.StatusCode, raw)
	}
	if resp, _ := e.do("PUT", "/api/settings/email", c.Sess, map[string]string{"email": adaAddr}); resp.StatusCode != 202 {
		t.Fatal(resp.StatusCode)
	}
	e.smtp.Wait(5 * time.Second)
	resp, raw = e.do("POST", "/api/meetings/"+c.ID+"/drafts/send", c.Sess, map[string]any{})
	if resp.StatusCode != 409 {
		t.Fatalf("send with the address only pending = %d %s (an unconfirmed owner address was used)", resp.StatusCode, raw)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(e.smtp.Messages()); n != 1 {
		t.Errorf("%d messages: want only the confirmation", n)
	}
}

// 🔴 The exact message each recipient gets: multipart/mixed holding the
// alternative (text, then HTML) and the two transcripts; the headers;
// Reply-To the owner's confirmed address; and the recipient's OWN items in
// the styled block, everyone else's muted.
func TestSendMailsEachRecipientTheirNotesExactly(t *testing.T) {
	e := pcEnv(t, false)
	c := e.fixture()
	e.postDone(c, 1)
	e.confirmOwner(c)
	before := len(e.smtp.Messages())
	resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/drafts/send", c.Sess, map[string]any{})
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"queued":2`) {
		t.Fatalf("send = %d %s", resp.StatusCode, raw)
	}
	eventually(t, 10*time.Second, "two notes sent", func() bool { return len(e.smtp.Messages()) == before+2 })
	byTo := map[string]mailtest.Message{}
	for _, m := range e.smtp.Messages()[before:] {
		byTo[m.To[0]] = m
		if m.From != mailFrom || !m.TLS {
			t.Errorf("envelope from %q, TLS %v", m.From, m.TLS)
		}
	}
	ds := e.drafts(c)
	for _, want := range []struct {
		to, name, yours, others, id string
	}{
		{adaAddr, "Ada L.", "Book the caterer", "Send the venue shortlist", ds[0].ID},
		{graceAddr, grace, "Send the venue shortlist", "Book the caterer", ds[1].ID},
	} {
		m, ok := byTo[want.to]
		if !ok {
			t.Fatalf("nothing to %s", want.to)
		}
		h, _ := netmail.ReadMessage(bytes.NewReader(m.Data))
		hdr := func(k string) string { return h.Header.Get(k) }
		if hdr("From") != `"Ada L. via Meetings" <`+mailFrom+`>` || hdr("Reply-To") != "<"+adaAddr+">" ||
			hdr("To") != `"`+want.name+`" <`+want.to+`>` || hdr("Message-Id") != "<"+want.id+"@mail.example.test>" ||
			hdr("Subject") != "Notes: the meeting on 14 Mar 2031" || hdr("Date") == "" || hdr("Auto-Submitted") != "" || hdr("List-Unsubscribe") != "" {
			t.Errorf("headers to %s: %v", want.to, h.Header)
		}
		if mt, _, _ := mime.ParseMediaType(hdr("Content-Type")); mt != "multipart/mixed" {
			t.Errorf("top type %s", mt)
		}
		ps := parts(t, m.Data)
		var shape []string
		for _, p := range ps {
			shape = append(shape, p.ctype+":"+p.name)
		}
		if strings.Join(shape, " ") != "text/plain: text/html: text/plain:meeting-"+c.ID+".txt text/vtt:meeting-"+c.ID+".vtt" {
			t.Errorf("shape to %s: %v", want.to, shape)
		}
		html := ps[1].body
		yours, others := cut(html, `id="yours"`, "</table>"), cut(html, `id="others"`, "</div>")
		if !strings.Contains(yours, want.yours) || strings.Contains(yours, want.others) || !strings.Contains(yours, "border-left:4px solid #2f6fdf") {
			t.Errorf("to %s the YOUR section is not their own items alone:\n%s", want.to, yours)
		}
		if !strings.Contains(others, want.others) || strings.Contains(others, want.yours) || !strings.Contains(others, "color:#6e7781") {
			t.Errorf("to %s the muted section:\n%s", want.to, others)
		}
		if !strings.Contains(ps[0].body, "YOUR ACTION ITEMS\r\n  * "+want.yours+"\r\n") {
			t.Errorf("to %s the text part:\n%s", want.to, ps[0].body)
		}
		if !strings.Contains(ps[2].body, "«Ada L.»: I'll book the caterer.") || !strings.HasPrefix(ps[3].body, "WEBVTT") {
			t.Errorf("attachments to %s: %q / %q", want.to, ps[2].body, ps[3].body)
		}
		if strings.Contains(string(m.Data), "/r/") {
			t.Errorf("a video link with no recording")
		}
	}
	// Sent: the guest's address is gone, the drafts locked, a second Send
	// queues nothing.
	eventually(t, 5*time.Second, "both sent", func() bool {
		ds := e.drafts(c)
		return ds[0].State == "sent" && ds[1].State == "sent"
	})
	if ds := e.drafts(c); ds[1].Email != "" {
		t.Errorf("a sent draft kept the address")
	}
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/drafts/send", c.Sess, map[string]any{}); !strings.Contains(string(raw), `"queued":0`) {
		t.Errorf("a second send = %d %s", resp.StatusCode, raw)
	}
	if resp, _ := e.do("PATCH", "/api/meetings/"+c.ID+"/drafts/"+ds[1].ID, c.Sess, map[string]any{"removed": true}); resp.StatusCode != 409 {
		t.Errorf("editing a sent draft = %d, want 409", resp.StatusCode)
	}
}

// The owner edits a recipient's items (an assignee changed, an item
// removed) and removes another recipient: what is sent is what they left.
func TestEditedDraftsAreWhatIsSent(t *testing.T) {
	e := pcEnv(t, false)
	c := e.fixture()
	e.postDone(c, 1)
	e.confirmOwner(c)
	ds := e.drafts(c)
	resp, raw := e.do("PATCH", "/api/meetings/"+c.ID+"/drafts/"+ds[1].ID, c.Sess, map[string]any{"items": []map[string]string{
		{"text": "Send the venue shortlist", "assignee": "Ada L.", "due": "Monday"}}})
	if resp.StatusCode != 200 {
		t.Fatalf("edit = %d %s", resp.StatusCode, raw)
	}
	if resp, _ := e.do("PATCH", "/api/meetings/"+c.ID+"/drafts/"+ds[0].ID, c.Sess, map[string]any{"removed": true}); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	resp, raw = e.do("GET", "/api/meetings/"+c.ID+"/drafts/"+ds[1].ID+"/preview", c.Sess, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"yours":[]`) || !strings.Contains(string(raw), `"others":[{"text":"Send the venue shortlist","assignee":"Ada L.","due":"Monday"}]`) {
		t.Fatalf("preview = %d %s", resp.StatusCode, raw)
	}
	before := len(e.smtp.Messages())
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/drafts/send", c.Sess, map[string]any{}); !strings.Contains(string(raw), `"queued":1`) {
		t.Fatalf("send = %d %s", resp.StatusCode, raw)
	}
	eventually(t, 10*time.Second, "one sent", func() bool { return len(e.smtp.Messages()) == before+1 })
	time.Sleep(50 * time.Millisecond)
	m := e.smtp.Messages()[before]
	if m.To[0] != graceAddr || len(e.smtp.Messages()) != before+1 {
		t.Fatalf("sent to %v (%d), want Grace alone", m.To, len(e.smtp.Messages())-before)
	}
	text := decodedText(t, m.Data)
	if !strings.Contains(text, "You have no action items from this meeting.") || !strings.Contains(text, "  - Ada L.: Send the venue shortlist (due Monday)") {
		t.Errorf("text:\n%s", text)
	}
}

// 🔴 The confirmation door: an unknown token, and the real one once
// expired, are the same 404 by GET and POST, byte for byte.
func TestTheConfirmationDoorAnswersOneNotFound(t *testing.T) {
	e := pcEnv(t, false)
	c := e.meeting(ada)
	if resp, _ := e.do("PUT", "/api/settings/email", c.Sess, map[string]string{"email": adaAddr}); resp.StatusCode != 202 {
		t.Fatal(resp.StatusCode)
	}
	e.smtp.Wait(5 * time.Second)
	link := regexp.MustCompile(`/e/[A-Za-z0-9_-]{43}`).FindString(decodedText(t, e.smtp.Messages()[0].Data))
	unknown := "/e/" + strings.Repeat("A", 43)
	var answers []answer
	for _, m := range []string{"GET", "POST"} {
		for _, p := range []string{unknown, "/e/x", "/e/" + strings.Repeat("A", 44)} {
			resp, raw := e.do(m, p, "", nil)
			answers = append(answers, answerOf(resp, raw))
		}
	}
	e.clock.Add(store.EmailTokenLifetime)
	for _, m := range []string{"GET", "POST"} {
		resp, raw := e.do(m, link, "", nil)
		answers = append(answers, answerOf(resp, raw))
	}
	for i, a := range answers {
		if a.Status != 404 || a.Body != answers[0].Body || a.Headers["Content-Security-Policy"] != answers[0].Headers["Content-Security-Policy"] {
			t.Errorf("answer %d = %d %v, want the first's 404", i, a.Status, a.Headers)
		}
	}
	if _, raw := e.do("GET", "/api/settings/email", e.enter(ada), nil); strings.TrimSpace(string(raw)) != `{"confirmed":"","pending":""}` {
		em := string(raw)
		t.Errorf("after an expired link the address = %s", em)
	}
}

// 🔴 Reprocess: a new run under another model becomes current, the sent
// draft is untouched, the unsent are redrafted (and the answer says how
// many were left alone), and the older run can be made current again.
func TestReprocessMakesANewCurrentRunAndLeavesSentNotesAlone(t *testing.T) {
	e := pcEnv(t, false)
	c := e.fixture()
	first := e.postDone(c, 1)
	e.confirmOwner(c)
	ds := e.drafts(c)
	// The owner's notes are sent (the owner is a recipient of every run);
	// Grace's are not.
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/drafts/send", c.Sess, map[string]any{"ids": []string{ds[0].ID}}); resp.StatusCode != 200 {
		t.Fatalf("send = %d %s", resp.StatusCode, raw)
	}
	eventually(t, 10*time.Second, "the owner's sent", func() bool { return e.drafts(c)[0].State == "sent" })
	sentBefore := e.drafts(c)[0]
	e.or.Script(openroutertest.KindItems, func(d openroutertest.Doc) openroutertest.Reply {
		return openroutertest.Reply{Content: openroutertest.JSON(map[string]any{"items": []any{map[string]any{"task": "Confirm the date", "owner": "Ada L.", "due": "", "segment_ids": []int64{d.Segments[0].ID}}}})}
	})
	resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/reprocess", c.Sess, map[string]any{"items_model": "z-ai/glm-5-20261001", "items_provider_prefs": json.RawMessage(`{"order":["z-ai"],"allow_fallbacks":false}`)})
	if resp.StatusCode != 202 {
		t.Fatalf("reprocess = %d %s", resp.StatusCode, raw)
	}
	second := e.postDone(c, 2)
	if second.Current == nil || *second.Current == *first.Current || second.Output.Items[0].Text != "Confirm the date" {
		t.Fatalf("after reprocess: current %v (was %v), output %+v", second.Current, *first.Current, second.Output)
	}
	if e.or.Requests()[len(e.or.Requests())-2].Model != "z-ai/glm-5-20261001" {
		t.Errorf("the reprocess did not use its model")
	}
	after := e.drafts(c)
	if after[0].ID != sentBefore.ID || after[0].State != "sent" || after[0].Items[0].Text != sentBefore.Items[0].Text || after[0].RunID != sentBefore.RunID {
		t.Errorf("a sent draft was changed by reprocess: %+v → %+v", sentBefore, after[0])
	}
	if len(after) != 2 || after[1].RunID != *second.Current || after[1].Items[0].Text != "Confirm the date" || after[1].State != "draft" {
		t.Errorf("Grace's unsent draft was not redrafted: %+v", after)
	}
	old := e.analysis(c, "?run="+i64s(*first.Current))
	if old.Output == nil || len(old.Output.Items) != 2 {
		t.Errorf("the older run is not viewable: %+v", old.Output)
	}
	resp, raw = e.do("POST", "/api/meetings/"+c.ID+"/analysis/runs/"+i64s(*first.Current)+"/current", c.Sess, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"drafts":{"made":1,"already_sent":1}`) {
		t.Fatalf("revert = %d %s", resp.StatusCode, raw)
	}
	if a := e.analysis(c, ""); *a.Current != *first.Current || e.drafts(c)[1].Items[0].Text != "Book the caterer" {
		t.Errorf("after revert: current %d", *a.Current)
	}
	// A prefs object that would let a provider keep the transcript is refused.
	resp, raw = e.do("POST", "/api/meetings/"+c.ID+"/reprocess", c.Sess, map[string]any{"items_model": "z-ai/glm-5-20261001", "items_provider_prefs": json.RawMessage(`{"data_collection":"allow","order":["z-ai"],"allow_fallbacks":false}`)})
	if resp.StatusCode != 400 {
		t.Errorf("reprocess with data_collection allow = %d %s", resp.StatusCode, raw)
	}
}

// cut is s from start to the first end after it, "" when start is not in s.
func cut(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	s = s[i:]
	if j := strings.Index(s, end); j >= 0 {
		return s[:j]
	}
	return s
}

func i64s(v int64) string { b, _ := json.Marshal(v); return string(b) }

// 🔴 Every new owner route is the owner's alone: another owner's session is
// the meeting's 404, no session is 401.
func TestEveryNewRouteIsTheOwnersAlone(t *testing.T) {
	e := pcEnv(t, false)
	c := e.fixture()
	e.postDone(c, 1)
	ds := e.drafts(c)
	other := e.enter(bea)
	for _, p := range []struct{ method, path string }{
		{"GET", "/api/meetings/" + c.ID + "/analysis"},
		{"POST", "/api/meetings/" + c.ID + "/reprocess"},
		{"POST", "/api/meetings/" + c.ID + "/analysis/runs/1/current"},
		{"POST", "/api/meetings/" + c.ID + "/agent-token"},
		{"GET", "/api/meetings/" + c.ID + "/drafts"},
		{"GET", "/api/meetings/" + c.ID + "/drafts/" + ds[0].ID + "/preview"},
		{"PATCH", "/api/meetings/" + c.ID + "/drafts/" + ds[0].ID},
		{"POST", "/api/meetings/" + c.ID + "/drafts/send"},
		{"POST", "/api/meetings/" + c.ID + "/drafts/" + ds[0].ID + "/retry"},
	} {
		var body any
		if p.method != "GET" {
			body = map[string]any{"removed": true}
		}
		if resp, raw := e.do(p.method, p.path, other, body); resp.StatusCode != 404 {
			t.Errorf("%s %s as another owner = %d %s", p.method, p.path, resp.StatusCode, raw)
		}
		if resp, _ := e.do(p.method, p.path, "", body); resp.StatusCode != 401 {
			t.Errorf("%s %s with no session = %d", p.method, p.path, resp.StatusCode)
		}
	}
	for _, p := range []string{"/api/settings/email"} {
		if resp, _ := e.do("GET", p, "", nil); resp.StatusCode != 401 {
			t.Errorf("GET %s with no session = %d", p, resp.StatusCode)
		}
	}
	if d := e.drafts(c); d[0].State != "draft" {
		t.Errorf("another owner changed a draft: %+v", d[0])
	}
}

// The agent's read path: an owner-minted token reads that meeting's
// timeline with its analysis, and nothing else.
func TestAnAgentTokenReadsOneMeetingsTimeline(t *testing.T) {
	e := pcEnv(t, false)
	c := e.fixture()
	e.postDone(c, 1)
	resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/agent-token", c.Sess, nil)
	var tk struct {
		Token string `json:"token"`
		URL   string `json:"timeline_url"`
	}
	if resp.StatusCode != 201 || json.Unmarshal(raw, &tk) != nil || !strings.HasPrefix(tk.Token, "mmr_") || tk.URL != publicURL+"/api/agent/meetings/"+c.ID+"/timeline" {
		t.Fatalf("agent token = %d %s", resp.StatusCode, raw)
	}
	resp, raw = e.do("GET", "/api/agent/meetings/"+c.ID+"/timeline", tk.Token, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"analysis":{"run":{`) || !strings.Contains(string(raw), "Send the venue shortlist") {
		t.Fatalf("agent timeline = %d %s", resp.StatusCode, raw)
	}
	other := e.meeting(ada)
	for _, a := range []struct{ path, tok string }{
		{"/api/agent/meetings/" + other.ID + "/timeline", tk.Token},
		{"/api/agent/meetings/" + c.ID + "/timeline", "mmr_" + strings.Repeat("A", 43)},
		{"/api/agent/meetings/" + c.ID + "/timeline", c.Sess},
		{"/api/agent/meetings/" + c.ID + "/timeline", ""},
	} {
		if resp, _ := e.do("GET", a.path, a.tok, nil); resp.StatusCode != 404 {
			t.Errorf("agent %s with %.8s… = %d, want 404", a.path, a.tok, resp.StatusCode)
		}
	}
	e.clock.Add(store.AgentTokenLifetime)
	if resp, _ := e.do("GET", "/api/agent/meetings/"+c.ID+"/timeline", tk.Token, nil); resp.StatusCode != 404 {
		t.Errorf("an expired agent token = %d", resp.StatusCode)
	}
}

// 🔴 Frames + transcript: frames of a shared screen at the moments a line
// was said over it, sent with that excerpt to the vision model; the notes
// cite the frame (a note citing a frame not sent is dropped), and the
// bundle carries them.
func TestTheFrameAnalysisSendsSharedScreenFramesWithTheExcerpt(t *testing.T) {
	e := pcEnv(t, true)
	if !e.server.FramesAvailableForTest() {
		if os.Getenv("MEETINGS_REQUIRE_FFMPEG") == "1" {
			t.Fatal("no ffmpeg (MEETINGS_REQUIRE_FFMPEG=1)")
		}
		t.Skip("no ffmpeg")
	}
	e.or.Script(openroutertest.KindVision, func(d openroutertest.Doc) openroutertest.Reply {
		return openroutertest.Reply{Content: openroutertest.JSON(map[string]any{
			"frames": []any{
				map[string]any{"frame_ms": d.Frames[0], "note": "The slide shows ticket MEET-42.", "segment_ids": []int64{d.Segments[0].ID}},
				map[string]any{"frame_ms": 999_999, "note": "a frame never sent", "segment_ids": []int64{}},
			},
			"items": []any{map[string]any{"task": "Close ticket MEET-42", "owner": "Ada L.", "due": "", "segment_ids": []int64{d.Segments[0].ID}, "frame_ms": []int64{d.Frames[0]}},
				map[string]any{"task": "An item citing no frame", "owner": "Ada L.", "due": "", "segment_ids": []int64{d.Segments[0].ID}, "frame_ms": []int64{}}},
		})}
	})
	c := e.meeting(ada)
	e.started(c)
	e.admitGuest(c, grace)
	g := e.composite(1)
	e.lk.BeginComposite(g.ID, t0.Add(1000*time.Millisecond))
	e.clock.Add(4 * time.Second)
	scr := e.publish(c, store.OwnerIdentity, "SCREEN_SHARE")
	e.say(c, store.OwnerIdentity, "Ada L.", 5_000, 7_000, "On this slide, I'll close the ticket.")
	e.clock.Add(6 * time.Second)
	e.lk.Unpublish(c.Room, store.OwnerIdentity, scr)
	e.clock.Add(5 * time.Second)
	e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil)
	a := e.postDone(c, 1)
	if n := e.or.Count(openroutertest.KindVision); n != 1 {
		t.Fatalf("%d vision calls, want 1", n)
	}
	var vreq openroutertest.Request
	for _, r := range e.or.Requests() {
		if r.Kind == openroutertest.KindVision {
			vreq = r
		}
	}
	if vreq.Images != 1 || vreq.Model != visionModel || !strings.Contains(string(vreq.Body), `"provider":{"allow_fallbacks":false,"data_collection":"deny","order":["anthropic"]}`) ||
		!strings.Contains(string(vreq.Body), `\"frames\":[6000]`) || !strings.Contains(string(vreq.Body), "data:image/jpeg;base64,/9j/") {
		t.Errorf("the vision request: %d images, model %s: %.400s", vreq.Images, vreq.Model, vreq.Body)
	}
	if a.Output == nil || len(a.Output.Notes) != 1 || a.Output.Notes[0].FrameMS != 6000 || a.Output.Notes[0].Text != "The slide shows ticket MEET-42." {
		t.Errorf("notes = %+v (the note citing a frame never sent must be dropped)", a.Output)
	}
	// The transcript's item ("Close the ticket"), refined by the frame
	// (same line cited): the screen's wording and its frame; the item citing
	// no frame is dropped.
	var got []string
	for _, it := range a.Output.Items {
		got = append(got, it.Source+":"+it.Text+":"+i64s(int64(len(it.Frames))))
	}
	if strings.Join(got, "|") != "transcript:Close ticket MEET-42:1" || a.Output.Items[0].Frames[0] != 6000 {
		t.Errorf("items = %v", got)
	}
	resp, raw := e.do("GET", "/api/meetings/"+c.ID+"/timeline", c.Sess, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"notes":[{"frame_ms":6000,"text":"The slide shows ticket MEET-42."`) {
		t.Errorf("timeline = %d %.600s", resp.StatusCode, raw)
	}
}

// 🔴 No log line carries what was said, an item, a note, a name typed or an
// address: the whole flow above, then the log is searched.
func TestTheLogCarriesNoTranscriptItemNameOrAddress(t *testing.T) {
	e := pcEnv(t, false)
	c := e.fixture()
	e.postDone(c, 1)
	e.confirmOwner(c)
	e.do("POST", "/api/meetings/"+c.ID+"/drafts/send", c.Sess, map[string]any{})
	eventually(t, 10*time.Second, "sent", func() bool { return e.drafts(c)[1].State == "sent" })
	logged := e.log.String()
	if !strings.Contains(logged, "postcall: meeting "+c.ID) || !strings.Contains(logged, "notes: draft ") {
		t.Fatalf("the positive control: the post run's and the outbox's lines are missing:\n%s", logged)
	}
	for _, secret := range []string{"book the caterer", "Book the caterer", "venue shortlist", grace, linus, graceAddr, adaAddr, "or-fixture-not-a-key"} {
		if strings.Contains(logged, secret) {
			t.Errorf("the log carries %q", secret)
		}
	}
}

func decodeB64(s string) string {
	b, _ := base64.StdEncoding.DecodeString(strings.NewReplacer("\r", "", "\n", "").Replace(s))
	return string(b)
}
