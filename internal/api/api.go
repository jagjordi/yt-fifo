// Package api is the HTTP face of the player: a small JSON API, a
// server-sent-events feed and a one-page web UI.
package api

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jagjordi/yt-fifo/internal/player"
)

//go:embed index.html
var indexHTML []byte

// Server exposes a Player over HTTP.
type Server struct {
	p       *player.Player
	log     *slog.Logger
	version string
	output  string
}

// New builds the handler. output is reported by /health so a caller can see
// where audio is going.
func New(p *player.Player, log *slog.Logger, version, output string) http.Handler {
	s := &Server{p: p, log: log, version: version, output: output}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("GET /queue", s.getQueue)
	mux.HandleFunc("POST /queue", s.enqueue)
	mux.HandleFunc("POST /queue/batch", s.enqueueBatch)
	mux.HandleFunc("DELETE /queue/{id}", s.remove)
	mux.HandleFunc("POST /skip", s.skip)
	mux.HandleFunc("POST /next", s.skip)
	mux.HandleFunc("POST /pause", s.pause)
	mux.HandleFunc("POST /resume", s.play)
	mux.HandleFunc("POST /play", s.play)
	mux.HandleFunc("POST /toggle", s.toggle)
	mux.HandleFunc("POST /stop", s.stop)
	mux.HandleFunc("POST /clear", s.clear)
	mux.HandleFunc("POST /seek", s.seek)
	mux.HandleFunc("GET /volume", s.getVolume)
	mux.HandleFunc("POST /volume", s.setVolume)
	mux.HandleFunc("GET /events", s.events)
	return s.logging(mux)
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" && r.URL.Path != "/health" && r.URL.Path != "/status" {
			s.log.Debug("http", "method", r.Method, "path", r.URL.Path, "from", r.RemoteAddr)
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func decode(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	st := s.p.Status()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.version,
		"output":  s.output,
		"state":   st.State,
		"paused":  s.p.Paused(),
		"volume":  st.Volume,
	})
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.p.Status())
}

// getQueue keeps the original API's shape: now_playing plus queued.
func (s *Server) getQueue(w http.ResponseWriter, _ *http.Request) {
	st := s.p.Status()
	writeJSON(w, http.StatusOK, map[string]any{
		"state":       st.State,
		"now_playing": st.NowPlaying,
		"queued":      st.Queue,
	})
}

type enqueueRequest struct {
	URL      string `json:"url"`
	Playlist bool   `json:"playlist"`
	// PlayNow interrupts the current track and plays this one immediately;
	// what was queued stays queued behind it.
	PlayNow bool `json:"play_now"`
}

func validURL(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

func (s *Server) enqueue(w http.ResponseWriter, r *http.Request) {
	var req enqueueRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !validURL(req.URL) {
		writeError(w, http.StatusBadRequest, errors.New("url must be an http(s) URL"))
		return
	}
	var tracks []*player.Track
	var err error
	if req.PlayNow {
		tracks, err = s.p.PlayNow(r.Context(), req.URL, req.Playlist)
	} else {
		tracks, err = s.p.Enqueue(r.Context(), req.URL, req.Playlist)
	}
	if err != nil {
		s.log.Warn("enqueue failed", "url", req.URL, "err", err)
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	s.log.Info("enqueued", "count", len(tracks), "title", tracks[0].Title, "url", req.URL, "play_now", req.PlayNow)
	writeJSON(w, http.StatusOK, map[string]any{
		"enqueued":     tracks,
		"queue_length": len(s.p.Status().Queue),
	})
}

type batchRequest struct {
	URLs     []string `json:"urls"`
	Playlist bool     `json:"playlist"`
}

// enqueueBatch resolves each URL in turn and reports failures per URL rather
// than failing the whole batch.
func (s *Server) enqueueBatch(w http.ResponseWriter, r *http.Request) {
	var req batchRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var enqueued []*player.Track
	failed := map[string]string{}
	for _, u := range req.URLs {
		if !validURL(u) {
			failed[u] = "not an http(s) URL"
			continue
		}
		tracks, err := s.p.Enqueue(r.Context(), u, req.Playlist)
		if err != nil {
			failed[u] = err.Error()
			continue
		}
		enqueued = append(enqueued, tracks...)
	}
	code := http.StatusOK
	if len(enqueued) == 0 && len(failed) > 0 {
		code = http.StatusUnprocessableEntity
	}
	if enqueued == nil {
		enqueued = []*player.Track{}
	}
	writeJSON(w, code, map[string]any{
		"enqueued":     enqueued,
		"failed":       failed,
		"queue_length": len(s.p.Status().Queue),
	})
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	if err := s.p.Remove(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": r.PathValue("id")})
}

func (s *Server) skip(w http.ResponseWriter, _ *http.Request) {
	st := s.p.Status()
	if err := s.p.Skip(); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	var skipped any
	if st.NowPlaying != nil {
		skipped = st.NowPlaying.Track
	}
	writeJSON(w, http.StatusOK, map[string]any{"skipped": skipped, "status": s.p.Status()})
}

func (s *Server) pause(w http.ResponseWriter, _ *http.Request) {
	s.p.Pause()
	writeJSON(w, http.StatusOK, map[string]any{"paused": true, "status": s.p.Status()})
}

func (s *Server) play(w http.ResponseWriter, _ *http.Request) {
	s.p.Play()
	writeJSON(w, http.StatusOK, map[string]any{"paused": false, "status": s.p.Status()})
}

func (s *Server) toggle(w http.ResponseWriter, _ *http.Request) {
	s.p.PlayPause()
	writeJSON(w, http.StatusOK, map[string]any{"paused": s.p.Paused(), "status": s.p.Status()})
}

func (s *Server) stop(w http.ResponseWriter, _ *http.Request) {
	s.p.Stop()
	writeJSON(w, http.StatusOK, map[string]any{"stopped": true, "status": s.p.Status()})
}

func (s *Server) clear(w http.ResponseWriter, _ *http.Request) {
	s.p.Clear()
	writeJSON(w, http.StatusOK, map[string]any{"cleared": true})
}

type seekRequest struct {
	Position *float64 `json:"position"` // absolute seconds
	Offset   *float64 `json:"offset"`   // relative seconds
}

func (s *Server) seek(w http.ResponseWriter, r *http.Request) {
	var req seekRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var err error
	switch {
	case req.Position != nil:
		err = s.p.Seek(*req.Position, false)
	case req.Offset != nil:
		err = s.p.Seek(*req.Offset, true)
	default:
		writeError(w, http.StatusBadRequest, errors.New("give position or offset"))
		return
	}
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": s.p.Status()})
}

type volumeRequest struct {
	Volume *float64 `json:"volume"` // absolute, 0..1
	Offset *float64 `json:"offset"` // relative, e.g. -0.1
}

func (s *Server) getVolume(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"volume": s.p.Volume()})
}

// setVolume changes the software gain. Out-of-range values are clamped and
// the value actually set is returned.
func (s *Server) setVolume(w http.ResponseWriter, r *http.Request) {
	var req volumeRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var v float64
	switch {
	case req.Volume != nil:
		v = *req.Volume
	case req.Offset != nil:
		v = s.p.Volume() + *req.Offset
	default:
		writeError(w, http.StatusBadRequest, errors.New("give volume (0..1) or offset"))
		return
	}
	v = s.p.SetVolume(v)
	s.log.Info("volume", "level", v)
	writeJSON(w, http.StatusOK, map[string]any{"volume": v})
}

// events streams Status as server-sent events: the current state first, then
// one event per change, with a comment every 15 s as a keepalive.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	ch, cancel := s.p.Subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(st player.Status) bool {
		b, err := json.Marshal(st)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: status\ndata: %s\n\n", b); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !send(s.p.Status()) {
		return
	}
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case st := <-ch:
			if !send(st) {
				return
			}
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
