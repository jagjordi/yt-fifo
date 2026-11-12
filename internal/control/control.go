// Package control bridges a running yt-fifo daemon to Snapcast's stream
// plugin protocol (doc/json_rpc_api/stream_plugin.md in the snapcast repo).
//
// snapserver launches this as the stream's `controlscript`, talks newline
// delimited JSON-RPC over stdin/stdout, and in return gets track metadata
// (title, artwork, duration) for Snapweb and Home Assistant plus working
// play/pause/next/seek from every Snapcast client. The bridge is stateless:
// every request is answered from the daemon's HTTP API, and the daemon's
// /events feed is relayed as Plugin.Stream.Player.Properties notifications.
package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jagjordi/yt-fifo/internal/player"
)

// Bridge is one control script instance.
type Bridge struct {
	API    string // base URL of the daemon, e.g. http://127.0.0.1:8080
	Log    *slog.Logger
	Client *http.Client

	out   io.Writer
	outMu sync.Mutex
}

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type controlParams struct {
	Command string          `json:"command"`
	Params  json.RawMessage `json:"params"`
}

// rpcError is a JSON-RPC error object. snapserver (PcmStream::control) casts
// code to its ControlErrc enum and forwards data, as a string, to whoever sent
// the Stream.Control request, so data must always be present and a string.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

// Error codes snapserver understands (server/streamreader/control_error.hpp).
const (
	errCanSeekIsFalse = 6
	errMethodNotFound = -32601
	errInvalidParams  = -32602
	errInternal       = -32603
)

// Run serves until stdin closes (snapserver went away) or ctx ends.
func (b *Bridge) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	if b.Client == nil {
		b.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if b.Log == nil {
		b.Log = slog.Default()
	}
	b.API = strings.TrimRight(b.API, "/")
	b.out = out

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Snapserver queries properties as soon as it sees Ready; make sure the
	// daemon can answer first, otherwise the stream shows up with no
	// metadata until the next change.
	b.waitForAPI(ctx)
	b.notify("Plugin.Stream.Ready", nil)

	go b.relayEvents(ctx)

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			b.Log.Warn("ignoring malformed request", "err", err)
			continue
		}
		b.handle(ctx, req)
	}
	return sc.Err()
}

func (b *Bridge) waitForAPI(ctx context.Context) {
	for ctx.Err() == nil {
		if _, err := b.status(ctx); err == nil {
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func (b *Bridge) write(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	b.outMu.Lock()
	defer b.outMu.Unlock()
	_, _ = b.out.Write(append(data, '\n'))
}

func (b *Bridge) notify(method string, params any) {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	b.write(msg)
}

func (b *Bridge) respond(id json.RawMessage, result any) {
	b.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (b *Bridge) fail(id json.RawMessage, code int, msg string) {
	b.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": rpcError{Code: code, Message: msg, Data: msg}})
}

func (b *Bridge) handle(ctx context.Context, req request) {
	switch req.Method {
	case "Plugin.Stream.Player.GetProperties":
		st, err := b.status(ctx)
		if err != nil {
			b.fail(req.ID, errInternal, "yt-fifo API unreachable: "+err.Error())
			return
		}
		b.respond(req.ID, properties(st))

	case "Plugin.Stream.Player.Control":
		var cp controlParams
		if err := json.Unmarshal(req.Params, &cp); err != nil {
			b.fail(req.ID, errInvalidParams, "invalid params")
			return
		}
		if err := b.control(ctx, cp); err != nil {
			code := errInternal
			if errors.Is(err, errNotSeekable) {
				code = errCanSeekIsFalse
			}
			b.fail(req.ID, code, err.Error())
			return
		}
		b.respond(req.ID, "ok")

	case "Plugin.Stream.Player.SetProperty":
		// params is {"<property>": value}. Volume (percent) maps onto the
		// daemon's software gain; mute, shuffle, loop and rate are not
		// things this player has.
		var props map[string]json.RawMessage
		if err := json.Unmarshal(req.Params, &props); err != nil {
			b.fail(req.ID, errInvalidParams, "invalid params")
			return
		}
		raw, ok := props["volume"]
		if !ok || len(props) != 1 {
			b.fail(req.ID, errMethodNotFound, "yt-fifo only supports setting volume")
			return
		}
		var pct float64
		if err := json.Unmarshal(raw, &pct); err != nil {
			b.fail(req.ID, errInvalidParams, "volume must be a number 0..100")
			return
		}
		if err := b.post(ctx, "/volume", map[string]any{"volume": pct / 100}); err != nil {
			b.fail(req.ID, errInternal, err.Error())
			return
		}
		b.respond(req.ID, "ok")

	default:
		if len(req.ID) > 0 && string(req.ID) != "null" {
			b.fail(req.ID, errMethodNotFound, "method not found: "+req.Method)
		}
	}
}

// control maps the plugin protocol's commands onto the HTTP API.
func (b *Bridge) control(ctx context.Context, cp controlParams) error {
	var num struct {
		Offset   float64 `json:"offset"`
		Position float64 `json:"position"`
	}
	if len(cp.Params) > 0 {
		_ = json.Unmarshal(cp.Params, &num)
	}
	switch cp.Command {
	case "play":
		return b.post(ctx, "/play", nil)
	case "pause":
		return b.post(ctx, "/pause", nil)
	case "playPause":
		return b.post(ctx, "/toggle", nil)
	case "stop":
		return b.post(ctx, "/stop", nil)
	case "next":
		return b.post(ctx, "/skip", nil)
	case "previous":
		// There is no history; "previous" restarts the track, which is what
		// most players do when pressed early in a song anyway.
		return b.post(ctx, "/seek", map[string]any{"position": 0})
	case "seek":
		return b.post(ctx, "/seek", map[string]any{"offset": num.Offset})
	case "setPosition":
		return b.post(ctx, "/seek", map[string]any{"position": num.Position})
	default:
		return fmt.Errorf("unknown command %q", cp.Command)
	}
}

func (b *Bridge) status(ctx context.Context) (*player.Status, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.API+"/status", nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /status: %s", resp.Status)
	}
	var st player.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (b *Bridge) post(ctx context.Context, path string, body any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.API+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		if e.Error == player.ErrNotSeekable.Error() {
			return errNotSeekable
		}
		return errors.New(e.Error)
	}
	return nil
}

// errNotSeekable mirrors player.ErrNotSeekable across the HTTP boundary so the
// bridge can answer with snapserver's can_seek_is_false code.
var errNotSeekable = errors.New(player.ErrNotSeekable.Error())

// relayEvents follows /events and forwards every status as a Properties
// notification, reconnecting for as long as ctx lives.
func (b *Bridge) relayEvents(ctx context.Context) {
	for ctx.Err() == nil {
		err := b.followEvents(ctx)
		if ctx.Err() != nil {
			return
		}
		b.Log.Warn("event stream dropped, reconnecting", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *Bridge) followEvents(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.API+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{}).Do(req) // no timeout: this connection is long-lived
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /events: %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var st player.Status
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &st); err != nil {
			continue
		}
		b.notify("Plugin.Stream.Player.Properties", properties(&st))
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.EOF
}

// properties converts a Status to the plugin protocol's property set.
func properties(st *player.Status) map[string]any {
	np := st.NowPlaying
	props := map[string]any{
		"canControl":     true,
		"canPlay":        true,
		"canPause":       true,
		"canSeek":        np != nil && !np.Live,
		"canGoNext":      np != nil || len(st.Queue) > 0,
		"canGoPrevious":  false,
		"loopStatus":     "none",
		"shuffle":        false,
		"rate":           1.0,
		"volume":         int(math.Round(st.Volume * 100)),
		"playbackStatus": st.State,
		"position":       0.0,
		// An empty object, not an omitted key: the server keeps the previous
		// metadata when the key is missing, and a stopped stream must not
		// keep showing the last video.
		"metadata": map[string]any{},
	}
	if np == nil {
		return props
	}
	props["position"] = np.Position
	md := map[string]any{
		"trackId": np.ID,
		"url":     np.URL,
	}
	if np.Title != "" {
		md["title"] = np.Title
	}
	if np.Uploader != "" {
		md["artist"] = []string{np.Uploader}
	}
	if np.Duration > 0 {
		md["duration"] = np.Duration
	}
	if np.Thumbnail != "" {
		md["artUrl"] = np.Thumbnail
	}
	if np.WebpageURL != "" {
		md["url"] = np.WebpageURL
	}
	props["metadata"] = md
	return props
}
