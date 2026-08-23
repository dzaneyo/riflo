import {
  buildHandoffURL,
  normalizeHTTPURL,
  normalizeRequestContext,
  normalizeRequestHeaders,
  REQUEST_CONTEXT_SOURCE_DOCUMENT,
} from './model.js';

/**
 * Select the safest Referer for an explicit riflo handoff. An exact
 * document-url describes the frame that made the request and wins. Initiator
 * and origin are weak context hints: prefer the current top-level tab when it
 * exists, and use those hints only when the tab URL is unavailable.
 */
export function candidateReferer(candidate, currentTabURL = '') {
  const capturedReferer = normalizeRequestHeaders(candidate?.requestHeaders)?.referer;
  if (capturedReferer) return capturedReferer;
  const requestContext = normalizeRequestContext(candidate?.requestContext);
  if (requestContext?.source === REQUEST_CONTEXT_SOURCE_DOCUMENT) {
    return requestContext.url;
  }
  const currentTab = normalizeHTTPURL(currentTabURL);
  if (currentTab) return currentTab;
  return requestContext?.url || '';
}

export function buildCandidateHandoff(candidate, currentTabURL = '', base) {
  if (!candidate || typeof candidate.url !== 'string' || !normalizeHTTPURL(candidate.url)) {
    return '';
  }
  return buildHandoffURL(
    candidate.url,
    candidateReferer(candidate, currentTabURL),
    base,
    candidate.requestHeaders,
  );
}
