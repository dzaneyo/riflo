import test from 'node:test';
import assert from 'node:assert/strict';
import { buildCandidateHandoff, candidateReferer } from '../handoff.js';

function handoffParams(value) {
  const parsed = new URL(value);
  return new URLSearchParams(parsed.hash.slice(1));
}

test('uses the candidate request context before the active top-level tab URL', () => {
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
