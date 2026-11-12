importScripts('urls.js');

const VIDEO_PATTERNS = [
  '*://*.youtube.com/watch*', '*://*.youtube.com/shorts/*', '*://*.youtube.com/live/*',
  '*://*.youtube.com/embed/*', '*://youtu.be/*', '*://*.youtube-nocookie.com/embed/*',
];
const PLAYLIST_PATTERNS = ['*://*.youtube.com/*list=*', '*://*.youtube.com/playlist*', '*://youtu.be/*list=*'];

const MENU = {
  play: { title: 'Play on home audio', body: { play_now: true } },
  queue: { title: 'Queue on home audio', body: { play_now: false } },
  playlist: { title: 'Queue whole playlist on home audio', body: { play_now: false, playlist: true } },
};

chrome.runtime.onInstalled.addListener(async () => {
  await chrome.contextMenus.removeAll();
  for (const id of ['play', 'queue']) {
    // On links to videos anywhere, and on a watch page itself (page, frame,
    // video element). YouTube replaces the context menu on its own player,
    // so there right-click the page around it, or shift+right-click.
    chrome.contextMenus.create({ id, title: MENU[id].title, contexts: ['link'], targetUrlPatterns: VIDEO_PATTERNS });
    chrome.contextMenus.create({ id: `page-${id}`, title: MENU[id].title, contexts: ['page', 'frame', 'video'], documentUrlPatterns: VIDEO_PATTERNS });
  }
  chrome.contextMenus.create({ id: 'playlist', title: MENU.playlist.title, contexts: ['link'], targetUrlPatterns: PLAYLIST_PATTERNS });
  chrome.contextMenus.create({ id: 'page-playlist', title: MENU.playlist.title, contexts: ['page', 'frame', 'video'], documentUrlPatterns: PLAYLIST_PATTERNS });
  const { server } = await chrome.storage.sync.get('server');
  if (!server) chrome.runtime.openOptionsPage();
});

chrome.contextMenus.onClicked.addListener((info) => {
  const action = info.menuItemId.replace(/^page-/, '');
  const target = parseYouTube(info.linkUrl || info.frameUrl || info.pageUrl || '');
  if (!target) return notify('Not a YouTube video', info.linkUrl || info.pageUrl || '');
  send(action, target);
});

chrome.action.onClicked.addListener(async () => {
  const { server } = await chrome.storage.sync.get('server');
  if (server) chrome.tabs.create({ url: server });
  else chrome.runtime.openOptionsPage();
});

async function send(action, target) {
  const { server } = await chrome.storage.sync.get('server');
  if (!server) {
    chrome.runtime.openOptionsPage();
    return notify('yt-fifo server not set', 'Enter the server URL in the extension options.');
  }
  const body = { url: target.url, ...MENU[action].body };
  const id = notify(action === 'play' ? 'Sending to home audio…' : 'Queueing on home audio…', target.url);
  try {
    const r = await fetch(`${server.replace(/\/+$/, '')}/queue`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
    });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || `${r.status} ${r.statusText}`);
    const tracks = j.enqueued || [];
    const title = tracks[0]?.title || target.url;
    const extra = tracks.length > 1 ? ` (+${tracks.length - 1} more)` : '';
    notify(action === 'play' ? 'Playing on home audio' : `Queued on home audio (#${j.queue_length ?? '?'})`, title + extra, id);
  } catch (e) {
    notify('Home audio failed', e.message || String(e), id);
  }
}

function notify(title, message, id) {
  const nid = id || `ytfifo-${Date.now()}`;
  const opts = { type: 'basic', iconUrl: 'icons/icon128.png', title, message: String(message).slice(0, 300) };
  if (id) chrome.notifications.update(nid, opts); else chrome.notifications.create(nid, opts);
  return nid;
}
