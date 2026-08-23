import {
  normalizeRequestContext,
  requestContextStrength,
} from './model.js';

export const STORAGE_KEY = 'rifloHlsCandidatesByTab';
export const MAX_CANDIDATES_PER_TAB = 20;
export const CANDIDATE_TTL_MS = 30 * 60 * 1000;

function tabKey(tabId) {
  return String(tabId);
}

function clone(value) {
  return value == null ? value : structuredClone(value);
}

function isValidTabId(tabId) {
  return Number.isInteger(tabId) && tabId >= 0;
}

function normalizeBuckets(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return {};
  const buckets = {};
  for (const [key, entries] of Object.entries(value)) {
    if (!Array.isArray(entries)) continue;
    buckets[key] = entries.filter((entry) => (
      entry && typeof entry === 'object' && typeof entry.url === 'string'
    )).map((entry) => {
      const normalized = { ...entry };
      const requestContext = normalizeRequestContext(entry.requestContext);
      if (requestContext) normalized.requestContext = requestContext;
      else delete normalized.requestContext;
      return normalized;
    });
  }
  return buckets;
}

function isFresh(candidate, now, ttlMs) {
  return Number.isFinite(candidate.lastSeen)
    && candidate.lastSeen > now - ttlMs;
}

function freshEntries(entries, now, ttlMs) {
  return (entries || []).filter((entry) => isFresh(entry, now, ttlMs));
}

function orderEntries(entries) {
  return entries.sort((left, right) => {
    const byRecent = (right.lastSeen || 0) - (left.lastSeen || 0);
    return byRecent || (right.firstSeen || 0) - (left.firstSeen || 0);
  });
}

function detectionSources(value) {
  if (!Array.isArray(value)) return [];
  return value.filter((source) => typeof source === 'string' && source);
}

function mergeRequestContext(existing, candidate) {
  const current = normalizeRequestContext(existing);
  const incoming = normalizeRequestContext(candidate);
  if (!current) return incoming;
  if (!incoming) return current;
  // Keep the stronger context. When both have equal strength, retain the
  // existing value so an empty/less precise event cannot cause churn.
  return requestContextStrength(incoming) > requestContextStrength(current)
    ? incoming
    : current;
}

function mergeCandidate(existing, candidate, timestamp) {
  const next = {
    ...existing,
    ...clone(candidate),
    firstSeen: Number.isFinite(existing.firstSeen) ? existing.firstSeen : timestamp,
    lastSeen: timestamp,
  };

  const contentType = typeof candidate.contentType === 'string' && candidate.contentType
    ? candidate.contentType
    : existing.contentType;
  next.contentType = typeof contentType === 'string' ? contentType : '';
  next.detectionSources = [
    ...new Set([...detectionSources(existing.detectionSources), ...detectionSources(candidate.detectionSources)]),
  ];
  const requestContext = mergeRequestContext(existing.requestContext, candidate.requestContext);
  if (requestContext) next.requestContext = requestContext;
  else delete next.requestContext;
  return next;
}

/**
 * A storage-backed candidate store. Every mutation serializes its complete
 * read-modify-write cycle. A single queue is intentional: all tab buckets are
 * stored under one key, so per-tab queues would still lose updates across tabs.
 */
export function createCandidateStore(storageArea, options = {}) {
  if (!storageArea || typeof storageArea.get !== 'function' || typeof storageArea.set !== 'function') {
    throw new TypeError('A chrome.storage.session-compatible area is required');
  }

  const now = options.now || (() => Date.now());
  const ttlMs = Number.isFinite(options.ttlMs) ? options.ttlMs : CANDIDATE_TTL_MS;
  const maxPerTab = Number.isInteger(options.maxPerTab) ? options.maxPerTab : MAX_CANDIDATES_PER_TAB;
  let mutationQueue = Promise.resolve();

  async function readBuckets() {
    const result = await storageArea.get(STORAGE_KEY);
    return normalizeBuckets(result?.[STORAGE_KEY]);
  }

  async function writeBuckets(buckets) {
    await storageArea.set({ [STORAGE_KEY]: buckets });
  }

  function mutate(operation) {
    const result = mutationQueue.then(operation, operation);
    mutationQueue = result.catch(() => undefined);
    return result;
  }

  async function get(tabId, at = now()) {
    if (!isValidTabId(tabId)) return [];
    const buckets = await readBuckets();
    return clone(orderEntries(freshEntries(buckets[tabKey(tabId)], at, ttlMs)));
  }

  async function add(tabId, candidate, seenAt = now()) {
    if (!isValidTabId(tabId)) throw new TypeError('tabId must be a non-negative integer');
    if (!candidate || typeof candidate.url !== 'string' || !candidate.url) {
      throw new TypeError('candidate.url is required');
    }
    return mutate(async () => {
      const buckets = await readBuckets();
      const timestamp = Number.isFinite(seenAt) ? seenAt : now();
      for (const [key, entries] of Object.entries(buckets)) {
        const fresh = freshEntries(entries, timestamp, ttlMs);
        if (fresh.length) buckets[key] = fresh;
        else delete buckets[key];
      }

      const key = tabKey(tabId);
      const entries = buckets[key] || [];
      const existingIndex = entries.findIndex((entry) => entry.url === candidate.url);
      if (existingIndex >= 0) {
        entries[existingIndex] = mergeCandidate(entries[existingIndex], candidate, timestamp);
      } else {
        const entry = {
          ...clone(candidate),
          firstSeen: Number.isFinite(candidate.firstSeen) ? candidate.firstSeen : timestamp,
          lastSeen: timestamp,
        };
        const requestContext = normalizeRequestContext(candidate.requestContext);
        if (requestContext) entry.requestContext = requestContext;
        else delete entry.requestContext;
        entries.push(entry);
      }
      buckets[key] = orderEntries(entries).slice(0, maxPerTab);
      await writeBuckets(buckets);
      return clone(buckets[key]);
    });
  }

  async function clear(tabId) {
    if (!isValidTabId(tabId)) return;
    return mutate(async () => {
      const buckets = await readBuckets();
      delete buckets[tabKey(tabId)];
      await writeBuckets(buckets);
    });
  }

  async function remove(tabId, url) {
    if (!isValidTabId(tabId) || typeof url !== 'string' || !url) return [];
    return mutate(async () => {
      const buckets = await readBuckets();
      const key = tabKey(tabId);
      const entries = freshEntries(buckets[key], now(), ttlMs)
        .filter((entry) => entry.url !== url);
      if (entries.length) buckets[key] = orderEntries(entries);
      else delete buckets[key];
      await writeBuckets(buckets);
      return clone(buckets[key] || []);
    });
  }

  async function removeTab(tabId) {
    return clear(tabId);
  }

  return { get, add, clear, remove, removeTab };
}
