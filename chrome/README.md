# Chrome extension

Right-click a YouTube video and send it to a yt-fifo server.

```
chrome/
  manifest.json   Manifest V3
  background.js   context menu + POST /queue + notifications
  urls.js         YouTube URL parsing (shared with urls.test.js)
  options.html/js server URL, host-permission request, "Test" button
  icons/          resized from ../logo.png
```

## What you get

| Where you right-click | Menu entries |
|---|---|
| a link to a video (home feed, sidebar, search, any site) | **Play on home audio**, **Queue on home audio** |
| a link with a `list=` parameter, or a playlist page link | **Queue whole playlist on home audio** |
| the page of a watch / shorts / live page (not on the player itself) | the same entries for the video you are on |

"Play" interrupts what is playing and keeps the rest of the queue (yt-fifo's
`play_now`). A notification shows the resolved title, or yt-dlp's error for
an unavailable video. The toolbar icon opens the server's web UI.

YouTube draws its own menu on top of the video player, so on a watch page
right-click next to the player (title, description, comments) or use
shift+right-click on the player to get Chrome's menu.

## Install

1. `chrome://extensions`, enable *Developer mode*, *Load unpacked*, pick this
   `chrome/` folder.
2. The options page opens: enter the server URL, e.g.
   `http://192.168.1.181:8888`, and *Save*. Chrome asks for permission to
   talk to that address (the extension declares no fixed hosts, it asks for
   the one you configure). *Test* calls `/health`.

Links from `youtube.com`, `m.`, `music.`, `youtu.be`, `youtube-nocookie.com`,
shorts, live, embed and playlist URLs are recognised; `t=` and `si=`
parameters are dropped, `list=` is kept so "Queue whole playlist" works from
a watch URL too.

## Test

```sh
node chrome/urls.test.js
```
