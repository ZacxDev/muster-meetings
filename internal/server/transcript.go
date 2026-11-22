package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/ZacxDev/muster-meetings/internal/store"
)

// The transcript's routes and events (design: "The plugin's HTTP API"):
//
//	GET /api/meetings/{id}/transcript?format=json|txt|vtt   the owner's transcript (the archive view, and its downloads)
//
// and, on the owner's event stream and an admitted guest's, the events
//
//	segment     {"id", "speaker", "kind", "start_ms", "end_ms", "text"}: a stored line, or a gap
//	            marker (kind "gap", end_ms null while it is open, sent again when it closes)
//	amend       {"id", "text"}: a line already sent, its text amended in place (the nemotron
//	            engine's late closing mark, stt.Segment.Lead): the panel replaces that line's text
//	partials    {"partials": [{"speaker", "start_ms", "text"}]}: the lines in progress, never stored
//	transcript  {"status"}: "" or "Transcript paused"
//
// 🔴 A line's text and its speaker's name are text a stranger may have
// produced (a guest said it, or typed it). The pages render both with
// textContent only; the txt and vtt exports put each on one line and escape
// what their format would read as markup (oneLine, vttEscape), so no guest
// can forge another line, a cue or a speaker; and nothing here logs either.

// tcursor is what one event stream has sent of a meeting's transcript.
type tcursor struct {
	last     int64          // the highest segment id sent
	open     map[int64]bool // the gaps sent while open
	partials string
	status   string
	primed   bool
	// amendSeq is the latest amend (Server.noteAmend) this stream has
	// accounted for; amendInit says it was set, at the stream's first send
	// (amends before then are in the lines it reads).
	amendSeq  int64
	amendInit bool
}

func newTCursor() *tcursor { return &tcursor{open: map[int64]bool{}} }

// sendTranscript sends what a stream has not yet sent of the meeting's
// transcript: new segments (read through after, which binds them to the
// stream's meeting and, for the owner, to the owner), gaps that closed, the
// partials when they changed, and the status when it changed.
func (s *Server) sendTranscript(ctx context.Context, w io.Writer, f http.Flusher, meetingID string, cur *tcursor,
	after func(int64) ([]store.Segment, error)) error {
	// At a stream's first send: amends numbered up to now are already in
	// every line it reads (the store is written before an amend is noted).
	if !cur.amendInit {
		cur.amendSeq, cur.amendInit = s.amendSeqNow(), true
	}
	// The open gaps are read FIRST, and serve both the closed-gap updates
	// and the status: every gap in it was stored before the segments are
	// read, so a stream never says "Transcript paused" about a gap it has
	// not sent, nor "" before it has sent the gap's close.
	open, err := s.store.OpenGaps(ctx, meetingID)
	if err != nil {
		return err
	}
	// Only gaps sent open by EARLIER sends are judged by that read: one sent
	// open just now may have been stored after it.
	var judged []int64
	for id := range cur.open {
		judged = append(judged, id)
	}
	for {
		segs, err := after(cur.last)
		if err != nil {
			return err
		}
		for _, g := range segs {
			if err := sse(w, f, "segment", g); err != nil {
				return err
			}
			cur.last = g.ID
			if g.Kind == store.SegGap && g.EndMS == nil {
				cur.open[g.ID] = true
			}
		}
		if len(segs) < store.MaxSegmentsPage {
			break
		}
	}
	if len(judged) > 0 {
		still := map[int64]bool{}
		for _, id := range open {
			still[id] = true
		}
		var closed []int64
		for _, id := range judged {
			if !still[id] {
				closed = append(closed, id)
			}
		}
		if len(closed) > 0 {
			segs, err := s.store.SegmentsByID(ctx, meetingID, closed)
			if err != nil {
				return err
			}
			for _, g := range segs {
				if err := sse(w, f, "segment", g); err != nil {
					return err
				}
				delete(cur.open, g.ID)
			}
		}
	}
	// Lines amended in place (a late closing mark) that this stream has
	// already sent: their new text, as `amend`. Read AFTER the new lines, so
	// a line stored and amended meanwhile is either read with its new text
	// above or re-sent here; one not yet sent is left to be read in full.
	if ids, seq := s.amendsSince(meetingID, cur.amendSeq); seq != cur.amendSeq {
		var sent []int64
		for _, id := range ids {
			if id <= cur.last {
				sent = append(sent, id)
			}
		}
		segs, err := s.store.SegmentsByID(ctx, meetingID, sent)
		if err != nil {
			return err
		}
		for _, g := range segs {
			if g.Kind != store.SegText {
				continue
			}
			if err := sse(w, f, "amend", map[string]any{"id": g.ID, "text": g.Text}); err != nil {
				return err
			}
		}
		cur.amendSeq = seq
	}
	p := s.partialsOf(meetingID)
	if b := mustJSON(p); b != cur.partials {
		cur.partials = b
		if err := sse(w, f, "partials", map[string]any{"partials": p}); err != nil {
			return err
		}
	}
	if st := s.transcriptStatus(meetingID, open); st != cur.status || !cur.primed {
		cur.status, cur.primed = st, true
		if err := sse(w, f, "transcript", map[string]any{"status": st}); err != nil {
			return err
		}
	}
	return nil
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// handleTranscript is the owner's transcript of one of their meetings, live
// or ended: JSON for the archive view, or a download as plain text or
// WebVTT. Another person's meeting is the same 404 as none.
func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "txt" && format != "vtt" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "format is json, txt or vtt"})
		return
	}
	m, segs, err := repo.Transcript(r.Context(), r.PathValue("id"))
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	// A gap still open, in a meeting still running, ends now.
	start := m.CreatedAt
	if m.StartedAt != nil {
		start = *m.StartedAt
	}
	now := max(s.cfg.Now().Sub(start).Milliseconds(), 0)
	for i := range segs {
		if segs[i].EndMS == nil {
			v := max(now, segs[i].StartMS)
			segs[i].EndMS = &v
		}
	}
	name := "meeting-" + m.ID
	switch format {
	case "json":
		writeJSON(w, http.StatusOK, map[string]any{"meeting": m, "segments": segs})
	case "txt":
		download(w, "text/plain; charset=utf-8", name+".txt", TranscriptText(m, segs))
	case "vtt":
		download(w, "text/vtt; charset=utf-8", name+".vtt", TranscriptVTT(segs))
	}
}

func download(w http.ResponseWriter, ctype, filename, body string) {
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, body)
}

// clock is a meeting time as m:ss, or h:mm:ss past an hour.
func clock(msv int64) string {
	t := msv / 1000
	if t >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", t/3600, t/60%60, t%60)
	}
	return fmt.Sprintf("%d:%02d", t/60, t%60)
}

// oneLine is untrusted text on one line: every control character, line or
// paragraph separator and bidi control becomes a space, runs of space become
// one, and the ends are trimmed. A guest's words can then never start a
// line of their own in an export (a forged "[0:10] Host: …").
func oneLine(v string) string {
	var b strings.Builder
	space := false
	for _, r := range v {
		if unicode.IsControl(r) || unicode.IsSpace(r) || r == ' ' || r == ' ' || unicode.Is(unicode.Bidi_Control, r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// speakerTag is a speaker as the text export writes it: the name fenced in
// «», with any «, » or \ in it backslash-escaped, then " (guest)" for a
// guest, OUTSIDE the fence. 🔴 A guest's name is text a stranger typed, and
// may hold ": " (a guest named `Ada L.: I resign. Eve` would otherwise read
// as the owner saying "I resign."); fenced, where the name ends is never in
// doubt, and no guest can drop the "(guest)" mark.
func speakerTag(g store.Segment) string {
	name := g.Speaker
	if g.Guest {
		name = strings.TrimSuffix(name, guestSuffix)
	}
	name = strings.NewReplacer(`\`, `\\`, "«", `\«`, "»", `\»`).Replace(oneLine(name))
	tag := "«" + name + "»"
	if g.Guest {
		tag += guestSuffix
	}
	return tag
}

// guestSuffix is what guestLabel puts after a guest's name.
const guestSuffix = " (guest)"

// TranscriptText is the plain-text export: a heading, then one line per
// segment, "[m:ss] «Speaker»: text" (a guest's "«Name» (guest): text"), and
// "[m:ss–m:ss] «Speaker»: (not transcribed)" for a gap.
func TranscriptText(m store.Meeting, segs []store.Segment) string {
	var b strings.Builder
	title := oneLine(m.Title)
	if title == "" {
		title = m.CreatedAt.UTC().Format("2006-01-02 15:04 UTC")
	}
	fmt.Fprintf(&b, "Transcript: %s\n\n", title)
	for _, g := range segs {
		if g.Kind == store.SegGap {
			end := g.StartMS
			if g.EndMS != nil {
				end = *g.EndMS
			}
			fmt.Fprintf(&b, "[%s–%s] %s: (not transcribed)\n", clock(g.StartMS), clock(end), speakerTag(g))
			continue
		}
		fmt.Fprintf(&b, "[%s] %s: %s\n", clock(g.StartMS), speakerTag(g), oneLine(g.Text))
	}
	return b.String()
}

// vttTime is a WebVTT timestamp, hh:mm:ss.ttt.
func vttTime(msv int64) string {
	d := time.Duration(msv) * time.Millisecond
	return fmt.Sprintf("%02d:%02d:%02d.%03d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60, msv%1000)
}

// vttEscape is untrusted text as WebVTT cue text: on one line (a blank line
// would end the cue, and the next line could be a forged cue), with &, < and
// > escaped (a cue reads <v …>, <c> and <b> as markup, and "-->" as timing).
func vttEscape(v string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(oneLine(v))
}

// TranscriptVTT is the WebVTT export: one cue per line, its speaker as the
// cue's voice; a gap is a cue saying "(not transcribed)".
func TranscriptVTT(segs []store.Segment) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	n := 0
	for _, g := range segs {
		end := g.StartMS
		if g.EndMS != nil {
			end = *g.EndMS
		}
		if end <= g.StartMS {
			end = g.StartMS + 1
		}
		n++
		text := vttEscape(g.Text)
		if g.Kind == store.SegGap {
			text = "(not transcribed)"
		}
		fmt.Fprintf(&b, "%d\n%s --> %s\n<v %s>%s\n\n", n, vttTime(g.StartMS), vttTime(end), vttEscape(g.Speaker), text)
	}
	return b.String()
}
