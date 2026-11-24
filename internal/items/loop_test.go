package items_test

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/items"
)

// scripted answers its n-th call with script(n) and records each window.
type scripted struct {
	n       int
	windows [][]int64
	script  func(n int) (items.Result, error)
}

func (s *scripted) Name() string { return "scripted" }
func (s *scripted) Propose(_ context.Context, w items.Window) (items.Result, error) {
	var ids []int64
	for _, x := range w.Segments {
		ids = append(ids, x.ID)
	}
	s.windows = append(s.windows, ids)
	n := s.n
	s.n++
	return s.script(n)
}

// timeline is one final segment ending every 10 s, ids 1..n.
func timeline(n int) []items.Segment {
	var out []items.Segment
	for i := 1; i <= n; i++ {
		out = append(out, items.Segment{ID: int64(i), Speaker: "Ada", EndMS: int64(i) * 10000, Text: "line"})
	}
	return out
}

func retry(after time.Duration) error {
	return &items.Error{Class: items.Retry, Status: 429, RetryAfter: after}
}

func TestLoopKeepsTheAnchorAfterARetryAndCoversTheGap(t *testing.T) {
	tl := timeline(20)
	a := &scripted{script: func(n int) (items.Result, error) {
		if n < 11 { // 11 ticks = 110 s: past the 90 s lookback
			return items.Result{}, retry(0)
		}
		return items.Result{Items: []items.Item{{Text: "book the room", Segments: []int64{1}}}}, nil
	}}
	l := &items.Loop{Adapter: a}
	for now := int64(10000); now <= 120000; now += 10000 {
		l.Tick(context.Background(), tl, now)
	}
	if len(a.windows) != 12 || a.windows[11][0] != 1 {
		t.Fatalf("after 11 retried windows the first success's window is %v, want it to start at segment 1 (the anchor stays on a retry)", a.windows[len(a.windows)-1])
	}
	if got := l.Items(); len(got) != 1 || l.Failures() != 0 || l.Wait() != 0 {
		t.Errorf("after the success: items %v, failures %d, wait %v; want 1 item, 0, 0", got, l.Failures(), l.Wait())
	}
}

func TestLoopSpendsASchemaFailuresWindow(t *testing.T) {
	tl := timeline(5)
	a := &scripted{script: func(n int) (items.Result, error) {
		if n == 0 {
			return items.Result{}, &items.Error{Class: items.Schema}
		}
		return items.Result{}, nil
	}}
	l := &items.Loop{Adapter: a}
	l.Tick(context.Background(), tl, 10000)
	if l.Wait() != 0 {
		t.Errorf("wait after a schema failure = %v, want 0", l.Wait())
	}
	if _, called := l.Tick(context.Background(), tl, 15000); called {
		t.Errorf("a call with no segment newer than the spent window: %v (a schema failure moves the anchor)", a.windows)
	}
}

func TestLoopPausesAfterThreeFailuresOfAnyClassAndASuccessClearsThem(t *testing.T) {
	tl := timeline(10)
	script := []error{retry(0), &items.Error{Class: items.Schema}, retry(0), nil, retry(9 * time.Second)}
	a := &scripted{script: func(n int) (items.Result, error) { return items.Result{}, script[n] }}
	l := &items.Loop{Adapter: a}
	want := []struct {
		fails  int
		paused bool
		wait   time.Duration
	}{{1, false, time.Second}, {2, false, 0}, {3, true, 4 * time.Second}, {0, false, 0}, {1, false, 9 * time.Second}}
	for i, w := range want {
		l.Tick(context.Background(), tl, int64(i+1)*10000)
		if l.Failures() != w.fails || l.Paused() != w.paused || l.Wait() != w.wait {
			t.Errorf("after call %d: failures %d paused %v wait %v; want %d %v %v", i, l.Failures(), l.Paused(), l.Wait(), w.fails, w.paused, w.wait)
		}
	}
}

func TestLoopStopsOnAFatalFailure(t *testing.T) {
	tl := timeline(5)
	a := &scripted{script: func(int) (items.Result, error) { return items.Result{}, &items.Error{Class: items.Fatal, Status: 404} }}
	l := &items.Loop{Adapter: a}
	l.Tick(context.Background(), tl, 10000)
	for now := int64(20000); now <= 50000; now += 10000 {
		if _, called := l.Tick(context.Background(), tl, now); called {
			t.Fatalf("a call after a fatal failure at %d ms", now)
		}
	}
	if l.Stopped() == nil || a.n != 1 {
		t.Errorf("stopped %v after %d calls, want stopped after 1", l.Stopped(), a.n)
	}
}

func TestLoopGuardsUnlessRaw(t *testing.T) {
	tl := timeline(2)
	twice := func(int) (items.Result, error) {
		it := items.Item{Text: "send the deck", Segments: []int64{1}}
		return items.Result{Items: []items.Item{it, it}}, nil
	}
	l := &items.Loop{Adapter: &scripted{script: twice}}
	l.Tick(context.Background(), tl, 10000)
	r := &items.Loop{Adapter: &scripted{script: twice}, Raw: true}
	r.Tick(context.Background(), tl, 10000)
	if len(l.Items()) != 1 || len(r.Items()) != 2 {
		t.Errorf("guarded kept %d, raw kept %d; want 1 and 2", len(l.Items()), len(r.Items()))
	}
}
