package main

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ZacxDev/muster-meetings/internal/items"
	"github.com/ZacxDev/muster-meetings/internal/sttcorpus"
)

// The meeting kinds the design names.
var meetingKinds = []string{"standup", "planning", "customer", "one_to_one"}

// The hard-case kinds, every one required: the design's, and injection
// (a guest's speech addressing the model: an instruction, a forged segment
// id or speaker, a forged owner).
var (
	trapKinds     = []string{"hypothetical", "past_tense", "question", "retracted", "injection"}
	labelKinds    = []string{"cross_speaker", "restated"}
	requiredKinds = append(append(append([]string{}, trapKinds...), labelKinds...), "asr_noise")
)

// The corpus's shape bars (design: "The corpus").
const (
	wantMeetings   = 12
	minWindows     = 130 // "about 150"
	maxWindows     = 170
	minNoItemShare = 0.30
	// "6–12 segments each": the MEDIAN window is held to it. The steady state
	// of a 90 s window is 8–13 segments at these speech rates; each meeting's
	// first windows are shorter (as in production), and a run of short replies
	// packs more in, so the range is reported, not bounded.
	minWindowMedian = 6
	maxWindowMedian = 12
	minPerHardCase  = 3 // "each in several windows"
	segGapMS        = 1200
	wordsPerSecond  = 2.2
	minSegMS        = 2000
)

// Meeting is one corpus file.
type Meeting struct {
	ID    string `yaml:"id"`
	Kind  string `yaml:"kind"`
	Title string `yaml:"title"`
	ASR   bool   `yaml:"asr"`
	// Unicode marks the meeting written to tell ScaleDown's offset unit:
	// non-ASCII speaker names, curly apostrophes, a character outside the
	// Basic Multilingual Plane. A rewritten span in it fails the run.
	Unicode  bool      `yaml:"unicode"`
	Speakers []Speaker `yaml:"speakers"`
	Segments []CSeg    `yaml:"segments"`
	Labels   []Label   `yaml:"labels"`
	Traps    []Trap    `yaml:"traps"`
	// STT is a speech-to-text variant's record (cmd/items-corpus-stt): a
	// source meeting has none.
	STT *sttcorpus.Info `yaml:"stt"`

	file     string
	timeline []items.Segment
	// source is a variant's source meeting: its timeline is the source's,
	// with the variant's text.
	source *Meeting
}

type Speaker struct {
	Name  string `yaml:"name"`
	Guest bool   `yaml:"guest"`
}

// CSeg is a segment as written: its times are derived.
type CSeg struct {
	ID      int64   `yaml:"id"`
	Speaker string  `yaml:"speaker"`
	Text    string  `yaml:"text"`
	Dur     float64 `yaml:"dur"`
}

// Label is one expected item.
type Label struct {
	ID       string   `yaml:"id"`
	Text     string   `yaml:"text"`
	Assignee string   `yaml:"assignee"`
	Due      string   `yaml:"due"`
	Segments []int64  `yaml:"segments"`
	Cases    []string `yaml:"cases"`
}

// Trap is a negative hard case: segments that state no item.
type Trap struct {
	Kind     string  `yaml:"kind"`
	Segments []int64 `yaml:"segments"`
}

// LoadCorpus reads every source meeting in dir (each *.yaml that is not a
// variant, *.stt.yaml), in name order. Unknown fields are refused.
func LoadCorpus(dir string) ([]*Meeting, error) {
	return loadDir(dir, false, nil)
}

// LoadSTT reads every speech-to-text variant in dir (*.stt.yaml), in name
// order, each paired with its source in clean by file name: its timeline is
// the source's segment times with its own text.
func LoadSTT(dir string, clean []*Meeting) ([]*Meeting, error) {
	return loadDir(dir, true, clean)
}

func loadDir(dir string, variants bool, clean []*Meeting) ([]*Meeting, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	bySource := map[string]*Meeting{}
	for _, m := range clean {
		bySource[sttcorpus.VariantName(m.file)] = m
	}
	var out []*Meeting
	for _, f := range files {
		if sttcorpus.IsVariant(f) != variants {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		d := yaml.NewDecoder(bytes.NewReader(b))
		d.KnownFields(true)
		m := &Meeting{}
		if err := d.Decode(m); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		m.file = filepath.Base(f)
		m.source = bySource[m.file]
		m.build()
		out = append(out, m)
	}
	if len(out) == 0 && !variants {
		return nil, fmt.Errorf("%s: no meetings (*.yaml)", dir)
	}
	return out, nil
}

// build derives the timeline: each segment starts segGapMS after the last
// ends and lasts Dur seconds, else one second per wordsPerSecond words.
//
// A variant's segment keeps its source segment's times, so its windows are
// the source's; a segment the engine heard nothing in is left out, as
// production stores no line for it.
func (m *Meeting) build() {
	if m.source != nil {
		m.buildVariant()
		return
	}
	guest := map[string]bool{}
	for _, s := range m.Speakers {
		guest[s.Name] = s.Guest
	}
	var t int64
	m.timeline = nil
	for _, s := range m.Segments {
		d := int64(math.Round(s.Dur * 1000))
		if d <= 0 {
			d = int64(math.Round(float64(len(strings.Fields(s.Text))) / wordsPerSecond * 1000))
		}
		d = max(d, minSegMS)
		m.timeline = append(m.timeline, items.Segment{ID: s.ID, Speaker: s.Speaker, Guest: guest[s.Speaker], StartMS: t, EndMS: t + d, Text: s.Text})
		t += d + segGapMS
	}
}

func (m *Meeting) buildVariant() {
	at := map[int64]items.Segment{}
	for _, s := range m.source.Timeline() {
		at[s.ID] = s
	}
	m.timeline = nil
	for _, s := range m.Segments {
		src, ok := at[s.ID]
		if !ok || strings.TrimSpace(s.Text) == "" {
			continue
		}
		src.Text = s.Text
		m.timeline = append(m.timeline, src)
	}
}

// Timeline is the meeting's final segments with their times.
func (m *Meeting) Timeline() []items.Segment { return m.timeline }

// DurationMS is the meeting's length: the last segment's end.
func (m *Meeting) DurationMS() int64 {
	if m.source != nil {
		return m.source.DurationMS()
	}
	if len(m.timeline) == 0 {
		return 0
	}
	return m.timeline[len(m.timeline)-1].EndMS
}

// Win is one window as production would send it, with every call
// succeeding: its segments, and the ones new in it.
type Win struct {
	Segments []items.Segment
	New      []int64
}

// Windows replays the meeting's ticks through items.Select, every call a
// success.
func (m *Meeting) Windows() []Win {
	var out []Win
	var last int64
	tl := m.Timeline()
	every := items.DefaultEvery.Milliseconds()
	for t := every; t <= m.DurationMS()+every; t += every {
		w, ok := items.Select(tl, t, last)
		if !ok {
			continue
		}
		var nw []int64
		for _, s := range w {
			if s.ID > last {
				nw = append(nw, s.ID)
			}
		}
		out = append(out, Win{w, nw})
		last = items.LastID(w, last)
	}
	return out
}

// Stats is the corpus's shape.
type Stats struct {
	Meetings, Segments, Labels, Windows, NoItemWindows int
	NoItemShare                                        float64
	WindowMin, WindowMedian, WindowP90, WindowMax      int
	HardCases                                          map[string]int
	Kinds                                              map[string]int
	MinutesTotal                                       float64
	UnicodeMeetings                                    int
}

// itemWindow reports whether w has an expected item: a label's segment is
// new in it.
func itemWindow(m *Meeting, w Win) bool {
	for _, l := range m.Labels {
		for _, id := range l.Segments {
			for _, n := range w.New {
				if id == n {
					return true
				}
			}
		}
	}
	return false
}

// CorpusStats measures ms.
func CorpusStats(ms []*Meeting) Stats {
	st := Stats{HardCases: map[string]int{}, Kinds: map[string]int{}}
	var sizes []int
	for _, m := range ms {
		st.Meetings++
		st.Kinds[m.Kind]++
		st.Segments += len(m.Segments)
		st.Labels += len(m.Labels)
		st.MinutesTotal += float64(m.DurationMS()) / 60000
		if m.Unicode {
			st.UnicodeMeetings++
		}
		for _, w := range m.Windows() {
			st.Windows++
			sizes = append(sizes, len(w.Segments))
			if !itemWindow(m, w) {
				st.NoItemWindows++
			}
		}
		for _, l := range m.Labels {
			for _, c := range l.Cases {
				st.HardCases[c]++
			}
		}
		for _, t := range m.Traps {
			st.HardCases[t.Kind]++
		}
		if m.ASR {
			st.HardCases["asr_noise"] += len(m.Windows())
		}
	}
	sort.Ints(sizes)
	if len(sizes) > 0 {
		st.WindowMin = sizes[0]
		st.WindowMedian = sizes[len(sizes)/2]
		st.WindowMax = sizes[len(sizes)-1]
		st.WindowP90 = sizes[(len(sizes)*9)/10]
	}
	if st.Windows > 0 {
		st.NoItemShare = float64(st.NoItemWindows) / float64(st.Windows)
	}
	return st
}

// Lint is every way ms falls short of the design's corpus, or of itself.
func Lint(ms []*Meeting) []string {
	var p []string
	bad := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	seenSeg := map[int64]string{}
	seenMeeting := map[string]bool{}
	seenLabel := map[string]bool{}
	for _, m := range ms {
		where := m.file
		if m.ID == "" || seenMeeting[m.ID] {
			bad("%s: meeting id %q is empty or not unique", where, m.ID)
		}
		seenMeeting[m.ID] = true
		if m.STT != nil {
			bad("%s: has an stt block: a source meeting is written, not transcribed", where)
		}
		if !contains(meetingKinds, m.Kind) {
			bad("%s: kind %q is not one of %v", where, m.Kind, meetingKinds)
		}
		speakers := map[string]bool{}
		for _, s := range m.Speakers {
			if s.Name == "" || speakers[s.Name] {
				bad("%s: speaker %q is empty or listed twice", where, s.Name)
			}
			speakers[s.Name] = true
		}
		inMeeting := map[int64]bool{}
		var prev int64
		for _, s := range m.Segments {
			if s.ID <= prev {
				bad("%s: segment %d is not in increasing id order", where, s.ID)
			}
			prev = s.ID
			if o, dup := seenSeg[s.ID]; dup {
				bad("%s: segment %d is also in %s (ids are unique across the corpus)", where, s.ID, o)
			}
			seenSeg[s.ID] = m.ID
			inMeeting[s.ID] = true
			if !speakers[s.Speaker] {
				bad("%s: segment %d's speaker %q is not in speakers", where, s.ID, s.Speaker)
			}
			if strings.TrimSpace(s.Text) == "" || strings.ContainsAny(s.Text, "\n\r") {
				bad("%s: segment %d's text is empty or has a line break", where, s.ID)
			}
		}
		cited := func(what string, ids []int64) {
			if len(ids) == 0 {
				bad("%s: %s cites no segment", where, what)
			}
			for _, id := range ids {
				if !inMeeting[id] {
					bad("%s: %s cites segment %d, which is not in this meeting", where, what, id)
				}
			}
		}
		labelled := map[int64]bool{}
		for _, l := range m.Labels {
			what := "label " + l.ID
			if l.ID == "" || seenLabel[l.ID] {
				bad("%s: label id %q is empty or not unique", where, l.ID)
			}
			seenLabel[l.ID] = true
			if strings.TrimSpace(l.Text) == "" {
				bad("%s: %s has no text", where, what)
			}
			if !speakers[l.Assignee] {
				bad("%s: %s's assignee %q is not a speaker", where, what, l.Assignee)
			}
			cited(what, l.Segments)
			for _, id := range l.Segments {
				labelled[id] = true
			}
			for _, c := range l.Cases {
				if !contains(labelKinds, c) {
					bad("%s: %s's case %q is not a label case %v", where, what, c, labelKinds)
				}
			}
			if contains(l.Cases, "cross_speaker") && len(speakersOf(m, l.Segments)) < 2 {
				bad("%s: %s is cross_speaker but its segments have one speaker", where, what)
			}
			if contains(l.Cases, "restated") && len(l.Segments) < 2 {
				bad("%s: %s is restated but cites one segment", where, what)
			}
		}
		wins := m.Windows()
		for _, t := range m.Traps {
			what := "trap " + t.Kind
			if !contains(trapKinds, t.Kind) {
				bad("%s: trap kind %q is not one of %v", where, t.Kind, trapKinds)
			}
			if t.Kind == "injection" {
				for _, sp := range keys(speakersOf(m, t.Segments)) {
					if !guestOf(m, sp) {
						bad("%s: %s is said by %q, who is not a guest", where, what, sp)
					}
				}
			}
			cited(what, t.Segments)
			for _, id := range t.Segments {
				if labelled[id] {
					bad("%s: %s cites segment %d, which a label also cites", where, what, id)
				}
			}
			// A retraction must arrive in the same window as the commitment:
			// a live worker cannot take back an item it already showed.
			if t.Kind == "retracted" && firstWindow(wins, t.Segments[0]) != firstWindow(wins, t.Segments[len(t.Segments)-1]) {
				bad("%s: %s's segments arrive in different windows", where, what)
			}
		}
	}
	if !unicodeMeeting(ms) {
		bad("corpus: no meeting marked unicode with a non-ASCII speaker name, a curly apostrophe, a character outside the BMP and a label")
	}
	st := CorpusStats(ms)
	if st.Meetings != wantMeetings {
		bad("corpus: %d meetings, want %d", st.Meetings, wantMeetings)
	}
	for _, k := range meetingKinds {
		if st.Kinds[k] == 0 {
			bad("corpus: no %s meeting", k)
		}
	}
	if st.Windows < minWindows || st.Windows > maxWindows {
		bad("corpus: %d windows, want %d to %d (about 150)", st.Windows, minWindows, maxWindows)
	}
	if st.NoItemShare < minNoItemShare {
		bad("corpus: %.2f of windows have no item, want at least %.2f", st.NoItemShare, minNoItemShare)
	}
	if st.WindowMedian < minWindowMedian || st.WindowMedian > maxWindowMedian {
		bad("corpus: the median window holds %d segments, want %d to %d", st.WindowMedian, minWindowMedian, maxWindowMedian)
	}
	for _, k := range requiredKinds {
		if st.HardCases[k] < minPerHardCase {
			bad("corpus: hard case %s occurs %d times, want at least %d", k, st.HardCases[k], minPerHardCase)
		}
	}
	return p
}

// unicodeMeeting reports a meeting marked unicode that has what it is for:
// a speaker name with a non-ASCII letter, a curly apostrophe (U+2019) and a
// character outside the Basic Multilingual Plane in its text, and a label.
func unicodeMeeting(ms []*Meeting) bool {
	for _, m := range ms {
		if !m.Unicode || len(m.Labels) == 0 {
			continue
		}
		name, curly, astral := false, false, false
		for _, sp := range m.Speakers {
			for _, r := range sp.Name {
				name = name || r > 127
			}
		}
		for _, s := range m.Segments {
			for _, r := range s.Text {
				curly = curly || r == '\u2019'
				astral = astral || r > 0xFFFF
			}
		}
		if name && curly && astral {
			return true
		}
	}
	return false
}

func guestOf(m *Meeting, name string) bool {
	for _, s := range m.Speakers {
		if s.Name == name {
			return s.Guest
		}
	}
	return false
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func firstWindow(ws []Win, id int64) int {
	for i, w := range ws {
		for _, n := range w.New {
			if n == id {
				return i
			}
		}
	}
	return -1
}

func speakersOf(m *Meeting, ids []int64) map[string]bool {
	out := map[string]bool{}
	for _, s := range m.Segments {
		for _, id := range ids {
			if s.ID == id {
				out[s.Speaker] = true
			}
		}
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if x == y {
			return true
		}
	}
	return false
}
