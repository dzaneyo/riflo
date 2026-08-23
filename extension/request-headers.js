import { normalizeRequestHeaders } from './model.js';

export const REQUEST_HEADERS_STORAGE_KEY = 'rifloHlsRequestHeaders';
export const REQUEST_HEADERS_TTL_MS = 2 * 60 * 1000;
export const MAX_PENDING_REQUEST_HEADERS = 128;

function clone(value) {
  return value == null ? value : structuredClone(value);
}

/**
 * webRequest requestId values are unique for a browser request, but the tab
 * prefix prevents a browser implementation from accidentally mixing the
 * same identifier across tabs.
 */
export function requestHeadersKey(details) {
  if (!details || typeof details !== 'object') return '';
  if (!Number.isInteger(details.tabId) || details.tabId < 0) return '';
  if (details.requestId === undefined || details.requestId === null) return '';
  const requestId = String(details.requestId).trim();
  return requestId ? `${details.tabId}:${requestId}` : '';
}

function normalizeBuckets(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return {};
  const buckets = {};
  for (const [key, entry] of Object.entries(value)) {
    if (!entry || typeof entry !== 'object') continue;
    const headers = normalizeRequestHeaders(entry.headers);
    const seenAt = Number(entry.seenAt);
    if (!headers || !Number.isFinite(seenAt)) continue;
    buckets[key] = { headers, seenAt };
  }
  return buckets;
}

function isFresh(entry, now, ttlMs) {
  return Number.isFinite(entry?.seenAt) && entry.seenAt > now - ttlMs;
}

/**
 * Keep a short-lived association between an explicit .m3u8 webRequest and its
 * safe headers in storage.session until the response confirms the candidate.
 */
export function createRequestHeadersStore(storageArea, options = {}) {
  if (!storageArea || typeof storageArea.get !== 'function' || typeof storageArea.set !== 'function') {
    throw new TypeError('A chrome.storage.session-compatible area is required');
  }

  const now = options.now || (() => Date.now());
  const ttlMs = Number.isFinite(options.ttlMs) ? options.ttlMs : REQUEST_HEADERS_TTL_MS;
  const maxPending = Number.isInteger(options.maxPending)
    ? options.maxPending
    : MAX_PENDING_REQUEST_HEADERS;
  let mutationQueue = Promise.resolve();

  async function readBuckets() {
    const result = await storageArea.get(REQUEST_HEADERS_STORAGE_KEY);
    return normalizeBuckets(result?.[REQUEST_HEADERS_STORAGE_KEY]);
  }

  async function writeBuckets(buckets) {
    await storageArea.set({ [REQUEST_HEADERS_STORAGE_KEY]: buckets });
  }

  function mutate(operation) {
    const result = mutationQueue.then(operation, operation);
    mutationQueue = result.catch(() => undefined);
    return result;
  }

  function prune(buckets, at) {
    for (const [key, entry] of Object.entries(buckets)) {
      if (!isFresh(entry, at, ttlMs)) delete buckets[key];
    }
    const entries = Object.entries(buckets)
      .sort((left, right) => right[1].seenAt - left[1].seenAt)
      .slice(0, maxPending);
    return Object.fromEntries(entries);
  }

  async function remember(details, headers) {
    const key = requestHeadersKey(details);
    const normalized = normalizeRequestHeaders(headers);
    if (!key || !normalized) return;
    return mutate(async () => {
      const timestamp = now();
      const buckets = prune(await readBuckets(), timestamp);
      buckets[key] = { headers: normalized, seenAt: timestamp };
      await writeBuckets(prune(buckets, timestamp));
    });
  }

  async function consume(details) {
    const key = requestHeadersKey(details);
    if (!key) return null;
    return mutate(async () => {
      const timestamp = now();
      const buckets = prune(await readBuckets(), timestamp);
      const entry = buckets[key];
      delete buckets[key];
      await writeBuckets(buckets);
      return clone(entry?.headers || null);
    });
  }

  async function remove(details) {
    const key = requestHeadersKey(details);
    if (!key) return;
    return mutate(async () => {
      const buckets = await readBuckets();
      if (!Object.hasOwn(buckets, key)) return;
      delete buckets[key];
      await writeBuckets(buckets);
    });
  }

  return { remember, consume, remove };
}
