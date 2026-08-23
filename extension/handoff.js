import {
  buildHandoffURL,
  normalizeHTTPURL,
  normalizeRequestContext,
} from './model.js';

/**
 * Select the safest Referer for an explicit riflo handoff. A context captured
 * with the candidate is more accurate for iframe/CDN requests than the
 * currently active top-level tab URL. The tab URL is only a fallback.
 */
export function candidateReferer(candidate, currentTabURL = '') {
  const requestContext = normalizeRequestContext(candidate?.requestContext);
  return requestContext?.url || normalizeHTTPURL(currentTabURL);
}

export function buildCandidateHandoff(candidate, currentTabURL = '', base) {
  if (!candidate || typeof candidate.url !== 'string' || !normalizeHTTPURL(candidate.url)) {
    return '';
  }
  return buildHandoffURL(candidate.url, candidateReferer(candidate, currentTabURL), base);
}
