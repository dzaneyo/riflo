const LOOPBACK_UI = 'http://127.0.0.1:8787/';

export const REQUEST_CONTEXT_SOURCE_DOCUMENT = 'document-url';
export const REQUEST_CONTEXT_SOURCE_INITIATOR = 'initiator';
export const REQUEST_CONTEXT_SOURCE_ORIGIN = 'origin';

export const REQUEST_CONTEXT_SOURCE_LABELS = Object.freeze({
  [REQUEST_CONTEXT_SOURCE_DOCUMENT]: 'documentUrl',
  [REQUEST_CONTEXT_SOURCE_INITIATOR]: 'initiator',
  [REQUEST_CONTEXT_SOURCE_ORIGIN]: 'origin',
});

export const REQUEST_HEADER_REFERER = 'referer';
export const REQUEST_HEADER_ORIGIN = 'origin';
export const REQUEST_HEADER_USER_AGENT = 'user_agent';

const MAX_USER_AGENT_LENGTH = 1024;

const REQUEST_CONTEXT_STRENGTH = Object.freeze({
  [REQUEST_CONTEXT_SOURCE_DOCUMENT]: 3,
  [REQUEST_CONTEXT_SOURCE_INITIATOR]: 2,
  [REQUEST_CONTEXT_SOURCE_ORIGIN]: 1,
});

/**
 * Normalize a URL that may have come from webRequest details.
 *
 * Request context is eventually used as a Referer. Keep it to web origins
 * that the extension can safely hand off, and do not retain URL credentials
 * or fragments. Query parameters are intentionally retained because some
 * sites use them to identify the page that initiated the media request.
 */
export function normalizeHTTPURL(value) {
  if (typeof value !== 'string' || !value.trim()) return '';
  try {
    const url = new URL(value.trim());
    if (url.protocol !== 'http:' && url.protocol !== 'https:') return '';
    url.username = '';
    url.password = '';
    url.hash = '';
    return url.toString();
  } catch {
    return '';
  }
}

function normalizeHTTPOrigin(value) {
  if (typeof value !== 'string' || !value.trim()) return '';
  const trimmed = value.trim();
  // A sandboxed document can legitimately send the opaque Origin value.
  if (trimmed === 'null') return trimmed;
  const normalized = normalizeHTTPURL(trimmed);
  if (!normalized) return '';
  try {
    return new URL(normalized).origin;
  } catch {
    return '';
  }
}

function normalizeUserAgent(value) {
  if (typeof value !== 'string') return '';
  const trimmed = value.trim();
  if (!trimmed || trimmed.length > MAX_USER_AGENT_LENGTH) return '';
  // Header values are later encoded into a handoff URL. Reject control
  // characters so a captured browser header can never become header syntax.
  if (/[\u0000-\u001f\u007f]/.test(trimmed)) return '';
  return trimmed;
}

function headerValue(entries, ...names) {
  const wanted = new Set(names.map((name) => name.toLowerCase()));
  for (const entry of entries) {
    if (!entry || typeof entry !== 'object' || typeof entry.name !== 'string') continue;
    if (!wanted.has(entry.name.toLowerCase())) continue;
    if (typeof entry.value === 'string') return entry.value;
  }
  return '';
}

/**
 * Keep only the request headers that are useful for replaying an HLS request.
 * This intentionally has a positive allow-list: Cookie, Authorization and
 * every other header are ignored rather than copied into session storage.
 * Both webRequest's [{name, value}] shape and our stored object shape are
 * accepted so old session entries can be normalized safely.
 */
export function normalizeRequestHeaders(value) {
  let entries;
  if (Array.isArray(value)) {
    entries = value;
  } else if (value && typeof value === 'object') {
    entries = [
      { name: REQUEST_HEADER_REFERER, value: value.referer ?? value.Referer },
      { name: REQUEST_HEADER_ORIGIN, value: value.origin ?? value.Origin },
      {
        name: 'user-agent',
        value: value.userAgent ?? value.user_agent ?? value['User-Agent'],
      },
    ];
  } else {
    return null;
  }

  const result = {};
  const referer = normalizeHTTPURL(headerValue(entries, 'referer'));
  if (referer) result[REQUEST_HEADER_REFERER] = referer;

  const origin = normalizeHTTPOrigin(headerValue(entries, 'origin'));
  if (origin) result[REQUEST_HEADER_ORIGIN] = origin;

  const userAgent = normalizeUserAgent(headerValue(entries, 'user-agent'));
  if (userAgent) result[REQUEST_HEADER_USER_AGENT] = userAgent;

  return Object.keys(result).length ? result : null;
}

/**
 * Return a safe URL for display in the popup.
 *
 * Signed URLs commonly put credentials in the query string or fragment. The
 * complete URL remains in the candidate model for the explicit handoff, but
 * the popup never renders those parts.
 */
export function displayURL(value) {
  try {
    const url = new URL(value);
    url.username = '';
    url.password = '';
    url.search = '';
    url.hash = '';
    return url.toString();
  } catch {
    return '';
  }
}

export function candidateHost(value) {
  try {
    return new URL(value).host;
  } catch {
    return '';
  }
}

export function requestContextStrength(value) {
  if (!value || typeof value !== 'object') return 0;
  return REQUEST_CONTEXT_STRENGTH[value.source] || 0;
}

/**
 * Normalize the context kept beside a candidate in storage.session.
 * Unknown or unsafe context is discarded instead of being used as a
 * handoff Referer.
 */
export function normalizeRequestContext(value) {
  if (!value || typeof value !== 'object') return null;
  const url = normalizeHTTPURL(value.url);
  const source = typeof value.source === 'string' ? value.source : '';
  if (!url || !REQUEST_CONTEXT_STRENGTH[source]) return null;
  return {
    url,
    displayUrl: displayURL(url),
    source,
  };
}

export const DETECTION_SOURCE_LABELS = Object.freeze({
  'url-suffix': 'URL 后缀',
  'content-type': 'Content-Type',
});

function normalizeDetectionSources(value) {
  if (!Array.isArray(value)) return [];
  return [...new Set(value.filter((source) => typeof source === 'string' && source))];
}

export function formatDetectionSources(value) {
  const labels = normalizeDetectionSources(value).map((source) => (
    DETECTION_SOURCE_LABELS[source] || source
  ));
  return labels.length ? labels.join('、') : '未知';
}

export function createCandidate(url, seenAt = Date.now(), metadata = {}) {
  const details = typeof metadata === 'string' ? { contentType: metadata } : metadata || {};
  return {
    url,
    displayUrl: displayURL(url),
    host: candidateHost(url),
    protocol: 'HLS',
    contentType: typeof details.contentType === 'string' ? details.contentType : '',
    detectionSources: normalizeDetectionSources(details.detectionSources),
    requestContext: normalizeRequestContext(details.requestContext),
    requestHeaders: normalizeRequestHeaders(details.requestHeaders),
    firstSeen: seenAt,
    lastSeen: seenAt,
  };
}

/**
 * Build the only handoff made by the extension. The complete source URL is
 * kept in the encoded hash for riflo; the extension does not POST credentials
 * or read cookies.
 */
export function buildHandoffURL(source, referer = '', base = LOOPBACK_UI, requestHeaders = null) {
  const target = new URL(base);
  const params = new URLSearchParams();
  params.set('source', source);
  // Callers that select a Referer use normalizeHTTPURL first. Keep this
  // low-level encoder lossless for already validated values (for example,
  // spaces in a manually constructed test URL are encoded by URLSearchParams
  // without changing the value returned by URLSearchParams.get()).
  params.set('referer', typeof referer === 'string' ? referer : '');
  const normalizedHeaders = normalizeRequestHeaders(requestHeaders);
  if (normalizedHeaders?.origin) params.set('origin', normalizedHeaders.origin);
  if (normalizedHeaders?.user_agent) params.set('user_agent', normalizedHeaders.user_agent);
  params.set('from', 'extension');
  target.hash = params.toString();
  return target.toString();
}

export { LOOPBACK_UI };
