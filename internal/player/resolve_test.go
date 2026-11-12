package player

import (
	"os"
	"testing"
	"time"
)

func TestParseInfoSingle(t *testing.T) {
	data, err := os.ReadFile("testdata/single.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tracks, err := parseInfo(data, "https://youtu.be/dCt6jXEVx0A", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 1 {
		t.Fatalf("got %d tracks, want 1", len(tracks))
	}
	tr := tracks[0]
	if tr.URL != "https://youtu.be/dCt6jXEVx0A" {
		t.Errorf("URL = %q, want the request URL kept", tr.URL)
	}
	if tr.VideoID != "dCt6jXEVx0A" || tr.Title == "" || tr.Uploader != "The Wild Project" || tr.Duration != 871 {
		t.Errorf("metadata not applied: %+v", tr)
	}
	if tr.streamURL == "" || tr.userAgent != "Mozilla/5.0 test" || !tr.resolvedAt.Equal(now) {
		t.Errorf("stream fields not set: url=%q ua=%q", tr.streamURL, tr.userAgent)
	}
	if tr.Thumbnail != "https://i.ytimg.com/vi/dCt6jXEVx0A/maxresdefault.jpg" {
		t.Errorf("thumbnail = %q", tr.Thumbnail)
	}
}

func TestParseInfoPlaylist(t *testing.T) {
	data, err := os.ReadFile("testdata/playlist.json")
	if err != nil {
		t.Fatal(err)
	}
	tracks, err := parseInfo(data, "https://www.youtube.com/playlist?list=PLx", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 {
		t.Fatalf("got %d tracks, want 2 (null entry skipped)", len(tracks))
	}
	if tracks[0].Title != "First" || tracks[0].Duration != 120 || tracks[0].Thumbnail == "" {
		t.Errorf("first entry: %+v", tracks[0])
	}
	if tracks[1].Uploader != "Chan2" || tracks[1].Duration != 0 {
		t.Errorf("second entry: %+v", tracks[1])
	}
	if tracks[0].streamURL != "" {
		t.Error("flat entries must not carry a stream URL")
	}
}

func TestParseInfoEmptyPlaylist(t *testing.T) {
	_, err := parseInfo([]byte(`{"_type":"playlist","entries":[null]}`), "u", time.Now())
	if err != ErrNoTracks {
		t.Errorf("err = %v, want ErrNoTracks", err)
	}
}

func TestYtDlpError(t *testing.T) {
	got := ytDlpError("WARNING: x\nERROR: [youtube] abc: Video unavailable\n", os.ErrInvalid)
	if got != "[youtube] abc: Video unavailable" {
		t.Errorf("got %q", got)
	}
	if ytDlpError("", os.ErrInvalid) != os.ErrInvalid.Error() {
		t.Error("fallback not used")
	}
}

func TestFFmpegArgs(t *testing.T) {
	tr := &Track{streamURL: "https://cdn/x", userAgent: "UA"}
	args := ffmpegArgs(tr, 12.5, 48000, 2)
	joined := ""
	for _, a := range args {
		joined += a + " "
	}
	for _, want := range []string{"-user_agent UA", "-reconnect 1", "-ss 12.500", "-i https://cdn/x", "-ac 2", "-ar 48000", "-f s16le pipe:1"} {
		if !contains(joined, want) {
			t.Errorf("args missing %q: %s", want, joined)
		}
	}
	live := ffmpegArgs(&Track{streamURL: "https://cdn/live.m3u8", Live: true}, 30, 48000, 2)
	for _, a := range live {
		if a == "-ss" || a == "-reconnect" {
			t.Errorf("live stream must not get %s", a)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
