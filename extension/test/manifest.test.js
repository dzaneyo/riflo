import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const manifest = JSON.parse(readFileSync(new URL('../manifest.json', import.meta.url), 'utf8'));
const apiSource = readFileSync(new URL('../api.js', import.meta.url), 'utf8');
const backgroundSource = readFileSync(new URL('../background.js', import.meta.url), 'utf8');
const popupSource = readFileSync(new URL('../popup.js', import.meta.url), 'utf8');
const handoffSource = readFileSync(new URL('../handoff.js', import.meta.url), 'utf8');

test('manifest declares one cross-browser MV3 background entry point', () => {
  assert.equal(manifest.manifest_version, 3);
  assert.equal(manifest.minimum_chrome_version, '121');
  assert.equal(manifest.browser_specific_settings?.gecko?.strict_min_version, '121');
  assert.deepEqual(manifest.background, {
    scripts: ['background.js'],
    service_worker: 'background.js',
    type: 'module',
  });
});

test('manifest keeps the intentionally narrow permission set', () => {
  assert.deepEqual([...manifest.permissions].sort(), ['activeTab', 'storage', 'webRequest']);
  assert.deepEqual(manifest.host_permissions, ['<all_urls>']);
  for (const forbidden of ['cookies', 'downloads', 'nativeMessaging', 'scripting', 'webRequestBlocking']) {
    assert.equal(manifest.permissions.includes(forbidden), false, `unexpected permission: ${forbidden}`);
  }
});

test('background and popup use the shared browser/chrome API alias', () => {
  assert.match(apiSource, /globalThis\.browser\s*\?\?\s*globalThis\.chrome/);
  assert.match(backgroundSource, /import \{ extensionAPI \} from ['"]\.\/api\.js['"]/);
  assert.match(popupSource, /import \{ extensionAPI \} from ['"]\.\/api\.js['"]/);
  assert.doesNotMatch(backgroundSource, /\bchrome\./);
  assert.doesNotMatch(popupSource, /\bchrome\./);
  assert.doesNotMatch(backgroundSource, /onMessage\.addListener\(\s*async/);
  assert.match(backgroundSource, /onMessage[\s\S]*return true;/);
});

test('background observes request and response phases and supports atomic removal', () => {
  assert.match(backgroundSource, /webRequest\.onBeforeRequest\.addListener/);
  assert.match(backgroundSource, /webRequest\.onBeforeSendHeaders\.addListener/);
  assert.match(backgroundSource, /webRequest\.onHeadersReceived\.addListener/);
  assert.match(backgroundSource, /\['requestHeaders'\]/);
  assert.match(backgroundSource, /webRequest\.onCompleted\.addListener/);
  assert.match(backgroundSource, /webRequest\.onErrorOccurred\.addListener/);
  assert.match(backgroundSource, /detectHLSRequest/);
  assert.match(backgroundSource, /type === ['"]remove-candidate['"]/);
  assert.match(popupSource, /type: ['"]remove-candidate['"]/);
  assert.match(popupSource, /formatDetectionSources/);
});

test('request header capture uses Chromium extraHeaders with a Firefox fallback', () => {
  assert.match(backgroundSource, /\['requestHeaders', 'extraHeaders'\]/);
  assert.match(backgroundSource, /catch\s*\{/);
  assert.match(backgroundSource, /\['requestHeaders'\]/);
  assert.doesNotMatch(backgroundSource, /webRequestBlocking|['"]blocking['"]/);
});

test('handoff prefers captured request context and keeps popup output redacted', () => {
  assert.match(backgroundSource, /requestContextFromDetails\(details\)/);
  assert.match(popupSource, /buildCandidateHandoff/);
  assert.match(popupSource, /requestContext\.displayUrl/);
  assert.match(handoffSource, /candidate\?\.requestContext/);
  assert.match(handoffSource, /normalizeHTTPURL\(currentTabURL\)/);
  assert.match(handoffSource, /candidate\.requestHeaders/);
});
