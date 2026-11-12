package player

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// Resolver turns a URL the user typed into Tracks, and a Track into a media
// URL ffmpeg can read, by shelling out to yt-dlp. yt-dlp is the only thing
// that keeps up with YouTube, so it is treated as a black box: one
// invocation, JSON in, no download.
type Resolver interface {
	// Resolve expands url into one or more tracks. With playlist=false a
	// watch URL that also carries a list= parameter yields just that video;
	// a bare playlist URL is always expanded (flat, so a 200-entry playlist
	// costs one yt-dlp call, not 200).
	Resolve(ctx context.Context, url string, playlist bool) ([]*Track, error)
	// ResolveStream fills in the direct media URL for a single track.
	ResolveStream(ctx context.Context, t *Track) error
}

// YtDlpResolver is the real Resolver.
type YtDlpResolver struct {
	Binary    string        // path to yt-dlp
	ExtraArgs []string      // appended verbatim, e.g. --js-runtimes quickjs
	Timeout   time.Duration // per invocation
	Log       *slog.Logger
}

// ErrNoTracks is returned when yt-dlp answered but nothing playable came back
// (empty playlist, every entry private/deleted).
var ErrNoTracks = errors.New("no playable tracks found")

func (r *YtDlpResolver) run(ctx context.Context, args ...string) ([]byte, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	bin := r.Binary
	if bin == "" {
		bin = "yt-dlp"
	}
	full := append([]string{
		"--dump-single-json",
		"--no-warnings",
		"--no-progress",
		"--format", "bestaudio/best",
	}, r.ExtraArgs...)
	full = append(full, args...)

	cmd := exec.CommandContext(ctx, bin, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
	if r.Log != nil {
		r.Log.Debug("yt-dlp finished", "args", args, "took", time.Since(start).Round(time.Millisecond), "err", err)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("yt-dlp timed out after %s", timeout)
		}
		return nil, fmt.Errorf("yt-dlp: %s", ytDlpError(stderr.String(), err))
	}
	return stdout.Bytes(), nil
}

// ytDlpError extracts the useful line from yt-dlp's stderr. It prints
// "ERROR: [youtube] abc: Video unavailable" style lines; the last ERROR line
// is the one worth surfacing to an API caller.
func ytDlpError(stderr string, fallback error) string {
	var last string
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ERROR:") {
			last = strings.TrimSpace(strings.TrimPrefix(line, "ERROR:"))
		}
	}
	if last == "" {
		return fallback.Error()
	}
	return last
}

// ytDlpInfo is the subset of yt-dlp's JSON this program reads. Field names
// follow yt-dlp's output exactly.
type ytDlpInfo struct {
	Type        string            `json:"_type"`
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	Uploader    string            `json:"uploader"`
	Channel     string            `json:"channel"`
	Duration    float64           `json:"duration"`
	Thumbnail   string            `json:"thumbnail"`
	Thumbnails  []ytDlpThumbnail  `json:"thumbnails"`
	WebpageURL  string            `json:"webpage_url"`
	URL         string            `json:"url"`
	IsLive      bool              `json:"is_live"`
	HTTPHeaders map[string]string `json:"http_headers"`
	Entries     []*ytDlpInfo      `json:"entries"`
}

type ytDlpThumbnail struct {
	URL string `json:"url"`
}

func (i *ytDlpInfo) uploader() string {
	if i.Uploader != "" {
		return i.Uploader
	}
	return i.Channel
}

func (i *ytDlpInfo) thumbnail() string {
	if i.Thumbnail != "" {
		return i.Thumbnail
	}
	if n := len(i.Thumbnails); n > 0 {
		return i.Thumbnails[n-1].URL // yt-dlp sorts ascending by quality
	}
	return ""
}

// apply copies metadata from a yt-dlp info dict onto t. Flat playlist
// entries carry a subset of these, which is why every field is guarded.
func (i *ytDlpInfo) apply(t *Track) {
	if i.ID != "" {
		t.VideoID = i.ID
	}
	if i.Title != "" {
		t.Title = i.Title
	}
	if u := i.uploader(); u != "" {
		t.Uploader = u
	}
	if i.Duration > 0 {
		t.Duration = i.Duration
	}
	if th := i.thumbnail(); th != "" {
		t.Thumbnail = th
	}
	if i.WebpageURL != "" {
		t.WebpageURL = i.WebpageURL
	}
	t.Live = i.IsLive
}

// parseInfo turns yt-dlp JSON into tracks. Exported through the package only
// so the tests can feed it canned output.
func parseInfo(data []byte, requestURL string, now time.Time) ([]*Track, error) {
	var info ytDlpInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("parsing yt-dlp output: %w", err)
	}

	if info.Type == "playlist" || info.Type == "multi_video" {
		var tracks []*Track
		for _, e := range info.Entries {
			if e == nil || e.Type == "playlist" {
				continue // deleted/private entries come back as null
			}
			u := e.WebpageURL
			if u == "" {
				u = e.URL // flat entries put the watch URL here
			}
			if u == "" && e.ID != "" {
				u = "https://www.youtube.com/watch?v=" + e.ID
			}
			if u == "" {
				continue
			}
			t := &Track{URL: u, AddedAt: now}
			e.apply(t)
			tracks = append(tracks, t)
		}
		if len(tracks) == 0 {
			return nil, ErrNoTracks
		}
		return tracks, nil
	}

	t := &Track{URL: requestURL, AddedAt: now}
	info.apply(t)
	if info.URL == "" {
		return nil, errors.New("yt-dlp returned no media URL")
	}
	t.streamURL = info.URL
	t.userAgent = info.HTTPHeaders["User-Agent"]
	t.resolvedAt = now
	return []*Track{t}, nil
}

// Resolve implements Resolver.
func (r *YtDlpResolver) Resolve(ctx context.Context, url string, playlist bool) ([]*Track, error) {
	args := []string{"--flat-playlist"}
	if !playlist {
		args = append(args, "--no-playlist")
	}
	args = append(args, "--", url)
	out, err := r.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return parseInfo(out, url, time.Now())
}

// ResolveStream implements Resolver.
func (r *YtDlpResolver) ResolveStream(ctx context.Context, t *Track) error {
	out, err := r.run(ctx, "--no-playlist", "--", t.URL)
	if err != nil {
		return err
	}
	tracks, err := parseInfo(out, t.URL, time.Now())
	if err != nil {
		return err
	}
	if len(tracks) != 1 || tracks[0].streamURL == "" {
		return errors.New("yt-dlp did not resolve a single media URL")
	}
	fresh := tracks[0]
	fresh.apply(t)
	t.streamURL = fresh.streamURL
	t.userAgent = fresh.userAgent
	t.resolvedAt = fresh.resolvedAt
	return nil
}

// apply copies the public metadata of src onto dst, keeping dst's identity.
func (src *Track) apply(dst *Track) {
	dst.VideoID = src.VideoID
	dst.Title = src.Title
	dst.Uploader = src.Uploader
	dst.Duration = src.Duration
	dst.Thumbnail = src.Thumbnail
	dst.WebpageURL = src.WebpageURL
	dst.Live = src.Live
}
