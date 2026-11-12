# yt-fifo

Queue YouTube videos, get their audio as one continuous raw PCM stream for
[Snapcast](https://github.com/snapcast/snapcast). A single static Go binary
that drives `yt-dlp` and `ffmpeg`, with an HTTP API, a one-page web UI and a
Snapcast stream-plugin bridge so titles, thumbnails and play/pause/next show
up in Snapweb and Home Assistant.

```
POST /queue {"url": ...}
        │
        ▼
┌─ yt-fifo serve ─────────────────────────────────────────────┐
│  queue ──▶ yt-dlp (resolve) ──▶ ffmpeg (decode) ──▶ s16le ──┼──▶ snapserver ──▶ snapclients
│  HTTP API · web UI · /events                                │
└──────────────────────────────────────────────────────────────┘
        ▲ /status, /events, /play, /skip, /seek …
┌─ yt-fifo control ───────┐
│ Snapcast stream plugin  │◀── JSON-RPC over stdin/stdout ──▶ snapserver
└─────────────────────────┘
```

Output is `s16le`, 48 kHz stereo by default (the Snapcast default). Nothing
is written while idle, so snapserver marks the stream `idle` and a
`meta://` stream falls through to the next source.

## Running it

### Inside home-audio (recommended)

The [home-audio](https://github.com/jagjordi/home-audio) image bundles this
binary. snapserver launches it as a `process://` source and reads PCM from its
stdout, the same way it runs go-librespot for Spotify:

```ini
source = process:///usr/local/bin/yt-fifo?name=YouTube&sampleformat=48000:16:2&params=serve%20--listen%20:8888&idle_threshold=2000&wd_timeout=0&log_stderr=true&controlscript=/usr/local/bin/yt-fifo&controlscriptparams=control%20--api%20http://127.0.0.1:8888
```

No FIFO, no shared directories: snapserver owns the process, restarts it if it
dies, and the `controlscript` is the same binary in `control` mode. The HTTP
API is then on the host's port 8888 (home-audio uses host networking).

### Standalone container

```sh
docker compose up -d    # see docker-compose.yml
```

The container defaults to writing a named pipe at `/pipe/ytaudio`; mount the
directory snapserver reads its `pipe://` source from at `/pipe`, e.g. with
home-audio's `/tmp:/fifos` mapping use `-v /tmp:/pipe` and

```ini
source = pipe:///fifos/ytaudio?name=YouTube&mode=create&sampleformat=48000:16:2
```

snapserver re-opens an idle pipe every 100 ms, so yt-fifo opens the write side
blocking and the kernel pairs the two; nothing needs to be started in a
particular order. Or set
`YT_FIFO_OUTPUT=tcp://<snapserver>:4953` and add to `snapserver.conf`:

```ini
source = tcp://0.0.0.0:4953?name=YouTube&mode=server&sampleformat=48000:16:2
```

An Unraid template is in `unraid/yt-fifo.xml`.

### Bare

```sh
go build -o yt-fifo .
./yt-fifo serve -listen :8888 -output /tmp/snapfifo      # FIFO
./yt-fifo serve -output tcp://127.0.0.1:4953             # snapserver tcp source
./yt-fifo serve -output - | aplay -f S16_LE -r 48000 -c 2 # anything that reads PCM
```

`yt-dlp` and `ffmpeg` must be on `$PATH` (or pass `-yt-dlp` / `-ffmpeg`).

## HTTP API

Default port 8080 (`-listen`, `YT_FIFO_LISTEN`, or `PORT`). Bodies and
responses are JSON. `GET /` serves the web UI.

| Method | Path | What it does |
|---|---|---|
| `GET` | `/health` | `{"status":"ok", "state": …, "output": …}` |
| `GET` | `/status` | State, `now_playing` (with `position`), `queue` |
| `GET` | `/queue` | Same as `/status`, with the queue under `queued` (original API shape) |
| `POST` | `/queue` | `{"url": "…", "playlist": false, "play_now": false}` – resolves and appends. A bare playlist URL expands to all its entries; `playlist: true` also expands a watch URL that carries a `list=` parameter. `play_now: true` interrupts the current track and plays this one immediately, keeping the rest of the queue behind it. Returns `422` with yt-dlp's message if the URL cannot be resolved. |
| `POST` | `/queue/batch` | `{"urls": […]}` – each URL resolved in turn, failures reported per URL |
| `DELETE` | `/queue/{id}` | Remove a queued item |
| `POST` | `/skip`, `/next` | Drop the current track, start the next (`409` if nothing to skip) |
| `POST` | `/pause` | Freeze playback. With nothing loaded, hold the queue |
| `POST` | `/resume`, `/play` | Resume, or start the queue |
| `POST` | `/toggle` | Play/pause |
| `POST` | `/stop` | Drop the current track and hold the queue until `/play`, or until something new is queued |
| `POST` | `/clear` | Empty the queue (current track keeps playing) |
| `POST` | `/seek` | `{"position": 90}` absolute or `{"offset": -30}` relative, seconds |
| `GET` | `/volume` | `{"volume": 0.5}` – the software gain, 0..1 |
| `POST` | `/volume` | `{"volume": 0.5}` absolute or `{"offset": -0.1}` relative. Applied to the PCM in software, so it takes effect immediately without restarting the stream; the snapclients' own volume sits on top of it |
| `GET` | `/events` | Server-sent events: a `status` event with the full status on every change |

```sh
curl -X POST -H 'Content-Type: application/json' \
  -d '{"url":"https://www.youtube.com/watch?v=dCt6jXEVx0A"}' http://host:8888/queue
```

Pause really stops decoding: ffmpeg is killed and the position remembered,
and resume starts ffmpeg again with `-ss`. A paused stream therefore never
holds a CDN connection open, at the cost of a brief gap on resume. Seek, skip
and stop work the same way.

## Snapcast control bridge

`yt-fifo control --api http://127.0.0.1:8888` implements Snapcast's
[stream plugin protocol](https://github.com/snapcast/snapcast/blob/develop/doc/json_rpc_api/stream_plugin.md).
snapserver starts it (via `controlscript=`) and it:

- reports `title`, `artist` (channel), `duration`, `artUrl` (thumbnail) and
  `position` as the stream's properties, pushing updates from `/events`
- maps `play`, `pause`, `playPause`, `stop`, `next`, `seek`, `setPosition`
  onto the HTTP API; `previous` restarts the track
- rejects `SetProperty` (volume, shuffle, loop): snapclients own volume

It ignores the `--stream`, `--snapcast-host` and `--snapcast-port` arguments
snapserver always appends.

## Configuration

Flags win over environment variables. The `AUDIO_*` and `PORT` names are kept
from the original Python version.

| Flag | Env | Default | |
|---|---|---|---|
| `-listen` | `YT_FIFO_LISTEN`, `PORT` | `:8080` | HTTP API address |
| `-output` | `YT_FIFO_OUTPUT`, `AUDIO_FIFO` | `-` | `-` (stdout), `tcp://host:port`, or a FIFO path |
| `-rate` | `YT_FIFO_RATE`, `AUDIO_RATE` | `48000` | Output sample rate |
| `-channels` | `YT_FIFO_CHANNELS`, `AUDIO_CHANNELS` | `2` | Output channels |
| `-volume` | `YT_FIFO_VOLUME` | `1` | Initial output volume, 0..1 (linear gain). Change it at runtime with `POST /volume`, the web UI slider, Snapcast's `Stream.SetProperty` or Home Assistant |
| `-max-volume` | `YT_FIFO_MAX_VOLUME` | `1` | Fixed scaling on top of the volume, 0..1: what volume `1.0` amounts to. Use it when YouTube is much louder than the other snapserver sources, so the volume control keeps its full range |
| `-yt-dlp-args` | `YT_DLP_ARGS` | | Extra yt-dlp arguments, e.g. `--cookies /config/cookies.txt` |
| `-yt-dlp`, `-ffmpeg` | `YT_DLP`, `FFMPEG` | names on `$PATH` | Binaries |
| `-log-level` | `YT_FIFO_LOG_LEVEL` | `info` | `debug` logs every yt-dlp call and HTTP request |

Logs go to stderr without timestamps, since both snapserver (`log_stderr=true`)
and Docker add their own.

### yt-dlp notes

- YouTube extraction needs a JavaScript runtime. The image ships
  [quickjs](https://bellard.org/quickjs/) (2 MB) and `yt-dlp-ejs`, selected via
  `/etc/yt-dlp.conf`, rather than deno (120 MB).
- yt-dlp also reads `~/.config/yt-dlp/config` and `./yt-dlp.conf`; a mounted
  config file is the place for cookies or a proxy.
- When YouTube changes something and extraction breaks, updating the image
  (which pulls the current `yt-dlp-core` and `yt-dlp-ejs` from Alpine edge) is
  the fix. `POST /queue` returns yt-dlp's error verbatim so the cause is visible.
- Resolved media URLs expire after a few hours; a track that waited in the
  queue for more than 30 minutes is resolved again just before it plays.

## Chrome extension

[`chrome/`](chrome/) is a Manifest V3 extension: right-click a YouTube link
or watch page for **Play on home audio** / **Queue on home audio** (and
**Queue whole playlist**), with the resolved title or the error in a
notification. Load it unpacked and set the server URL in its options.

## WizeStream integration

`wizestream/` holds a patch for [WizeStream](https://github.com/jagjordi/WizeStream) (NewPipe
fork) that adds "Play on home audio" and "Queue on home audio" to the video long-press menu and
the Cast picker, posting to this API. See [`wizestream/README.md`](wizestream/README.md).

## Development

```sh
go test ./...          # unit tests; the player tests use testdata/fake-ffmpeg.sh
go vet ./... && gofmt -l .
docker build --build-arg VERSION=$(git describe --tags --always) -t yt-fifo .
```

Layout: `internal/player` (queue, ffmpeg, position), `internal/sink`
(stdout/tcp/FIFO outputs with reconnect), `internal/api` (HTTP + web UI),
`internal/control` (Snapcast bridge), `main.go` (CLI).
