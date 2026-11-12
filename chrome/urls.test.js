// node chrome/urls.test.js
const assert = require('node:assert');
const { parseYouTube } = require('./urls.js');
const cases = [
  ['https://www.youtube.com/watch?v=dCt6jXEVx0A&t=12s', 'https://www.youtube.com/watch?v=dCt6jXEVx0A', null],
  ['https://youtu.be/dCt6jXEVx0A?si=abc', 'https://www.youtube.com/watch?v=dCt6jXEVx0A', null],
  ['https://m.youtube.com/watch?v=dCt6jXEVx0A', 'https://www.youtube.com/watch?v=dCt6jXEVx0A', null],
  ['https://music.youtube.com/watch?v=dCt6jXEVx0A&list=RDAMVM', 'https://www.youtube.com/watch?v=dCt6jXEVx0A&list=RDAMVM', 'RDAMVM'],
  ['https://www.youtube.com/shorts/dCt6jXEVx0A', 'https://www.youtube.com/watch?v=dCt6jXEVx0A', null],
  ['https://www.youtube.com/embed/dCt6jXEVx0A', 'https://www.youtube.com/watch?v=dCt6jXEVx0A', null],
  ['https://www.youtube.com/playlist?list=PL123', 'https://www.youtube.com/playlist?list=PL123', 'PL123'],
  ['https://www.youtube.com/', null],
  ['https://www.youtube.com/@channel', null],
  ['https://example.com/watch?v=dCt6jXEVx0A', null],
  ['not a url', null],
];
for (const [input, url, list] of cases) {
  const got = parseYouTube(input);
  if (url === null) assert.strictEqual(got, null, input);
  else { assert.strictEqual(got.url, url, input); assert.strictEqual(got.list, list, input); }
}
console.log(`${cases.length} cases ok`);
