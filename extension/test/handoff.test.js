import test from 'node:test';
import assert from 'node:assert/strict';
import { buildCandidateHandoff, candidateReferer } from '../handoff.js';

function handoffParams(value) {
  const parsed = new URL(value);
  return new URLSearchParams(parsed.hash.slice(1));
}

test('uses an exact document context before the active top-level tab URL', () => {
  const candidate = {
    url: 'https://cdn.example.test/master.m3u8?sig=secret',
    requestContext: {
      url: 'https://site.example.test/embed/player?id=7',
      source: 'document-url',
    },
  };
  assert.equal(
    candidateReferer(candidate, 'https://site.example.test/watch?id=7'),
    'https://site.example.test/embed/player?id=7',
  );
  const params = handoffParams(buildCandidateHandoff(candidate, 'https://site.example.test/watch?id=7'));
  assert.equal(params.get('source'), candidate.url);
  assert.equal(params.get('referer'), 'https://site.example.test/embed/player?id=7');
  assert.equal(params.get('from'), 'extension');
});

test('replays the Referer that the browser actually sent', () => {
  const candidate = {
    url: 'https://cdn.example.test/master.m3u8?sig=secret',
    requestHeaders: { referer: 'https://site.example.test/watch?id=7' },
    requestContext: {
      url: 'https://site.example.test/embed/player?id=7',
      source: 'document-url',
    },
  };
  assert.equal(
    candidateReferer(candidate, 'https://site.example.test/other'),
    'https://site.example.test/watch?id=7',
  );
});

test('prefers the active tab over weak initiator or origin context', () => {
  const currentTab = 'https://site.example.test/watch?id=7';
  assert.equal(candidateReferer({
    requestContext: {
      url: 'https://site.example.test/embed/player?id=7',
      source: 'initiator',
    },
  }, currentTab), currentTab);
  assert.equal(candidateReferer({
    requestContext: {
      url: 'https://cdn.example.test',
      source: 'origin',
    },
  }, currentTab), currentTab);
});

test('uses weak context only when the active tab URL is unavailable', () => {
  assert.equal(candidateReferer({
    requestContext: {
      url: 'https://site.example.test/embed/player?id=7',
      source: 'initiator',
    },
  }, 'chrome://newtab/'), 'https://site.example.test/embed/player?id=7');
  assert.equal(candidateReferer({
    requestContext: {
      url: 'https://site.example.test',
      source: 'origin',
    },
  }), 'https://site.example.test/');
});

test('falls back to a safe active tab URL when context is absent or unsafe', () => {
  const candidate = { url: 'https://cdn.example.test/master.m3u8' };
  assert.equal(
    candidateReferer(candidate, 'https://site.example.test/watch?id=7#player'),
    'https://site.example.test/watch?id=7',
  );
  const params = handoffParams(buildCandidateHandoff(candidate, 'chrome://newtab/'));
  assert.equal(params.get('referer'), '');
});

test('rejects an unsafe candidate source before opening riflo', () => {
  assert.equal(buildCandidateHandoff({ url: 'javascript:alert(1)' }, 'https://site.example.test/'), '');
});

test('passes captured Origin and User-Agent through the handoff hash', () => {
  const params = handoffParams(buildCandidateHandoff({
    url: 'https://cdn.example.test/master.m3u8?sig=secret',
    requestHeaders: {
      origin: 'https://site.example.test',
      user_agent: 'riflo-test-agent/1.0',
      cookie: 'session=secret',
    },
  }, 'https://site.example.test/watch'));
  assert.equal(params.get('origin'), 'https://site.example.test');
  assert.equal(params.get('user_agent'), 'riflo-test-agent/1.0');
  assert.equal(params.has('cookie'), false);
});
