// Package player holds the queue and drives yt-dlp + ffmpeg to turn YouTube
// URLs into one continuous raw PCM stream.
//
// Playback is a single goroutine (Run) that pops tracks and copies ffmpeg's
// output to the sink. Every control command (pause, skip, seek, ...) is
// expressed the same way: mutate the desired state under the lock, bump the
// generation counter and kill ffmpeg. The run loop notices the generation
// changed, leaves the state alone and simply starts over from whatever the
// command left behind. Pause therefore really is "kill ffmpeg and remember
// the position", and resume is "start ffmpeg again with -ss". That costs a
// moment of silence on resume but means a paused stream never holds a CDN
// connection open, and seek, skip and stop fall out of the same mechanism.
package player

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Config wires a Player.
type Config struct {
	Rate     int // output sample rate, Hz
	Channels int
	FFmpeg   string // ffmpeg binary
	Resolver Resolver
	Sink     io.Writer // where PCM goes; writes may block (that is the pacing)
	Log      *slog.Logger
	// ReresolveAfter bounds how old a resolved media URL may be when playback
	// starts. googlevideo URLs expire after roughly six hours; a track that sat
	// in the queue longer than this is resolved again before it is played.
	ReresolveAfter time.Duration
	// Volume is the initial output gain, 0..1 (linear). Zero means full volume.
	Volume float64
	// MaxVolume is a fixed scaling, 0..1, applied on top of Volume: it is what
	// "100 %" on the volume control amounts to. Use it to tame a source that
	// is much louder than the others on the same snapserver. Zero means 1.
	MaxVolume float64
}

// Errors returned by control methods.
var (
	ErrNothingPlaying = errors.New("nothing is playing")
	ErrNotFound       = errors.New("no such queue item")
	ErrNotSeekable    = errors.New("current track is not seekable")
)

// Player is safe for concurrent use.
type Player struct {
	cfg  Config
	mu   sync.Mutex
	cond *sync.Cond

	queue   []*Track
	current *Track
	paused  bool
	// pos is the playback position (seconds) at which the running ffmpeg
	// started, or the position to resume from while paused/stopped.
	pos float64
	// bytes written to the sink by the current ffmpeg run.
	bytes atomic.Int64
	ff    *exec.Cmd
	// gen is bumped by every command that interrupts ffmpeg, so the run loop
	// can tell "ffmpeg ended by itself" from "I was told to do something".
	gen    uint64
	nextID uint64
	// gain holds the float64 bits of the output volume (0..1). The pump reads
	// it per chunk without taking the lock, so a volume change is heard
	// within a few milliseconds and never restarts ffmpeg.
	gain atomic.Uint64

	subs map[chan Status]struct{}
}

// New returns a stopped, empty player. Call Run to start it.
func New(cfg Config) *Player {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.FFmpeg == "" {
		cfg.FFmpeg = "ffmpeg"
	}
	if cfg.ReresolveAfter <= 0 {
		cfg.ReresolveAfter = 30 * time.Minute
	}
	if cfg.Volume <= 0 || cfg.Volume > 1 || math.IsNaN(cfg.Volume) {
		cfg.Volume = 1
	}
	if cfg.MaxVolume <= 0 || cfg.MaxVolume > 1 || math.IsNaN(cfg.MaxVolume) {
		cfg.MaxVolume = 1
	}
	p := &Player{cfg: cfg, subs: map[chan Status]struct{}{}}
	p.cond = sync.NewCond(&p.mu)
	p.gain.Store(math.Float64bits(cfg.Volume))
	return p
}

// Volume is the output gain, 0..1.
func (p *Player) Volume() float64 {
	return math.Float64frombits(p.gain.Load())
}

// SetVolume sets the output gain, clamped to 0..1, and returns the value
// that was set. It is applied in software to the next chunk of audio, so it
// takes effect at once and the stream keeps running.
func (p *Player) SetVolume(v float64) float64 {
	switch {
	case math.IsNaN(v) || v < 0:
		v = 0
	case v > 1:
		v = 1
	}
	v = math.Round(v*1000) / 1000
	p.mu.Lock()
	p.gain.Store(math.Float64bits(v))
	p.emitLocked()
	p.mu.Unlock()
	return v
}

func (p *Player) bytesPerSecond() float64 {
	return float64(p.cfg.Rate * p.cfg.Channels * 2) // s16le
}

// positionLocked is the current position of the current track.
func (p *Player) positionLocked() float64 {
	if p.current == nil {
		return 0
	}
	if p.ff == nil {
		return p.pos
	}
	return p.pos + float64(p.bytes.Load())/p.bytesPerSecond()
}

func (p *Player) statusLocked() Status {
	st := Status{State: "stopped", Volume: p.Volume(), Queue: append([]*Track(nil), p.queue...)}
	if st.Queue == nil {
		st.Queue = []*Track{}
	}
	if p.current != nil {
		st.NowPlaying = &NowPlaying{Track: p.current, Position: math.Round(p.positionLocked()*1000) / 1000}
		if p.paused {
			st.State = "paused"
		} else {
			st.State = "playing"
		}
	}
	return st
}

// Status returns a snapshot.
func (p *Player) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.statusLocked()
}

// Subscribe returns a channel that receives a Status after every change.
// Slow subscribers miss intermediate states rather than blocking the player.
func (p *Player) Subscribe() (<-chan Status, func()) {
	ch := make(chan Status, 16)
	p.mu.Lock()
	p.subs[ch] = struct{}{}
	p.mu.Unlock()
	return ch, func() {
		p.mu.Lock()
		delete(p.subs, ch)
		p.mu.Unlock()
	}
}

func (p *Player) emitLocked() {
	st := p.statusLocked()
	for ch := range p.subs {
		select {
		case ch <- st:
		default:
		}
	}
}

func (p *Player) emit() {
	p.mu.Lock()
	p.emitLocked()
	p.mu.Unlock()
}

// Enqueue resolves url and appends the resulting track(s).
func (p *Player) Enqueue(ctx context.Context, url string, playlist bool) ([]*Track, error) {
	return p.enqueue(ctx, url, playlist, false)
}

// PlayNow resolves url, puts the resulting track(s) at the front of the queue
// and starts the first of them immediately, dropping whatever was playing.
// The rest of the queue is kept and continues afterwards. It also clears a
// pause, since the caller evidently wants to hear something.
func (p *Player) PlayNow(ctx context.Context, url string, playlist bool) ([]*Track, error) {
	return p.enqueue(ctx, url, playlist, true)
}

func (p *Player) enqueue(ctx context.Context, url string, playlist, front bool) ([]*Track, error) {
	tracks, err := p.cfg.Resolver.Resolve(ctx, url, playlist)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, t := range tracks {
		p.nextID++
		t.ID = strconv.FormatUint(p.nextID, 10)
	}
	if front {
		p.queue = append(append([]*Track(nil), tracks...), p.queue...)
		p.killLocked()
		p.current = nil
		p.pos = 0
		p.paused = false
	} else {
		p.queue = append(p.queue, tracks...)
		// A stopped player (paused with nothing loaded, which is what Stop
		// leaves behind) starts again when something is queued: whoever adds
		// a track wants to hear it. A pause in the middle of a track is kept.
		if p.current == nil {
			p.paused = false
		}
	}
	p.cond.Broadcast()
	p.emitLocked()
	return tracks, nil
}

// Remove deletes a queued (not playing) item.
func (p *Player) Remove(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, t := range p.queue {
		if t.ID == id {
			p.queue = append(p.queue[:i], p.queue[i+1:]...)
			p.emitLocked()
			return nil
		}
	}
	return ErrNotFound
}

// Clear empties the queue. The current track keeps playing.
func (p *Player) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queue = nil
	p.emitLocked()
}

// killLocked interrupts the running ffmpeg, if any, and invalidates the
// current run so the loop does not treat the exit as a finished track.
// Clearing ff here, not when the loop reaps the process, matters for the
// position: until then the pump may still flush a buffer, and positionLocked
// must not add those bytes on top of a pos that already accounts for them.
func (p *Player) killLocked() {
	p.gen++
	if p.ff != nil {
		if p.ff.Process != nil {
			_ = p.ff.Process.Kill()
		}
		p.ff = nil
	}
}

// Skip drops the current track and moves to the next one. While paused the
// next track is loaded but not started.
func (p *Player) Skip() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil && len(p.queue) == 0 {
		return ErrNothingPlaying
	}
	p.killLocked()
	p.current = nil
	p.pos = 0
	if p.paused && len(p.queue) > 0 {
		p.current, p.queue = p.queue[0], p.queue[1:]
	}
	p.cond.Broadcast()
	p.emitLocked()
	return nil
}

// Pause freezes playback; with nothing loaded it just stops the queue from
// advancing until Play.
func (p *Player) Pause() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.paused {
		return
	}
	p.paused = true
	if p.current != nil {
		p.pos = p.positionLocked()
		p.killLocked()
	}
	p.emitLocked()
}

// Play resumes, or starts the queue.
func (p *Player) Play() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.paused {
		return
	}
	p.paused = false
	p.cond.Broadcast()
	p.emitLocked()
}

// PlayPause toggles.
func (p *Player) PlayPause() {
	p.mu.Lock()
	paused := p.paused
	p.mu.Unlock()
	if paused {
		p.Play()
	} else {
		p.Pause()
	}
}

// Paused reports the pause flag (true also while stopped).
func (p *Player) Paused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}

// Stop discards the current track and pauses the queue.
func (p *Player) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.killLocked()
	p.current = nil
	p.pos = 0
	p.paused = true
	p.emitLocked()
}

// Seek moves within the current track. offset is relative when relative is
// true, otherwise an absolute position in seconds.
func (p *Player) Seek(offset float64, relative bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil {
		return ErrNothingPlaying
	}
	if p.current.Live {
		return ErrNotSeekable
	}
	target := offset
	if relative {
		target = p.positionLocked() + offset
	}
	if target < 0 || math.IsNaN(target) {
		target = 0
	}
	if d := p.current.Duration; d > 0 && target > d {
		target = d
	}
	p.pos = target
	p.killLocked()
	p.cond.Broadcast()
	p.emitLocked()
	return nil
}

// Run drives playback until ctx is cancelled. It returns once ffmpeg, if
// running, has been stopped.
func (p *Player) Run(ctx context.Context) {
	stop := context.AfterFunc(ctx, func() {
		p.mu.Lock()
		p.killLocked()
		p.cond.Broadcast()
		p.mu.Unlock()
	})
	defer stop()

	log := p.cfg.Log
	for {
		p.mu.Lock()
		for ctx.Err() == nil && (p.paused || (p.current == nil && len(p.queue) == 0)) {
			p.cond.Wait()
		}
		if ctx.Err() != nil {
			p.mu.Unlock()
			return
		}
		if p.current == nil {
			p.current, p.queue = p.queue[0], p.queue[1:]
			p.pos = 0
		}
		tr, start, gen := p.current, p.pos, p.gen
		p.mu.Unlock()
		p.emit()

		if err := p.ensureStream(ctx, tr); err != nil {
			log.Error("could not resolve track, dropping it", "url", tr.URL, "err", err)
			p.dropIfCurrent(tr, gen)
			continue
		}

		p.mu.Lock()
		if p.gen != gen {
			p.mu.Unlock()
			continue // a command arrived while yt-dlp was running
		}
		cmd, stdout, stderr, err := p.startFFmpeg(ctx, tr, start)
		if err != nil {
			p.mu.Unlock()
			log.Error("could not start ffmpeg, dropping track", "url", tr.URL, "err", err)
			p.dropIfCurrent(tr, gen)
			continue
		}
		p.ff = cmd
		p.bytes.Store(0)
		p.mu.Unlock()
		log.Info("now playing", "title", tr.Title, "url", tr.URL, "start", fmt.Sprintf("%.1fs", start))
		p.emit()

		go p.relayFFmpegLog(stderr, tr)
		written, pumpErr := p.pump(stdout)
		waitErr := cmd.Wait()

		p.mu.Lock()
		if p.ff == cmd {
			p.ff = nil
		}
		if p.gen == gen {
			// ffmpeg ended on its own: the track is over (or broke).
			if waitErr != nil {
				log.Warn("ffmpeg exited abnormally", "title", tr.Title, "err", waitErr, "bytes", written)
			} else if pumpErr != nil {
				log.Warn("output write failed", "err", pumpErr)
			} else {
				log.Info("finished", "title", tr.Title)
			}
			p.current = nil
			p.pos = 0
		}
		p.mu.Unlock()
		p.emit()
	}
}

func (p *Player) dropIfCurrent(tr *Track, gen uint64) {
	p.mu.Lock()
	if p.gen == gen && p.current == tr {
		p.current = nil
		p.pos = 0
	}
	p.mu.Unlock()
	p.emit()
}

func (p *Player) ensureStream(ctx context.Context, tr *Track) error {
	if tr.streamURL != "" && time.Since(tr.resolvedAt) < p.cfg.ReresolveAfter {
		return nil
	}
	return p.cfg.Resolver.ResolveStream(ctx, tr)
}

// ffmpegArgs builds the command line. Exposed for tests.
func ffmpegArgs(tr *Track, start float64, rate, channels int) []string {
	args := []string{"-nostdin", "-hide_banner", "-nostats", "-loglevel", "warning"}
	if tr.userAgent != "" {
		args = append(args, "-user_agent", tr.userAgent)
	}
	u := tr.streamURL
	// The reconnect options belong to the http protocol; HLS playlists go
	// through the hls demuxer which handles its own retries.
	if strings.HasPrefix(u, "http") && !strings.Contains(u, ".m3u8") && !tr.Live {
		args = append(args, "-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "5")
	}
	if start > 0 && !tr.Live {
		args = append(args, "-ss", strconv.FormatFloat(start, 'f', 3, 64))
	}
	args = append(args,
		"-i", u,
		"-vn", "-sn", "-dn",
		"-ac", strconv.Itoa(channels),
		"-ar", strconv.Itoa(rate),
		"-c:a", "pcm_s16le",
		"-f", "s16le",
		"pipe:1",
	)
	return args
}

func (p *Player) startFFmpeg(ctx context.Context, tr *Track, start float64) (*exec.Cmd, io.ReadCloser, io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, p.cfg.FFmpeg, ffmpegArgs(tr, start, p.cfg.Rate, p.cfg.Channels)...)
	// snapserver stops a process source by signalling its process group, and
	// a plain Kill from us must not leave ffmpeg behind either.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	return cmd, stdout, stderr, nil
}

// pump copies ffmpeg's stdout to the sink until EOF, applying the volume and
// counting bytes so the position can be derived from them. Only whole s16le
// samples are forwarded: a read may end in the middle of one, and that byte
// is kept at the front of the buffer for the next round.
func (p *Player) pump(r io.Reader) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	carry := 0
	for {
		n, err := r.Read(buf[carry:])
		if n > 0 {
			end := carry + n
			whole := end &^ 1
			applyGain(buf[:whole], p.Volume()*p.cfg.MaxVolume)
			if _, werr := p.cfg.Sink.Write(buf[:whole]); werr != nil {
				return total, werr
			}
			total += int64(whole)
			p.bytes.Add(int64(whole))
			carry = end - whole
			if carry == 1 {
				buf[0] = buf[end-1]
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			return total, err
		}
	}
}

// applyGain scales the s16le samples in buf in place. buf must hold whole
// samples. A gain of 1 leaves the bytes untouched.
func applyGain(buf []byte, gain float64) {
	if gain == 1 {
		return
	}
	for i := 0; i+1 < len(buf); i += 2 {
		s := math.Round(float64(int16(binary.LittleEndian.Uint16(buf[i:]))) * gain)
		if s > math.MaxInt16 {
			s = math.MaxInt16
		} else if s < math.MinInt16 {
			s = math.MinInt16
		}
		binary.LittleEndian.PutUint16(buf[i:], uint16(int16(s)))
	}
}

func (p *Player) relayFFmpegLog(r io.Reader, tr *Track) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			p.cfg.Log.Warn("ffmpeg: "+line, "title", tr.Title)
		}
	}
}
