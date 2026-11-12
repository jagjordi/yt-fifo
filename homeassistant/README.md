# Home Assistant integration

A `media_player` entity for a yt-fifo server, plus the dashboard card and helper
scripts used on the "remote control" tab.

```
custom_components/yt_fifo/   the integration (YAML platform, no HACS needed)
configuration.yaml           the media_player platform entry
scripts.yaml                 "Play now" / "Queue" scripts reading input_text.youtube
dashboard-card.yaml          the card: mini-media-player + URL field + buttons + queue list
```

## What the entity does

`media_player.home_audio_youtube` polls `GET /status` every 5 s and exposes:

| Home Assistant | yt-fifo |
|---|---|
| state `playing` / `paused` / `idle` | `state` |
| `media_title`, `media_artist`, `entity_picture`, `media_duration`, `media_position` | now playing: title, uploader, thumbnail, duration, position |
| `queue` attribute (title, uploader, duration, url) and `queue_length` | the queue |
| `media_play`, `media_pause`, `media_stop`, `media_next_track`, `media_seek` | `/play`, `/pause`, `/stop`, `/skip`, `/seek` |
| `volume_level`; `volume_set`, `volume_up`, `volume_down` | `volume` (software gain 0..1, before the snapclients' own volume); `POST /volume` |
| `play_media` with `enqueue: add` | `POST /queue` (append; starts playing if nothing was) |
| `play_media` with `enqueue: play` or no `enqueue` | `POST /queue` with `play_now` (interrupts the current track) |
| `play_media` with `enqueue: replace` | `/clear` then `play_now` |
| `clear_playlist` | `/clear` |

`media_content_type` is ignored (yt-dlp works the URL out), `enqueue: next` is
treated as `add` because the server has no insert-after-current. Errors from
the server (an unavailable video, say) surface as the service call's error.

Because it is a plain `media_player`, every standard card and the voice
assistants work with it: `media_player.play_media` from an automation, the
mushroom media card, mini-media-player, `media_player.media_next_track` from a
button.

## Install

1. Copy `custom_components/yt_fifo` into `/config/custom_components/`.
2. Add the `media_player:` block from `configuration.yaml` (adjust host/port).
3. Add the two scripts from `scripts.yaml` (they expect `input_text.youtube`).
4. Restart Home Assistant.
5. Add `dashboard-card.yaml` to a view. It needs
   [mini-media-player](https://github.com/kalkih/mini-media-player) from HACS.

## Local test

```sh
docker run --rm -p 8123:8123 -v $PWD/test-config:/config ghcr.io/home-assistant/home-assistant:stable
```

with `custom_components/yt_fifo` and the `media_player:` block in `test-config`.
Onboarding gives a token; `GET /api/states/media_player.home_audio_youtube` and
`POST /api/services/media_player/play_media` exercise it against a real server.
