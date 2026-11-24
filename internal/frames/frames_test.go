package frames_test

import (
	"bytes"
	"context"
	"image/jpeg"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/ZacxDev/muster-meetings/internal/frames"
	"github.com/ZacxDev/muster-meetings/internal/s3"
	"github.com/ZacxDev/muster-meetings/internal/s3test"
	"github.com/ZacxDev/muster-meetings/internal/timeline"
)

// The fixtures (testdata/, made by ffmpeg and committed; see the README's
// Tests) are 20 s of 64x64 video at 5 fps whose colour is the second's
// number mod 4: 0 red, 1 green, 2 blue, 3 black. Their index (moov) is at
// the END of the file, as Egress writes its MP4. One is H.264 (what Egress
// records), one VP9 (what the e2e's Chromium can play).
//
// MEETINGS_REQUIRE_FFMPEG=1 (CI) makes a missing ffmpeg a failure, never a
// skip.
func sampler(t *testing.T) frames.Sampler {
	t.Helper()
	s := frames.Find(os.Getenv("MEETINGS_FFMPEG"))
	if !s.Available() {
		if os.Getenv("MEETINGS_REQUIRE_FFMPEG") == "1" {
			t.Fatal("no ffmpeg on PATH (MEETINGS_REQUIRE_FFMPEG=1)")
		}
		t.Skip("no ffmpeg on PATH")
	}
	return s
}

func colour(t *testing.T, jpg []byte) string {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(jpg))
	if err != nil {
		t.Fatalf("not a JPEG: %v", err)
	}
	b := img.Bounds()
	r, g, bl, _ := img.At(b.Dx()/2, b.Dy()/2).RGBA()
	r, g, bl = r>>8, g>>8, bl>>8
	switch {
	case r > 180 && g < 80 && bl < 80:
		return "red"
	case g > 180 && r < 80 && bl < 80:
		return "green"
	case bl > 180 && r < 80 && g < 80:
		return "blue"
	case r < 60 && g < 60 && bl < 60:
		return "black"
	}
	return "unclear"
}

func TestSampleExtractsTheFrameAtEachMeetingTimeFromTheStore(t *testing.T) {
	s := sampler(t)
	for _, fixture := range []string{"recording-h264.mp4", "recording-vp9.mp4"} {
		t.Run(fixture, func(t *testing.T) {
			body, err := os.ReadFile("testdata/" + fixture)
			if err != nil {
				t.Fatal(err)
			}
			f := s3test.New()
			defer f.Close()
			const key = "meetings/SUBJ/MEET/recording-1.mp4"
			f.Put(key, body)
			c, err := s3.New(f.Config())
			if err != nil {
				t.Fatal(err)
			}
			// Part 1's file began at meeting time 4200 and lasts 20 s.
			parts := []timeline.Part{{N: 1, StartMS: 4_200, DurationMS: 20_000}}
			open := func(part int) frames.Object {
				return func(ctx context.Context, rng string) (*http.Response, error) { return c.Get(ctx, key, rng) }
			}
			times := []int64{4_700, 5_700, 6_700, 17_700, 0, 24_200}
			got, skips, err := s.Sample(context.Background(), parts, open, times)
			if err != nil {
				t.Fatal(err)
			}
			want := []struct {
				at, off int64
				colour  string
			}{{4_700, 500, "red"}, {5_700, 1_500, "green"}, {6_700, 2_500, "blue"}, {17_700, 13_500, "green"}}
			if len(got) != len(want) {
				t.Fatalf("%d frames, want %d (skips %+v)", len(got), len(want), skips)
			}
			for i, w := range want {
				if got[i].MeetingMS != w.at || got[i].Part != 1 || got[i].OffsetMS != w.off {
					t.Errorf("frame %d: at %d part %d offset %d, want at %d part 1 offset %d", i, got[i].MeetingMS, got[i].Part, got[i].OffsetMS, w.at, w.off)
				}
				if c := colour(t, got[i].JPEG); c != w.colour {
					t.Errorf("frame at meeting time %d (offset %d ms) is %s, want %s", w.at, w.off, c, w.colour)
				}
			}
			wantSkips := []frames.Skip{{MeetingMS: 0, Why: frames.SkipNotRecorded}, {MeetingMS: 24_200, Why: frames.SkipNotRecorded}}
			if len(skips) != 2 || skips[0] != wantSkips[0] || skips[1] != wantSkips[1] {
				t.Errorf("skips = %+v, want %+v", skips, wantSkips)
			}
			// ffmpeg read the file through byte ranges, never whole.
			ranged := 0
			for _, call := range f.Calls() {
				if strings.HasPrefix(call, "GET "+key+" bytes=") {
					ranged++
				}
			}
			if ranged == 0 {
				t.Errorf("no ranged reads of the object: %v", f.Calls())
			}
		})
	}
}

func TestSampleWithoutFFmpegIsUnavailable(t *testing.T) {
	if _, _, err := (frames.Sampler{}).Sample(context.Background(), nil, nil, []int64{1}); err != frames.ErrUnavailable {
		t.Fatalf("err = %v", err)
	}
	if frames.Find("/nonexistent/ffmpeg").Available() {
		t.Fatal("a missing binary was found")
	}
	_ = exec.ErrNotFound
}
