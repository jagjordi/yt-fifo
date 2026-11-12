# WizeStream: "Play on home audio" / "Queue on home audio"

[WizeStream](https://github.com/jagjordi/WizeStream) is a NewPipe fork. `send-to-yt-fifo.patch`
adds two entries to the menu that opens when you long-press a video, and the same two to the
"Cast to TV" picker behind the Cast button on the video page:

- **Play on home audio** interrupts whatever yt-fifo is playing and plays this video's audio on
  the Snapcast speakers (`POST /queue` with `play_now: true`).
- **Queue on home audio** appends it to yt-fifo's queue.

The entries appear once the yt-fifo server URL is set in *Settings → Video and audio → Home audio
server (yt-fifo)*, e.g. `http://192.168.1.181:8888`, for streams of any service (yt-dlp handles
all of them). A toast reports the resolved title, or yt-dlp's error when the server rejects the
URL. Playing from the Cast button pauses local playback, like casting does.

Because yt-fifo is reached over plain HTTP on the LAN, the patch sets
`android:usesCleartextTraffic="true"` on the application; Android 9+ blocks cleartext otherwise.

The patch is written on top of TubeBeam's
[`send-to-tubebeam.patch`](https://github.com/jagjordi/tubebeam/tree/main/wizestream), which it
extends (both share the fixed rows at the top of the Cast picker). Apply that one first.

## Build

```
wizestream/build.sh       # clones WizeStream, applies both patches, builds a debug APK in Docker
```

The build uses TubeBeam's Android build image (JDK 21 + SDK), built from the tubebeam checkout that
`TUBEBEAM_DIR` points at (default: a sibling directory, cloned if missing). The debug build installs
as a separate app next to the official WizeStream.
