import test from 'node:test';
import assert from 'node:assert/strict';
import {
  buildHandoffURL,
  createCandidate,
  displayURL,
  formatDetectionSources,
  normalizeHTTPURL,
  normalizeRequestHeaders,
  normalizeRequestContext,
  REQUEST_CONTEXT_SOURCE_DOCUMENT,
} from '../model.js';

test('display URL removes query, fragment, and URL userinfo', () => {
  assert.equal(
    displayURL('https://user:secret@cdn.example.test/master.m3u8?token=secret#part'),
    'https://cdn.example.test/master.m3u8',
  );
});

test('candidate model contains protocol, host, and timestamps', () => {
  const candidate = createCandidate('https://cdn.example.test/master.m3u8?sig=1', 1234);
  assert.equal(candidate.host, 'cdn.example.test');
  assert.equal(candidate.protocol, 'HLS');
  assert.equal(candidate.firstSeen, 1234);
  assert.equal(candidate.lastSeen, 1234);
  assert.equal(candidate.displayUrl, 'https://cdn.example.test/master.m3u8');
  assert.deepEqual(candidate.detectionSources, []);
});

test('candidate model names and formats detection sources', () => {
  const candidate = createCandidate('https://cdn.example.test/master.m3u8', 1234, {
    contentType: 'application/vnd.apple.mpegurl',
    detectionSources: ['url-suffix', 'content-type', 'url-suffix'],
  });
  assert.equal(candidate.contentType, 'application/vnd.apple.mpegurl');
  assert.deepEqual(candidate.detectionSources, ['url-suffix', 'content-type']);
  assert.equal(formatDetectionSources(candidate.detectionSources), 'URL 后缀、Content-Type');
});

test('candidate model keeps a redacted request context beside the full URL', () => {
  const candidate = createCandidate('https://cdn.example.test/master.m3u8?sig=1', 1234, {
    requestContext: {
      url: 'https://user:secret@site.example.test/watch?id=7#player',
      source: REQUEST_CONTEXT_SOURCE_DOCUMENT,
    },
  });
  assert.deepEqual(candidate.requestContext, {
    url: 'https://site.example.test/watch?id=7',
    displayUrl: 'https://site.example.test/watch',
    source: REQUEST_CONTEXT_SOURCE_DOCUMENT,
  });
  assert.equal(normalizeHTTPURL('javascript:alert(1)'), '');
  assert.equal(normalizeRequestContext({ url: 'file:///tmp/page', source: 'origin' }), null);
});

test('candidate model keeps only safe request headers', () => {
  const candidate = createCandidate('https://cdn.example.test/master.m3u8', 1234, {
    requestHeaders: [
      { name: 'Origin', value: 'https://site.example.test/' },
      { name: 'User-Agent', value: 'riflo-test-agent' },
      { name: 'Cookie', value: 'session=secret' },
    ],
  });
  assert.deepEqual(candidate.requestHeaders, {
    origin: 'https://site.example.test',
    user_agent: 'riflo-test-agent',
  });
  assert.deepEqual(normalizeRequestHeaders({ Cookie: 'session=secret' }), null);
});

test('handoff URL encodes source and referer without a cookie field', () => {
  const source = 'https://cdn.example.test/master.m3u8?sig=a+b&redirect=%2Fv';
  const referer = 'https://site.example.test/watch?id=7&name=one two';
  const handoff = buildHandoffURL(source, referer);
  const parsed = new URL(handoff);
  const params = new URLSearchParams(parsed.hash.slice(1));

  assert.equal(parsed.origin, 'http://127.0.0.1:8787');
  assert.equal(params.get('source'), source);
  assert.equal(params.get('referer'), referer);
  assert.equal(params.get('from'), 'extension');
  assert.equal(params.has('cookie'), false);
  assert.match(handoff, /#source=/);
  assert.match(handoff, /%3A%2F%2F/);
});
