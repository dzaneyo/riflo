import {
  detectHLS,
  detectHLSRequest,
  requestContextFromDetails,
  requestHeadersFromDetails,
} from './detector.js';
import { createCandidate } from './model.js';
import { createCandidateStore } from './store.js';
import { extensionAPI } from './api.js';
import { createRequestHeadersStore } from './request-headers.js';

const store = createCandidateStore(extensionAPI.storage.session);
const requestHeadersStore = createRequestHeadersStore(extensionAPI.storage.session);

async function isActiveTab(tabId) {
  if (!Number.isInteger(tabId) || tabId < 0) return false;
  try {
    const tabs = await extensionAPI.tabs.query({ active: true, currentWindow: true });
    return tabs.some((tab) => tab && tab.id === tabId);
  } catch {
    return false;
  }
}

function updateBadge(tabId, count) {
  if (!Number.isInteger(tabId) || tabId < 0) return Promise.resolve();
  return extensionAPI.action.setBadgeText({
    tabId,
    text: count > 0 ? String(count) : '',
  });
}

async function recordCandidate(details, detector = detectHLS, capturedHeaders = null) {
  if (!await isActiveTab(details?.tabId)) return;
  const detected = detector(details);
  if (!detected) return;

  const requestHeaders = capturedHeaders || requestHeadersFromDetails(details);
  const candidate = createCandidate(detected.url, Date.now(), {
    contentType: detected.contentType,
    detectionSources: detected.detectionSources,
    requestContext: requestContextFromDetails(details),
    requestHeaders,
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

const mediaRequestFilter = {
  urls: ['http://*/*', 'https://*/*'],
  types: ['media', 'xmlhttprequest', 'other'],
};

function captureSafeRequestHeaders(details) {
  // At request time there are no response headers to identify an
  // extensionless playlist. Restrict the session association to explicit
  // .m3u8 requests so normal media segments and XHRs do not cause session
  // storage churn.
  if (!detectHLSRequest(details)) return;
  void isActiveTab(details?.tabId).then((active) => {
    if (!active) return;
    const headers = requestHeadersFromDetails(details);
    if (!headers) return;
    return requestHeadersStore.remember(details, headers)
      .then(() => recordCandidate(details, detectHLSRequest, headers));
  }).catch(() => undefined);
}

// Chromium hides Referer and some CORS-sensitive headers unless extraHeaders
// is requested. Firefox does not consistently accept that Chromium-specific
// option, so register with a safe fallback. Neither path is blocking and the
// callback still keeps only the explicit allow-list.
try {
  extensionAPI.webRequest.onBeforeSendHeaders.addListener(
    captureSafeRequestHeaders,
    mediaRequestFilter,
    ['requestHeaders', 'extraHeaders'],
  );
} catch {
  extensionAPI.webRequest.onBeforeSendHeaders.addListener(
    captureSafeRequestHeaders,
    mediaRequestFilter,
    ['requestHeaders'],
  );
}

extensionAPI.webRequest.onHeadersReceived.addListener(
  (details) => {
    const detected = detectHLS(details);
    if (!detected) return;
    void requestHeadersStore.consume(details)
      .catch(() => null)
      .then((headers) => recordCandidate(details, () => detected, headers));
  },
  {
    urls: ['http://*/*', 'https://*/*'],
    types: ['media', 'xmlhttprequest', 'other'],
  },
  ['responseHeaders'],
);

function discardRequestHeaders(details) {
  if (!detectHLSRequest(details)) return;
  void requestHeadersStore.remove(details).catch(() => undefined);
}

extensionAPI.webRequest.onCompleted.addListener(
  discardRequestHeaders,
  {
    urls: ['http://*/*', 'https://*/*'],
    types: ['media', 'xmlhttprequest', 'other'],
  },
);

extensionAPI.webRequest.onErrorOccurred.addListener(
  discardRequestHeaders,
  {
    urls: ['http://*/*', 'https://*/*'],
    types: ['media', 'xmlhttprequest', 'other'],
  },
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
