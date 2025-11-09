#!/usr/bin/env python3
# -*- coding: utf-8 -*-
#
# Simple YouTube→FIFO streamer with a queue and HTTP API.
# Output format: raw PCM s16le, 48kHz, stereo (continuous stream).
# API: FastAPI (see endpoints below).

import asyncio
import os
import signal
import sys
import time
import contextlib
from typing import Optional, List, Deque
from collections import deque

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, AnyUrl
import yt_dlp

FIFO_PATH = os.environ.get("AUDIO_FIFO", "/tmp/ytaudio.fifo")
SAMPLE_RATE = int(os.environ.get("AUDIO_RATE", "48000"))
CHANNELS = int(os.environ.get("AUDIO_CHANNELS", "2"))
SAMPLE_FMT = "s16le"  # 16-bit signed PCM little-endian

# ---------- Data models ----------
class QueueItem(BaseModel):
  url: AnyUrl
  added_ts: float = time.time()

class QueueRequest(BaseModel):
  url: AnyUrl

class EnqueueManyRequest(BaseModel):
  urls: List[AnyUrl]

# ---------- App state ----------
app = FastAPI(title="YouTube FIFO Streamer", version="1.0")
queue: Deque[QueueItem] = deque()
current: Optional[QueueItem] = None
stop_current = asyncio.Event()
paused = asyncio.Event()
paused.clear()  # not paused by default
shutdown_event = asyncio.Event()

# ---------- Helpers ----------
def ensure_fifo(path: str) -> None:
  if os.path.exists(path):
    if not stat_is_fifo(path):
      raise RuntimeError(f"Path exists but is not a FIFO: {path}")
  else:
    os.mkfifo(path, 0o666)

def stat_is_fifo(path: str) -> bool:
  st = os.stat(path)
  return (st.st_mode & 0o170000) == 0o010000

async def resolve_bestaudio_url(youtube_url: str) -> str:
  ydl_opts = {
    "quiet": True,
    "nocheckcertificate": True,
    "format": "bestaudio/best",
    "noplaylist": True,
    "extract_flat": False,
    "skip_download": True,
  }
  loop = asyncio.get_running_loop()
  def _extract():
    with yt_dlp.YoutubeDL(ydl_opts) as ydl:
      info = ydl.extract_info(youtube_url, download=False)
      return info
  info = await loop.run_in_executor(None, _extract)
  # Prefer a direct URL if present; fall back to webpage_url (yt-dlp handles both)
  stream_url = info.get("url") or info.get("webpage_url")
  if not stream_url:
    raise RuntimeError("Could not resolve audio URL")
  return stream_url

async def run_ffmpeg_to_fifo(stream_url: str, fifo_path: str) -> int:
  """
  Spawns ffmpeg to decode the given input URL to raw PCM and writes to FIFO.
  Returns ffmpeg's exit code.
  """
  # We let ffmpeg write directly to the FIFO path.
  # Opening for write will block until a reader opens the FIFO.
  cmd = [
    "ffmpeg",
    "-nostdin",
    "-hide_banner",
    "-loglevel", "warning",
    "-i", stream_url,
    "-vn",
    "-ac", str(CHANNELS),
    "-ar", str(SAMPLE_RATE),
    "-f", SAMPLE_FMT,
    fifo_path
  ]

  # Important: ffmpeg will open the FIFO for writing itself. Ensure a reader exists!
  proc = await asyncio.create_subprocess_exec(*cmd)
  try:
    await proc.wait()
  except asyncio.CancelledError:
    with contextlib.suppress(ProcessLookupError):
      proc.terminate()
    raise
  return proc.returncode

async def player_loop():
  global current
  ensure_fifo(FIFO_PATH)

  # Keep FIFO open writer-side too (prevents EOF when reader closes briefly).
  # We open a dummy writer FD in non-blocking mode.
  dummy_fd = os.open(FIFO_PATH, os.O_WRONLY | os.O_NONBLOCK)
  try:
    while not shutdown_event.is_set():
      if paused.is_set():
        await asyncio.sleep(0.2)
        continue

      if not queue:
        await asyncio.sleep(0.2)
        continue

      current = queue.popleft()
      stop_current.clear()

      try:
        stream_url = await resolve_bestaudio_url(str(current.url))
      except Exception as e:
        print(f"[resolver] failed for {current.url}: {e}", file=sys.stderr)
        current = None
        continue

      print(f"[player] now playing: {current.url}")
      # Run ffmpeg; allow skipping by setting stop_current
      ff_task = asyncio.create_task(run_ffmpeg_to_fifo(stream_url, FIFO_PATH))

      done, pending = await asyncio.wait(
        {ff_task, asyncio.create_task(stop_current.wait()), asyncio.create_task(shutdown_event.wait())},
        return_when=asyncio.FIRST_COMPLETED
      )

      if ff_task in done:
        rc = ff_task.result()
        if rc != 0:
          print(f"[ffmpeg] exited with code {rc}", file=sys.stderr)
      else:
        # Need to stop ffmpeg if we were signaled (skip or shutdown)
        ff_task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
          await ff_task

      current = None

  finally:
    os.close(dummy_fd)

# ---------- API ----------
@app.get("/health")
def health():
  return {"status": "ok", "fifo": FIFO_PATH, "paused": paused.is_set()}

@app.get("/queue")
def get_queue():
  return {
    "now_playing": current.model_dump() if current else None,
    "queued": [q.model_dump() for q in list(queue)]
  }

@app.post("/queue")
def add_to_queue(req: QueueRequest):
  item = QueueItem(url=req.url)
  queue.append(item)
  return {"enqueued": str(req.url), "queue_length": len(queue)}

@app.post("/queue/batch")
def add_many(req: EnqueueManyRequest):
  for u in req.urls:
    queue.append(QueueItem(url=u))
  return {"enqueued": len(req.urls), "queue_length": len(queue)}

@app.post("/skip")
def skip():
  if current is None:
    raise HTTPException(status_code=409, detail="Nothing is playing.")
  stop_current.set()
  return {"skipped": str(current.url)}

@app.post("/pause")
def pause():
  paused.set()
  return {"paused": True}

@app.post("/resume")
def resume():
  paused.clear()
  return {"paused": False}

@app.post("/clear")
def clear():
  queue.clear()
  return {"cleared": True}

# ---------- Startup / Shutdown ----------
@app.on_event("startup")
async def on_start():
  # Start player loop
  app.state.player = asyncio.create_task(player_loop())

@app.on_event("shutdown")
async def on_shutdown():
  shutdown_event.set()
  try:
    await app.state.player
  except Exception:
    pass

# ---------- Entrypoint ----------
if __name__ == "__main__":
  # Lazy imports for uvicorn
  import uvicorn
  import contextlib

  # Handle SIGTERM gracefully
  def _handle_sigterm(*_):
    shutdown_event.set()
  signal.signal(signal.SIGTERM, _handle_sigterm)

  uvicorn.run("yt_fifo_server:app",
              host="0.0.0.0",
              port=int(os.environ.get("PORT", "8080")),
              log_level="info")

