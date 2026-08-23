import { detectHLS, detectHLSRequest, requestContextFromDetails } from './detector.js';
import { createCandidate } from './model.js';
import { createCandidateStore } from './store.js';
import { extensionAPI } from './api.js';

const store = createCandidateStore(extensionAPI.storage.session);

function updateBadge(tabId, count) {
  if (!Number.isInteger(tabId) || tabId < 0) return Promise.resolve();
  return extensionAPI.action.setBadgeText({
    tabId,
    text: count > 0 ? String(count) : '',
  });
}

async function recordCandidate(details, detector = detectHLS) {
  const detected = detector(details);
  if (!detected) return;

  const candidate = createCandidate(detected.url, Date.now(), {
    contentType: detected.contentType,
    detectionSources: detected.detectionSources,
    requestContext: requestContextFromDetails(details),
  });
  try {
    const candidates = await store.add(details.tabId, candidate);
    await updateBadge(details.tabId, candidates.length);
  } catch {
    // A discarded service worker or an unavailable storage area should not
    // make a media request fail. The full URL is intentionally not logged.
  }
}

extensionAPI.webRequest.onBeforeRequest.addListener(
  (details) => {
    void recordCandidate(details, detectHLSRequest);
  },
  {
    urls: ['http://*/*', 'https://*/*'],
    types: ['media', 'xmlhttprequest', 'other'],
  },
);

extensionAPI.webRequest.onHeadersReceived.addListener(
  (details) => {
    void recordCandidate(details, detectHLS);
  },
  {
    urls: ['http://*/*', 'https://*/*'],
    types: ['media', 'xmlhttprequest', 'other'],
  },
  ['responseHeaders'],
);

extensionAPI.tabs.onRemoved.addListener((tabId) => {
  void store.removeTab(tabId).catch(() => undefined);
});

extensionAPI.runtime.onMessage.addListener((message, _sender, sendResponse) => {
  if (!message || !['clear-candidates', 'remove-candidate'].includes(message.type)) return false;

  const tabId = Number(message.tabId);
  if (!Number.isInteger(tabId) || tabId < 0) {
    sendResponse({ ok: false, error: 'invalid_tab_id' });
    return false;
  }

  if (message.type === 'remove-candidate'
    && (typeof message.url !== 'string' || !message.url)) {
    sendResponse({ ok: false, error: 'invalid_candidate_url' });
    return false;
  }

  const operation = message.type === 'remove-candidate'
    ? store.remove(tabId, message.url)
    : store.clear(tabId).then(() => []);
  void operation
    .then(async (candidates) => {
      await updateBadge(tabId, candidates.length);
      sendResponse({ ok: true, candidates });
    })
    .catch(() => sendResponse({ ok: false, error: 'storage_error' }));
  // Keep the callback response style so Chrome and Firefox agree on the
  // lifetime of this asynchronous message handler.
  return true;
});
