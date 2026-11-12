package player

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type fakeResolver struct {
	resolveCalls atomic.Int32
	streamCalls  atomic.Int32
}

func (f *fakeResolver) Resolve(_ context.Context, url string, _ bool) ([]*Track, error) {
	f.resolveCalls.Add(1)
	return []*Track{{URL: url, Title: "T " + url, Duration: 5, streamURL: "fake://" + url, resolvedAt: time.Now(), AddedAt: time.Now()}}, nil
}

func (f *fakeResolver) ResolveStream(_ context.Context, t *Track) error {
	f.streamCalls.Add(1)
	t.streamURL = "fake://" + t.URL
	t.resolvedAt = time.Now()
	return nil
}

type countingSink struct{ n atomic.Int64 }

func (c *countingSink) Write(b []byte) (int, error) { c.n.Add(int64(len(b))); return len(b), nil }

func newTestPlayer(t *testing.T) (*Player, *countingSink, *fakeResolver, context.CancelFunc) {
	t.Helper()
	wd, _ := os.Getwd()
	t.Setenv("FAKE_SECONDS", "5")
	sink := &countingSink{}
	res := &fakeResolver{}
	p := New(Config{
		Rate: 48000, Channels: 2,
		FFmpeg:   filepath.Join(wd, "testdata", "fake-ffmpeg.sh"),
		Resolver: res, Sink: sink,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return p, sink, res, cancel
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPlaysQueueInOrderAndAdvances(t *testing.T) {
	p, sink, _, _ := newTestPlayer(t)
	t.Setenv("FAKE_SECONDS", "1")
	if _, err := p.Enqueue(context.Background(), "a", false); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Enqueue(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a playing", func() bool {
		st := p.Status()
		return st.State == "playing" && st.NowPlaying != nil && st.NowPlaying.URL == "a"
	})
	if len(p.Status().Queue) != 1 {
		t.Errorf("queue should hold b, got %d", len(p.Status().Queue))
	}
	waitFor(t, "b playing after a finished", func() bool {
		st := p.Status()
		return st.NowPlaying != nil && st.NowPlaying.URL == "b"
	})
	waitFor(t, "stopped at end of queue", func() bool { return p.Status().State == "stopped" })
	// 2 tracks × 1 s × 192000 B/s
	if n := sink.n.Load(); n < 300000 || n > 500000 {
		t.Errorf("sink got %d bytes, want about 384000", n)
	}
}

func TestPauseRemembersPositionAndResumesWithSeek(t *testing.T) {
	p, sink, _, _ := newTestPlayer(t)
	if _, err := p.Enqueue(context.Background(), "a", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "some audio", func() bool { return sink.n.Load() >= 19200*4 })
	p.Pause()
	st := p.Status()
	if st.State != "paused" || st.NowPlaying == nil {
		t.Fatalf("state after pause: %+v", st)
	}
	pos := st.NowPlaying.Position
	if pos < 0.3 || pos > 1.5 {
		t.Errorf("paused position %.2f out of expected range", pos)
	}
	before := sink.n.Load()
	time.Sleep(400 * time.Millisecond)
	if after := sink.n.Load(); after-before > 19200*2 {
		t.Errorf("audio kept flowing while paused: %d bytes", after-before)
	}
	if p.Status().NowPlaying.Position != pos {
		t.Error("position moved while paused")
	}
	p.Play()
	waitFor(t, "playing again", func() bool { return p.Status().State == "playing" })
	waitFor(t, "position advanced past pause point", func() bool {
		np := p.Status().NowPlaying
		return np != nil && np.Position > pos+0.2
	})
}

func TestSkipAndStop(t *testing.T) {
	p, _, _, _ := newTestPlayer(t)
	if err := p.Skip(); err != ErrNothingPlaying {
		t.Errorf("Skip on empty = %v", err)
	}
	for _, u := range []string{"a", "b", "c"} {
		if _, err := p.Enqueue(context.Background(), u, false); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "a playing", func() bool { np := p.Status().NowPlaying; return np != nil && np.URL == "a" })
	if err := p.Skip(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b playing", func() bool {
		np := p.Status().NowPlaying
		return np != nil && np.URL == "b" && p.Status().State == "playing"
	})
	p.Stop()
	st := p.Status()
	if st.State != "stopped" || st.NowPlaying != nil || len(st.Queue) != 1 || st.Queue[0].URL != "c" {
		t.Fatalf("after stop: %+v", st)
	}
	p.Play()
	waitFor(t, "c playing", func() bool { np := p.Status().NowPlaying; return np != nil && np.URL == "c" })
	if err := p.Seek(4, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "position near 4", func() bool { np := p.Status().NowPlaying; return np != nil && np.Position >= 4 })
	waitFor(t, "finished shortly after seek", func() bool { return p.Status().State == "stopped" })
}

func TestRemoveAndClear(t *testing.T) {
	p, _, _, _ := newTestPlayer(t)
	p.Pause() // keep everything queued
	for _, u := range []string{"a", "b", "c"} {
		if _, err := p.Enqueue(context.Background(), u, false); err != nil {
			t.Fatal(err)
		}
	}
	q := p.Status().Queue
	if len(q) != 3 {
		t.Fatalf("queue %d", len(q))
	}
	if err := p.Remove(q[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := p.Remove("nope"); err != ErrNotFound {
		t.Errorf("Remove unknown = %v", err)
	}
	q = p.Status().Queue
	if len(q) != 2 || q[0].URL != "a" || q[1].URL != "c" {
		t.Errorf("after remove: %+v", q)
	}
	p.Clear()
	if len(p.Status().Queue) != 0 {
		t.Error("clear left items")
	}
}

func TestPlayNowInterruptsAndKeepsQueue(t *testing.T) {
	p, _, _, _ := newTestPlayer(t)
	for _, u := range []string{"a", "b"} {
		if _, err := p.Enqueue(context.Background(), u, false); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "a playing", func() bool { np := p.Status().NowPlaying; return np != nil && np.URL == "a" })
	if _, err := p.PlayNow(context.Background(), "x", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "x playing", func() bool {
		np := p.Status().NowPlaying
		return np != nil && np.URL == "x" && p.Status().State == "playing"
	})
	if q := p.Status().Queue; len(q) != 1 || q[0].URL != "b" {
		t.Errorf("queue after play now: %+v", q)
	}
	p.Pause()
	if _, err := p.PlayNow(context.Background(), "y", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "y playing despite earlier pause", func() bool {
		np := p.Status().NowPlaying
		return np != nil && np.URL == "y" && p.Status().State == "playing"
	})
}

func TestEnqueueAfterStopStartsPlaying(t *testing.T) {
	p, _, _, _ := newTestPlayer(t)
	if _, err := p.Enqueue(context.Background(), "a", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a playing", func() bool { np := p.Status().NowPlaying; return np != nil && np.URL == "a" })
	p.Stop()
	if st := p.Status(); st.State != "stopped" {
		t.Fatalf("state after stop %q", st.State)
	}
	if _, err := p.Enqueue(context.Background(), "b", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b playing after enqueue on a stopped player", func() bool {
		np := p.Status().NowPlaying
		return np != nil && np.URL == "b" && p.Status().State == "playing"
	})
	// A pause in the middle of a track must survive an enqueue.
	p.Pause()
	if _, err := p.Enqueue(context.Background(), "c", false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if st := p.Status(); st.State != "paused" || st.NowPlaying.URL != "b" || len(st.Queue) != 1 {
		t.Errorf("enqueue while paused mid-track changed state: %+v", st)
	}
}
