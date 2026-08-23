import test from 'node:test';
import assert from 'node:assert/strict';
import {
  DETECTION_SOURCE_CONTENT_TYPE,
  DETECTION_SOURCE_URL_SUFFIX,
  detectHLS,
  detectHLSRequest,
  isHLSContentType,
  isMediaSegmentURL,
  requestContextFromDetails,
} from '../detector.js';
import {
  REQUEST_CONTEXT_SOURCE_DOCUMENT,
  REQUEST_CONTEXT_SOURCE_INITIATOR,
  REQUEST_CONTEXT_SOURCE_ORIGIN,
} from '../model.js';

test('detects an m3u8 URL and preserves its full URL', () => {
  const url = 'https://cdn.example.test/video/master.m3u8?token=a%2Bb#variant';
  const candidate = detectHLS({ tabId: 3, url, responseHeaders: [] });
  assert.deepEqual(candidate, {
    url,
    contentType: '',
    detectionSources: [DETECTION_SOURCE_URL_SUFFIX],
  });
});

test('detects HLS content types when the URL has no extension', () => {
  assert.equal(isHLSContentType('application/vnd.apple.mpegurl; charset=utf-8'), true);
  assert.equal(isHLSContentType('APPLICATION/X-MPEGURL'), true);
  const candidate = detectHLS({
    tabId: 3,
    url: 'https://cdn.example.test/video/manifest?sig=abc',
    responseHeaders: [{ name: 'Content-Type', value: 'application/x-mpegurl; charset=utf-8' }],
  });
  assert.equal(candidate.url, 'https://cdn.example.test/video/manifest?sig=abc');
  assert.equal(candidate.contentType, 'application/x-mpegurl; charset=utf-8');
  assert.deepEqual(candidate.detectionSources, [DETECTION_SOURCE_CONTENT_TYPE]);
});

test('reports both detection sources when URL and Content-Type match', () => {
  const candidate = detectHLS({
    tabId: 3,
    url: 'https://cdn.example.test/video/master.m3u8',
    responseHeaders: [{ name: 'content-type', value: 'application/vnd.apple.mpegurl' }],
  });
  assert.deepEqual(candidate.detectionSources, [
    DETECTION_SOURCE_URL_SUFFIX,
    DETECTION_SOURCE_CONTENT_TYPE,
  ]);
});

test('request fallback only accepts explicit HTTP(S) .m3u8 URLs', () => {
  const candidate = detectHLSRequest({
    tabId: 3,
    url: 'https://cdn.example.test/video/master.m3u8?token=abc',
  });
  assert.deepEqual(candidate.detectionSources, [DETECTION_SOURCE_URL_SUFFIX]);
  assert.equal(detectHLSRequest({ tabId: 3, url: 'https://cdn.example.test/video/segment.ts' }), null);
  assert.equal(detectHLSRequest({ tabId: 3, url: 'https://cdn.example.test/video/manifest' }), null);
  assert.equal(detectHLSRequest({ tabId: 3, url: 'file:///tmp/master.m3u8' }), null);
});

test('rejects non-http URLs, requests without a tab, and non-HLS media', () => {
  assert.equal(detectHLS({ url: 'https://cdn.example.test/master.m3u8' }), null);
  assert.equal(detectHLS({ tabId: -1, url: 'https://cdn.example.test/master.m3u8' }), null);
  assert.equal(detectHLS({ tabId: 2, url: 'file:///tmp/master.m3u8' }), null);
  assert.equal(detectHLS({ tabId: 2, url: 'https://cdn.example.test/video.mp4' }), null);
});

test('excludes common HLS segments even when their content type is misleading', () => {
  for (const suffix of ['.ts', '.m4s', '.aac']) {
    const url = `https://cdn.example.test/video/segment${suffix}?token=abc`;
    assert.equal(isMediaSegmentURL(url), true);
    assert.equal(detectHLS({
      tabId: 2,
      url,
      responseHeaders: [{ name: 'content-type', value: 'application/vnd.apple.mpegurl' }],
    }), null);
  }
});

test('prefers documentUrl over initiator and origin and removes credentials/fragments', () => {
  assert.deepEqual(requestContextFromDetails({
    documentUrl: 'https://user:secret@site.example.test/watch?id=7#player',
    initiator: 'https://initiator.example.test/page',
    origin: 'https://origin.example.test/',
  }), {
    url: 'https://site.example.test/watch?id=7',
    source: REQUEST_CONTEXT_SOURCE_DOCUMENT,
  });
});

test('falls back from documentUrl to initiator, then origin aliases', () => {
  assert.deepEqual(requestContextFromDetails({
    documentUrl: 'javascript:alert(1)',
    initiator: 'https://initiator.example.test/page#part',
    origin: 'https://origin.example.test/',
  }), {
    url: 'https://initiator.example.test/page',
    source: REQUEST_CONTEXT_SOURCE_INITIATOR,
  });
  assert.deepEqual(requestContextFromDetails({
    documentUrl: 'data:text/html,unsafe',
    initiator: 'about:blank',
    originUrl: 'https://origin.example.test/frame?x=1#part',
  }), {
    url: 'https://origin.example.test/frame?x=1',
    source: REQUEST_CONTEXT_SOURCE_ORIGIN,
  });
});

test('does not extract non-http request context values', () => {
  assert.equal(requestContextFromDetails({
    documentUrl: 'file:///tmp/page.html',
    initiator: 'chrome://newtab/',
    origin: 'null',
  }), null);
  assert.equal(requestContextFromDetails(null), null);
});
