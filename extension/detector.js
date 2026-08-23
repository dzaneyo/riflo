import {
  normalizeHTTPURL,
  REQUEST_CONTEXT_SOURCE_DOCUMENT,
  REQUEST_CONTEXT_SOURCE_INITIATOR,
  REQUEST_CONTEXT_SOURCE_ORIGIN,
} from './model.js';

const HLS_CONTENT_TYPES = new Set([
  'application/vnd.apple.mpegurl',
  'application/x-mpegurl',
  'application/mpegurl',
  'audio/mpegurl',
  'audio/x-mpegurl',
]);

const SEGMENT_SUFFIXES = ['.ts', '.m4s', '.aac'];

export const DETECTION_SOURCE_URL_SUFFIX = 'url-suffix';
export const DETECTION_SOURCE_CONTENT_TYPE = 'content-type';

/**
 * Extract the page that initiated a request without trusting arbitrary
 * webRequest detail values. Chrome and Firefox expose slightly different
 * fields: documentUrl is the most precise when present, Chrome commonly
 * provides initiator, and Firefox may provide originUrl/origin.
 */
export function requestContextFromDetails(details) {
  if (!details || typeof details !== 'object') return null;

  const fields = [
    [REQUEST_CONTEXT_SOURCE_DOCUMENT, [
      details.documentUrl,
      details.documentURL,
      details.document_url,
    ]],
    [REQUEST_CONTEXT_SOURCE_INITIATOR, [details.initiator]],
    [REQUEST_CONTEXT_SOURCE_ORIGIN, [
      details.origin,
      details.originUrl,
      details.originURL,
      details.origin_url,
    ]],
  ];
  for (const [source, values] of fields) {
    for (const value of values) {
      const url = normalizeHTTPURL(value);
      if (url) return { url, source };
    }
  }
  return null;
}

function contentTypeFromHeaders(responseHeaders = []) {
  if (!Array.isArray(responseHeaders)) return '';
  const header = responseHeaders.find((entry) => (
    entry && typeof entry.name === 'string' && entry.name.toLowerCase() === 'content-type'
  ));
  return typeof header?.value === 'string' ? header.value.trim() : '';
}

function mediaType(value) {
  return value.split(';', 1)[0].trim().toLowerCase();
}

function pathnameFor(value) {
  try {
    return new URL(value).pathname.toLowerCase();
  } catch {
    return '';
  }
}

export function isHLSContentType(value) {
  return HLS_CONTENT_TYPES.has(mediaType(value || ''));
}

export function isMediaSegmentURL(value) {
  const pathname = pathnameFor(value);
  return SEGMENT_SUFFIXES.some((suffix) => pathname.endsWith(suffix));
}

/**
 * Inspect a webRequest onHeadersReceived detail object.
 *
 * It deliberately returns only a small, non-sensitive description. The full
 * URL is needed later for an explicit user handoff, but is never logged by the
 * background worker or placed in the badge.
 */
export function detectHLS(details) {
  if (!details || !Number.isInteger(details.tabId) || details.tabId < 0) return null;
  if (typeof details.url !== 'string') return null;

  let parsed;
  try {
    parsed = new URL(details.url);
  } catch {
    return null;
  }
  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') return null;
  if (isMediaSegmentURL(details.url)) return null;

  const contentType = contentTypeFromHeaders(details.responseHeaders);
  const hasURLSuffix = parsed.pathname.toLowerCase().endsWith('.m3u8');
  const hasContentType = isHLSContentType(contentType);
  if (!hasURLSuffix && !hasContentType) {
    return null;
  }

  const detectionSources = [];
  if (hasURLSuffix) detectionSources.push(DETECTION_SOURCE_URL_SUFFIX);
  if (hasContentType) detectionSources.push(DETECTION_SOURCE_CONTENT_TYPE);

  return {
    url: details.url,
    contentType,
    detectionSources,
  };
}

/**
 * Detect only an explicit HTTP(S) .m3u8 request. This is intentionally
 * separate from detectHLS because onBeforeRequest has no response headers and
 * must never treat a non-playlist request as an HLS candidate.
 */
export function detectHLSRequest(details) {
  if (!details || !Number.isInteger(details.tabId) || details.tabId < 0) return null;
  if (typeof details.url !== 'string') return null;

  let parsed;
  try {
    parsed = new URL(details.url);
  } catch {
    return null;
  }
  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') return null;
  if (!parsed.pathname.toLowerCase().endsWith('.m3u8')) return null;

  return {
    url: details.url,
    contentType: '',
    detectionSources: [DETECTION_SOURCE_URL_SUFFIX],
  };
}

export { HLS_CONTENT_TYPES, SEGMENT_SUFFIXES };
