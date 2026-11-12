"""media_player platform for yt-fifo (https://github.com/jagjordi/yt-fifo).

One entity per server. State and queue come from GET /status every few seconds;
controls map onto the server's HTTP API:

    play / pause / stop / next_track / seek   -> /play /pause /stop /skip /seek
    volume_set / volume_up / volume_down      -> /volume (software gain, 0..1)
    play_media(url, enqueue=...)              -> POST /queue
        add, next -> appended (the server has no "insert after current")
        play, replace, or no enqueue option   -> play_now: the current track is
                                                 interrupted; replace empties the
                                                 queue first
    clear_playlist                            -> /clear

The queue is exposed as the `queue` attribute (list of title/uploader/duration/
url) so a dashboard card can list it.
"""

from __future__ import annotations

import asyncio
import logging
from datetime import timedelta
from typing import Any

import aiohttp
import voluptuous as vol

from homeassistant.components.media_player import (
    PLATFORM_SCHEMA as MEDIA_PLAYER_PLATFORM_SCHEMA,
    MediaPlayerDeviceClass,
    MediaPlayerEnqueue,
    MediaPlayerEntity,
    MediaPlayerEntityFeature,
    MediaPlayerState,
    MediaType,
)
from homeassistant.const import CONF_HOST, CONF_NAME, CONF_PORT
from homeassistant.core import HomeAssistant
from homeassistant.exceptions import HomeAssistantError
from homeassistant.helpers import config_validation as cv
from homeassistant.helpers.aiohttp_client import async_get_clientsession
from homeassistant.helpers.entity_platform import AddEntitiesCallback
from homeassistant.helpers.typing import ConfigType, DiscoveryInfoType
from homeassistant.util import dt as dt_util

from . import DOMAIN

_LOGGER = logging.getLogger(__name__)

DEFAULT_NAME = "Home audio YouTube"
DEFAULT_PORT = 8080
SCAN_INTERVAL = timedelta(seconds=5)
# yt-dlp resolution happens inside POST /queue and can take a while.
QUEUE_TIMEOUT = aiohttp.ClientTimeout(total=90)
CONTROL_TIMEOUT = aiohttp.ClientTimeout(total=10)

PLATFORM_SCHEMA = MEDIA_PLAYER_PLATFORM_SCHEMA.extend(
    {
        vol.Required(CONF_HOST): cv.string,
        vol.Optional(CONF_PORT, default=DEFAULT_PORT): cv.port,
        vol.Optional(CONF_NAME, default=DEFAULT_NAME): cv.string,
    }
)

SUPPORTED_FEATURES = (
    MediaPlayerEntityFeature.PLAY
    | MediaPlayerEntityFeature.PAUSE
    | MediaPlayerEntityFeature.STOP
    | MediaPlayerEntityFeature.NEXT_TRACK
    | MediaPlayerEntityFeature.SEEK
    | MediaPlayerEntityFeature.PLAY_MEDIA
    | MediaPlayerEntityFeature.MEDIA_ENQUEUE
    | MediaPlayerEntityFeature.CLEAR_PLAYLIST
    | MediaPlayerEntityFeature.VOLUME_SET
    | MediaPlayerEntityFeature.VOLUME_STEP
)


async def async_setup_platform(
    hass: HomeAssistant,
    config: ConfigType,
    async_add_entities: AddEntitiesCallback,
    discovery_info: DiscoveryInfoType | None = None,
) -> None:
    """Set up one yt-fifo player from YAML."""
    base_url = f"http://{config[CONF_HOST]}:{config[CONF_PORT]}"
    async_add_entities(
        [YtFifoMediaPlayer(hass, base_url, config[CONF_NAME])], update_before_add=True
    )


class YtFifoMediaPlayer(MediaPlayerEntity):
    """A yt-fifo server as a media player."""

    _attr_device_class = MediaPlayerDeviceClass.SPEAKER
    _attr_supported_features = SUPPORTED_FEATURES
    _attr_media_content_type = MediaType.MUSIC
    _attr_media_image_remotely_accessible = True
    _attr_should_poll = True

    def __init__(self, hass: HomeAssistant, base_url: str, name: str) -> None:
        self._session = async_get_clientsession(hass)
        self._base_url = base_url
        self._attr_name = name
        self._attr_unique_id = f"{DOMAIN}_{base_url}"
        self._attr_available = False
        self._status: dict[str, Any] = {}

    # ---- polling ---------------------------------------------------------

    async def async_update(self) -> None:
        """Fetch /status."""
        try:
            async with self._session.get(
                f"{self._base_url}/status", timeout=CONTROL_TIMEOUT
            ) as resp:
                resp.raise_for_status()
                status = await resp.json()
        except (aiohttp.ClientError, asyncio.TimeoutError) as err:
            if self._attr_available:
                _LOGGER.warning("yt-fifo at %s unreachable: %s", self._base_url, err)
            self._attr_available = False
            return
        if not self._attr_available:
            _LOGGER.info("yt-fifo at %s is reachable", self._base_url)
        self._attr_available = True
        self._status = status
        self._attr_media_position_updated_at = dt_util.utcnow()

    @property
    def _now_playing(self) -> dict[str, Any] | None:
        return self._status.get("now_playing")

    # ---- state -----------------------------------------------------------

    @property
    def state(self) -> MediaPlayerState | None:
        return {
            "playing": MediaPlayerState.PLAYING,
            "paused": MediaPlayerState.PAUSED,
            "stopped": MediaPlayerState.IDLE,
        }.get(self._status.get("state", ""), MediaPlayerState.IDLE)

    @property
    def media_title(self) -> str | None:
        np = self._now_playing
        return (np.get("title") or np.get("url")) if np else None

    @property
    def media_artist(self) -> str | None:
        np = self._now_playing
        return np.get("uploader") if np else None

    @property
    def media_image_url(self) -> str | None:
        np = self._now_playing
        return np.get("thumbnail") if np else None

    @property
    def media_content_id(self) -> str | None:
        np = self._now_playing
        return (np.get("webpage_url") or np.get("url")) if np else None

    @property
    def media_duration(self) -> int | None:
        np = self._now_playing
        if np and np.get("duration"):
            return int(np["duration"])
        return None

    @property
    def media_position(self) -> int | None:
        np = self._now_playing
        return int(np.get("position", 0)) if np else None

    @property
    def volume_level(self) -> float | None:
        volume = self._status.get("volume")
        return float(volume) if volume is not None else None

    @property
    def extra_state_attributes(self) -> dict[str, Any]:
        queue = self._status.get("queue") or []
        return {
            "queue_length": len(queue),
            "queue": [
                {
                    "id": item.get("id"),
                    "title": item.get("title") or item.get("url"),
                    "uploader": item.get("uploader"),
                    "duration": item.get("duration"),
                    "url": item.get("webpage_url") or item.get("url"),
                }
                for item in queue
            ],
        }

    # ---- controls --------------------------------------------------------

    async def _post(
        self, path: str, body: dict[str, Any] | None = None, timeout=CONTROL_TIMEOUT
    ) -> dict[str, Any]:
        async with self._session.post(
            f"{self._base_url}{path}", json=body, timeout=timeout
        ) as resp:
            data = await resp.json(content_type=None)
            if resp.status >= 400:
                message = data.get("error", resp.reason) if isinstance(data, dict) else resp.reason
                raise HomeAssistantError(f"yt-fifo {path}: {message}")
            return data

    async def async_media_play(self) -> None:
        await self._post("/play")
        await self.async_update()

    async def async_media_pause(self) -> None:
        await self._post("/pause")
        await self.async_update()

    async def async_media_stop(self) -> None:
        await self._post("/stop")
        await self.async_update()

    async def async_media_next_track(self) -> None:
        await self._post("/skip")
        await self.async_update()

    async def async_media_seek(self, position: float) -> None:
        await self._post("/seek", {"position": position})
        await self.async_update()

    async def async_clear_playlist(self) -> None:
        await self._post("/clear")
        await self.async_update()

    async def async_set_volume_level(self, volume: float) -> None:
        """Set the server's software gain (volume_up/down use this in 0.1 steps)."""
        await self._post("/volume", {"volume": volume})
        await self.async_update()

    async def async_play_media(
        self,
        media_type: MediaType | str,
        media_id: str,
        enqueue: MediaPlayerEnqueue | None = None,
        **kwargs: Any,
    ) -> None:
        """Queue a URL. media_type is ignored: yt-dlp works it out from the URL."""
        if enqueue == MediaPlayerEnqueue.REPLACE:
            await self._post("/clear")
        play_now = enqueue not in (MediaPlayerEnqueue.ADD, MediaPlayerEnqueue.NEXT)
        if enqueue == MediaPlayerEnqueue.NEXT:
            _LOGGER.debug("yt-fifo has no insert-after-current; appending instead")
        result = await self._post(
            "/queue",
            {"url": media_id, "play_now": play_now, "playlist": kwargs.get("extra", {}).get("playlist", False)},
            timeout=QUEUE_TIMEOUT,
        )
        titles = [t.get("title") for t in result.get("enqueued", []) if t.get("title")]
        _LOGGER.info("yt-fifo %s: %s", "playing now" if play_now else "queued", titles or media_id)
        await self.async_update()

