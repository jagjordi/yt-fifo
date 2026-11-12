const input = document.getElementById('server');
const msg = document.getElementById('msg');

chrome.storage.sync.get('server').then(({ server }) => { if (server) input.value = server; });

function normalize() {
  let v = input.value.trim().replace(/\/+$/, '');
  if (v && !/^https?:\/\//.test(v)) v = 'http://' + v;
  return v;
}

document.getElementById('save').onclick = async () => {
  const server = normalize();
  if (!server) { msg.textContent = 'Enter a URL'; return; }
  let origin;
  try { origin = new URL(server).origin + '/*'; } catch { msg.textContent = 'Not a valid URL'; return; }
  // Host permission is what lets the service worker POST to the server
  // without CORS; it must be requested from a user gesture, i.e. here.
  const granted = await chrome.permissions.request({ origins: [origin] });
  if (!granted) { msg.textContent = 'Permission for ' + origin + ' was not granted'; return; }
  await chrome.storage.sync.set({ server });
  input.value = server;
  msg.textContent = 'Saved';
};

document.getElementById('test').onclick = async () => {
  const server = normalize();
  msg.textContent = 'Testing…';
  try {
    const r = await fetch(server + '/health');
    const j = await r.json();
    msg.textContent = `OK: yt-fifo ${j.version || '?'}, ${j.state}, output ${j.output}`;
  } catch (e) {
    msg.textContent = 'Failed: ' + (e.message || e) + ' (save first to grant permission)';
  }
};
