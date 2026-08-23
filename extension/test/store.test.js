import test from 'node:test';
import assert from 'node:assert/strict';
import { createCandidate } from '../model.js';
import {
  CANDIDATE_TTL_MS,
  MAX_CANDIDATES_PER_TAB,
  STORAGE_KEY,
  createCandidateStore,
} from '../store.js';

function memoryStorage() {
  let value = {};
  return {
    async get(key) {
      return { [key]: structuredClone(value[key]) };
    },
    async set(next) {
      value = { ...value, ...structuredClone(next) };
    },
    snapshot() {
      return structuredClone(value);
    },
  };
}

test('deduplicates an exact URL and updates its recent timestamp', async () => {
  const storage = memoryStorage();
  const store = createCandidateStore(storage);
  await store.add(4, createCandidate('https://cdn.example.test/master.m3u8?sig=1', 100), 100);
  await store.add(4, createCandidate('https://cdn.example.test/master.m3u8?sig=1', 200), 200);
  const candidates = await store.get(4, 200);

  assert.equal(candidates.length, 1);
  assert.equal(candidates[0].firstSeen, 100);
  assert.equal(candidates[0].lastSeen, 200);
});

test('merges detection sources and preserves a useful content type', async () => {
  const storage = memoryStorage();
  const store = createCandidateStore(storage);
  await store.add(4, createCandidate('https://cdn.example.test/master.m3u8?sig=1', 100, {
    detectionSources: ['url-suffix'],
  }), 100);
  await store.add(4, createCandidate('https://cdn.example.test/master.m3u8?sig=1', 200, {
    contentType: 'application/vnd.apple.mpegurl',
    detectionSources: ['content-type'],
  }), 200);
  let candidates = await store.get(4, 200);

  assert.deepEqual(candidates[0].detectionSources, ['url-suffix', 'content-type']);
  assert.equal(candidates[0].contentType, 'application/vnd.apple.mpegurl');

  await store.add(4, createCandidate('https://cdn.example.test/master.m3u8?sig=1', 300, {
    detectionSources: ['url-suffix'],
  }), 300);
  candidates = await store.get(4, 300);
  assert.deepEqual(candidates[0].detectionSources, ['url-suffix', 'content-type']);
  assert.equal(candidates[0].contentType, 'application/vnd.apple.mpegurl');
});

test('merges request context by strength and never lets an empty value erase it', async () => {
  const storage = memoryStorage();
  const store = createCandidateStore(storage);
  const url = 'https://cdn.example.test/master.m3u8?sig=1';
  await store.add(4, createCandidate(url, 100, {
    requestContext: {
      url: 'https://site.example.test/watch?id=7',
      source: 'document-url',
    },
  }), 100);
  await store.add(4, createCandidate(url, 200, {
    requestContext: {
      url: 'https://origin.example.test/',
      source: 'origin',
    },
  }), 200);
  let candidates = await store.get(4, 200);
  assert.equal(candidates[0].requestContext.url, 'https://site.example.test/watch?id=7');
  assert.equal(candidates[0].requestContext.source, 'document-url');

  await store.add(4, createCandidate(url, 300), 300);
  candidates = await store.get(4, 300);
  assert.equal(candidates[0].requestContext.url, 'https://site.example.test/watch?id=7');

  await store.add(4, createCandidate(url, 400, {
    requestContext: {
      url: 'https://initiator.example.test/frame',
      source: 'initiator',
    },
  }), 400);
  candidates = await store.get(4, 400);
  assert.equal(candidates[0].requestContext.url, 'https://site.example.test/watch?id=7');
});

test('upgrades an origin context to a later initiator context', async () => {
  const storage = memoryStorage();
  const store = createCandidateStore(storage);
  const url = 'https://cdn.example.test/master.m3u8';
  await store.add(5, createCandidate(url, 100, {
    requestContext: { url: 'https://origin.example.test/', source: 'origin' },
  }), 100);
  await store.add(5, createCandidate(url, 200, {
    requestContext: { url: 'https://site.example.test/embed', source: 'initiator' },
  }), 200);
  const [candidate] = await store.get(5, 200);
  assert.equal(candidate.requestContext.url, 'https://site.example.test/embed');
  assert.equal(candidate.requestContext.source, 'initiator');
});

test('merges safe request headers and discards forbidden headers', async () => {
  const storage = memoryStorage();
  const store = createCandidateStore(storage);
  const url = 'https://cdn.example.test/master.m3u8';
  await store.add(5, createCandidate(url, 100, {
    requestHeaders: [{ name: 'Origin', value: 'https://site.example.test/' }],
  }), 100);
  await store.add(5, createCandidate(url, 200, {
    requestHeaders: [
      { name: 'User-Agent', value: 'riflo-test-agent' },
      { name: 'Cookie', value: 'session=secret' },
      { name: 'Authorization', value: 'Bearer secret' },
    ],
  }), 200);

  const [candidate] = await store.get(5, 200);
  assert.deepEqual(candidate.requestHeaders, {
    origin: 'https://site.example.test',
    user_agent: 'riflo-test-agent',
  });
  assert.equal('cookie' in candidate.requestHeaders, false);
  assert.equal('authorization' in candidate.requestHeaders, false);
});

test('serializes concurrent read-modify-write operations across tabs', async () => {
  const storage = memoryStorage();
  const store = createCandidateStore(storage);
  await Promise.all(Array.from({ length: 12 }, (_, index) => (
    store.add(index, createCandidate(`https://cdn.example.test/${index}.m3u8`, index + 1), index + 1)
  )));

  for (let index = 0; index < 12; index += 1) {
    assert.equal((await store.get(index, index + 1)).length, 1);
  }
  assert.equal(Object.keys(storage.snapshot()[STORAGE_KEY]).length, 12);
});

test('expires candidates after the TTL', async () => {
  const storage = memoryStorage();
  const store = createCandidateStore(storage, { ttlMs: CANDIDATE_TTL_MS });
  await store.add(1, createCandidate('https://cdn.example.test/old.m3u8', 0), 0);
  assert.equal((await store.get(1, CANDIDATE_TTL_MS - 1)).length, 1);
  assert.equal((await store.get(1, CANDIDATE_TTL_MS + 1)).length, 0);
});

test('keeps at most 20 candidates per tab and retains the newest ones', async () => {
  const storage = memoryStorage();
  const store = createCandidateStore(storage);
  for (let index = 0; index < MAX_CANDIDATES_PER_TAB + 5; index += 1) {
    await store.add(8, createCandidate(`https://cdn.example.test/${index}.m3u8`, index + 1), index + 1);
  }
  const candidates = await store.get(8, MAX_CANDIDATES_PER_TAB + 5);

  assert.equal(candidates.length, MAX_CANDIDATES_PER_TAB);
  assert.equal(candidates[0].url, 'https://cdn.example.test/24.m3u8');
  assert.equal(candidates.at(-1).url, 'https://cdn.example.test/5.m3u8');
});

test('clears a tab bucket', async () => {
  const storage = memoryStorage();
  const store = createCandidateStore(storage);
  await store.add(9, createCandidate('https://cdn.example.test/master.m3u8', 1), 1);
  await store.clear(9);
  assert.deepEqual(await store.get(9, 1), []);
});

test('removes one candidate atomically and returns the remaining fresh candidates', async () => {
  const storage = memoryStorage();
  const store = createCandidateStore(storage, { now: () => 2 });
  const first = 'https://cdn.example.test/first.m3u8';
  const second = 'https://cdn.example.test/second.m3u8';
  await store.add(9, createCandidate(first, 1), 1);
  await store.add(9, createCandidate(second, 2), 2);

  const remaining = await store.remove(9, first);
  assert.deepEqual(remaining.map((candidate) => candidate.url), [second]);
  assert.deepEqual((await store.get(9, 2)).map((candidate) => candidate.url), [second]);
  assert.deepEqual((await store.remove(9, first)).map((candidate) => candidate.url), [second]);
});
