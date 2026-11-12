// YouTube URL handling shared by the service worker and the tests.
// Returns {url, playlist} for a video or playlist URL, or null if the URL is
// not something yt-fifo can play.
function parseYouTube(raw) {
  let u;
  try { u = new URL(raw); } catch { return null; }
  const host = u.hostname.replace(/^www\.|^m\.|^music\./, '');
  if (host !== 'youtube.com' && host !== 'youtu.be' && host !== 'youtube-nocookie.com') return null;
  const list = u.searchParams.get('list');
  let id = null;
  if (host === 'youtu.be') {
    id = u.pathname.slice(1).split('/')[0];
  } else if (u.pathname === '/watch') {
    id = u.searchParams.get('v');
  } else {
    const m = u.pathname.match(/^\/(?:shorts|live|embed|v)\/([A-Za-z0-9_-]{11})/);
    if (m) id = m[1];
  }
  if (id && /^[A-Za-z0-9_-]{11}$/.test(id)) {
    const url = `https://www.youtube.com/watch?v=${id}` + (list ? `&list=${encodeURIComponent(list)}` : '');
    return { url, video: id, list };
  }
  if (u.pathname === '/playlist' && list) {
    return { url: `https://www.youtube.com/playlist?list=${encodeURIComponent(list)}`, video: null, list };
  }
  return null;
}

if (typeof module !== 'undefined') module.exports = { parseYouTube };
