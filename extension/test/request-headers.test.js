import test from 'node:test';
import assert from 'node:assert/strict';
import {
  REQUEST_HEADERS_STORAGE_KEY,
  createRequestHeadersStore,
  requestHeadersKey,
} from '../request-headers.js';

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

const details = { tabId: 4, requestId: 'request-7' };

test('keys request headers by tab and webRequest request id', () => {
  assert.equal(requestHeadersKey(details), '4:request-7');
  assert.equal(requestHeadersKey({ tabId: -1, requestId: 'request-7' }), '');
  assert.equal(requestHeadersKey({ tabId: 4 }), '');
});

test('stores and consumes only safe request headers in session storage', async () => {
  const storage = memoryStorage();
  const store = createRequestHeadersStore(storage, { now: () => 100 });
  await store.remember(details, [
    { name: 'Referer', value: 'https://site.example.test/watch' },
    { name: 'Origin', value: 'https://site.example.test' },
    { name: 'User-Agent', value: 'riflo-test-agent' },
    { name: 'Cookie', value: 'session=secret' },
    { name: 'Authorization', value: 'Bearer secret' },
  ]);

  assert.deepEqual(storage.snapshot()[REQUEST_HEADERS_STORAGE_KEY], {
    '4:request-7': {
      headers: {
        referer: 'https://site.example.test/watch',
        origin: 'https://site.example.test',
        user_agent: 'riflo-test-agent',
      },
      seenAt: 100,
    },
  });
  assert.deepEqual(await store.consume(details), {
    referer: 'https://site.example.test/watch',
    origin: 'https://site.example.test',
    user_agent: 'riflo-test-agent',
  });
  assert.deepEqual(storage.snapshot()[REQUEST_HEADERS_STORAGE_KEY], {});
});

test('expires stale pending request headers and removes failed requests', async () => {
  let currentTime = 100;
  const storage = memoryStorage();
  const store = createRequestHeadersStore(storage, {
    now: () => currentTime,
    ttlMs: 10,
  });
  await store.remember(details, [{ name: 'Origin', value: 'https://site.example.test' }]);
  currentTime = 111;
  assert.equal(await store.consume(details), null);

  await store.remember(details, [{ name: 'Origin', value: 'https://site.example.test' }]);
  await store.remove(details);
  assert.equal(await store.consume(details), null);
});
