package control

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI records POSTs and serves a fixed status.
type fakeAPI struct {
	mu    sync.Mutex
	posts []string
	state string
}

func (f *fakeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		fmt.Fprintf(w, `{"state":%q,"now_playing":{"id":"7","url":"u","title":"Song","uploader":"Band","duration":100,"thumbnail":"http://x/t.jpg","position":12.5,"added_at":"2026-01-01T00:00:00Z"},"queue":[],"volume":0.35}`, f.state)
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: status\ndata: {\"state\":\"stopped\",\"now_playing\":null,\"queue\":[]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.posts = append(f.posts, r.URL.Path+" "+strings.TrimSpace(string(body)))
		f.mu.Unlock()
		if r.URL.Path == "/skip" {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":"nothing is playing"}`)
			return
		}
		fmt.Fprint(w, `{}`)
	})
	return mux
}

func TestBridge(t *testing.T) {
	api := &fakeAPI{state: "playing"}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	b := &Bridge{API: srv.URL, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx, inR, outW) }()

	out := bufio.NewScanner(outR)
	next := func() map[string]any {
		t.Helper()
		if !out.Scan() {
			t.Fatal("bridge output closed")
		}
		var m map[string]any
		if err := json.Unmarshal(out.Bytes(), &m); err != nil {
			t.Fatalf("bad json %q: %v", out.Text(), err)
		}
		return m
	}

	// Ready first, then the relayed event from /events.
	if m := next(); m["method"] != "Plugin.Stream.Ready" {
		t.Fatalf("first message %v", m)
	}
	if m := next(); m["method"] != "Plugin.Stream.Player.Properties" {
		t.Fatalf("second message %v", m)
	} else if p := m["params"].(map[string]any); p["playbackStatus"] != "stopped" || len(p["metadata"].(map[string]any)) != 0 {
		t.Errorf("relayed properties %v", p)
	}

	send := func(s string) { fmt.Fprintln(inW, s) }

	send(`{"id":1,"jsonrpc":"2.0","method":"Plugin.Stream.Player.GetProperties"}`)
	m := next()
	res := m["result"].(map[string]any)
	if res["playbackStatus"] != "playing" || res["position"] != 12.5 || res["canSeek"] != true || res["volume"] != 35.0 {
		t.Errorf("GetProperties result %v", res)
	}
	md := res["metadata"].(map[string]any)
	if md["title"] != "Song" || md["artUrl"] != "http://x/t.jpg" || md["duration"] != 100.0 {
		t.Errorf("metadata %v", md)
	}

	send(`{"id":2,"jsonrpc":"2.0","method":"Plugin.Stream.Player.Control","params":{"command":"setPosition","params":{"position":17.8}}}`)
	if m := next(); m["result"] != "ok" {
		t.Errorf("setPosition response %v", m)
	}
	send(`{"id":3,"jsonrpc":"2.0","method":"Plugin.Stream.Player.Control","params":{"command":"next"}}`)
	if m := next(); m["error"] == nil || !strings.Contains(m["error"].(map[string]any)["message"].(string), "nothing is playing") {
		t.Errorf("next should relay API error, got %v", m)
	}
	send(`{"id":4,"jsonrpc":"2.0","method":"Plugin.Stream.Player.SetProperty","params":{"volume":50}}`)
	if m := next(); m["result"] != "ok" {
		t.Errorf("SetProperty volume response %v", m)
	}
	send(`{"id":6,"jsonrpc":"2.0","method":"Plugin.Stream.Player.SetProperty","params":{"shuffle":true}}`)
	if m := next(); m["error"] == nil {
		t.Errorf("SetProperty shuffle should fail, got %v", m)
	} else if e := m["error"].(map[string]any); e["code"] != -32601.0 || e["data"] == nil || e["data"] == "" {
		t.Errorf("error must carry code and a string data field for snapserver, got %v", e)
	}
	send(`{"id":5,"jsonrpc":"2.0","method":"Plugin.Stream.Player.Control","params":{"command":"playPause"}}`)
	next()

	api.mu.Lock()
	got := strings.Join(api.posts, "|")
	api.mu.Unlock()
	for _, want := range []string{`/seek {"position":17.8}`, `/skip `, `/toggle `, `/volume {"volume":0.5}`} {
		if !strings.Contains(got, want) {
			t.Errorf("posts %q missing %q", got, want)
		}
	}

	inW.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Run did not exit when stdin closed")
	}
}
