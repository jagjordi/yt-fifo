#!/bin/sh
# Stand-in for ffmpeg in tests. Emits s16le-sized zero bytes at real-time pace
# (192000 B/s = 48 kHz stereo) for FAKE_SECONDS seconds, minus the -ss offset.
# Prints its argv to stderr so tests can assert on -ss.
echo "fake-ffmpeg $*" >&2
start=0
prev=""
for a in "$@"; do
  if [ "$prev" = "-ss" ]; then start=$a; fi
  prev=$a
done
secs=${FAKE_SECONDS:-5}
remaining=$(awk -v s="$secs" -v st="$start" 'BEGIN{r=s-st; if (r<0) r=0; printf "%d", r*10}')
i=0
while [ "$i" -lt "$remaining" ]; do
  head -c 19200 /dev/zero
  sleep 0.1
  i=$((i+1))
done
