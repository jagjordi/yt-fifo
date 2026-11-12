package player

import "time"

// Track is one queued (or playing) YouTube item.
//
// The JSON form is what the HTTP API hands out. Fields that only matter while
// streaming (the resolved media URL, its expiry) are deliberately unexported:
// googlevideo URLs are tied to the resolver's IP and expire within hours, so
// nothing outside the player should hold on to them.
type Track struct {
	ID         string    `json:"id"`
	URL        string    `json:"url"`
	VideoID    string    `json:"video_id,omitempty"`
	Title      string    `json:"title,omitempty"`
	Uploader   string    `json:"uploader,omitempty"`
	Duration   float64   `json:"duration,omitempty"` // seconds; 0 when unknown or live
	Thumbnail  string    `json:"thumbnail,omitempty"`
	WebpageURL string    `json:"webpage_url,omitempty"`
	Live       bool      `json:"live,omitempty"`
	AddedAt    time.Time `json:"added_at"`

	streamURL  string
	userAgent  string
	resolvedAt time.Time
}

// NowPlaying is the current track plus where playback is within it.
type NowPlaying struct {
	*Track
	Position float64 `json:"position"` // seconds
}

// Status is a snapshot of the whole player, as served by GET /status and
// pushed on /events.
type Status struct {
	// State is "playing", "paused" or "stopped". "stopped" means nothing is
	// loaded; a paused player with an empty queue is also "stopped".
	State      string      `json:"state"`
	NowPlaying *NowPlaying `json:"now_playing"`
	Queue      []*Track    `json:"queue"`
	// Volume is the output gain, 0..1, applied in software before the sink.
	// Snapcast clients have their own volume on top of it.
	Volume float64 `json:"volume"`
}
