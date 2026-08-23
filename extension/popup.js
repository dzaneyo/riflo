import {
  displayURL,
  formatDetectionSources,
  normalizeRequestContext,
  REQUEST_CONTEXT_SOURCE_LABELS,
} from './model.js';
import { buildCandidateHandoff } from './handoff.js';
import { createCandidateStore } from './store.js';
import { extensionAPI } from './api.js';

const store = createCandidateStore(extensionAPI.storage.session);
const elements = {
  count: document.querySelector('#count'),
  clearButton: document.querySelector('#clearButton'),
  tabLabel: document.querySelector('#tabLabel'),
  empty: document.querySelector('#empty'),
  candidateList: document.querySelector('#candidateList'),
};

let currentTab = null;
let candidates = [];

function formatTime(value) {
  if (!Number.isFinite(value)) return '未知时间';
  return new Intl.DateTimeFormat(undefined, {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  }).format(new Date(value));
}

function appendText(parent, tagName, className, value) {
  const element = document.createElement(tagName);
  if (className) element.className = className;
  element.textContent = value;
  parent.append(element);
  return element;
}

function render() {
  elements.count.textContent = String(candidates.length);
  elements.candidateList.replaceChildren();
  elements.empty.hidden = candidates.length !== 0;
  elements.clearButton.disabled = candidates.length === 0;

  for (const candidate of candidates) {
    const card = document.createElement('article');
    card.className = 'candidate';

    const top = document.createElement('div');
    top.className = 'candidate-top';
    appendText(top, 'strong', 'host', candidate.host || '未知来源');
    appendText(top, 'span', 'protocol', candidate.protocol || 'HLS');
    card.append(top);

    appendText(card, 'div', 'display-url', candidate.displayUrl || '地址不可显示');

    const meta = document.createElement('div');
    meta.className = 'candidate-meta';
    appendText(meta, 'span', '', `发现 ${formatDetectionSources(candidate.detectionSources)}`);
    const requestContext = normalizeRequestContext(candidate.requestContext);
    if (requestContext) {
      const contextLabel = REQUEST_CONTEXT_SOURCE_LABELS[requestContext.source]
        || requestContext.source;
      appendText(meta, 'span', 'context', `来源 ${requestContext.displayUrl}（${contextLabel}）`);
    }
    appendText(meta, 'span', '', `首次 ${formatTime(candidate.firstSeen)}`);
    appendText(meta, 'span', '', `最近 ${formatTime(candidate.lastSeen)}`);
    card.append(meta);

    const actions = document.createElement('div');
    actions.className = 'candidate-actions';
    const openButton = appendText(actions, 'button', 'button primary', '在 riflo 打开');
    openButton.type = 'button';
    openButton.addEventListener('click', () => {
      void openInRiflo(candidate).catch(() => undefined);
    });
    card.append(actions);
    const removeButton = appendText(actions, 'button', 'button secondary', '删除');
    removeButton.type = 'button';
    removeButton.setAttribute('aria-label', `删除 ${candidate.displayUrl || '候选地址'}`);
    removeButton.addEventListener('click', () => {
      void removeCandidate(candidate, removeButton).catch(() => undefined);
    });
    elements.candidateList.append(card);
  }
}

async function openInRiflo(candidate) {
  if (!candidate || typeof candidate.url !== 'string' || !currentTab) return;
  const handoffURL = buildCandidateHandoff(
    candidate,
    typeof currentTab.url === 'string' ? currentTab.url : '',
  );
  if (!handoffURL) return;
  await extensionAPI.tabs.create({ url: handoffURL });
}

async function removeCandidate(candidate, button) {
  if (!currentTab || !Number.isInteger(currentTab.id) || currentTab.id < 0) return;
  if (!candidate || typeof candidate.url !== 'string' || !candidate.url) return;
  button.disabled = true;
  try {
    const response = await extensionAPI.runtime.sendMessage({
      type: 'remove-candidate',
      tabId: currentTab.id,
      url: candidate.url,
    });
    if (!response?.ok) throw new Error('remove failed');
    candidates = Array.isArray(response.candidates)
      ? response.candidates
      : candidates.filter((entry) => entry.url !== candidate.url);
    render();
  } catch {
    button.disabled = false;
  }
}

async function clearCurrentTab() {
  if (!currentTab || !Number.isInteger(currentTab.id) || currentTab.id < 0) return;
  elements.clearButton.disabled = true;
  try {
    const response = await extensionAPI.runtime.sendMessage({
      type: 'clear-candidates',
      tabId: currentTab.id,
    });
    if (!response?.ok) throw new Error('clear failed');
    candidates = [];
    render();
  } catch {
    // Keep the existing list visible if the service worker is restarting.
    elements.clearButton.disabled = candidates.length === 0;
  }
}

async function load() {
  try {
    const tabs = await extensionAPI.tabs.query({ active: true, currentWindow: true });
    currentTab = tabs[0] || null;
    if (!currentTab || !Number.isInteger(currentTab.id) || currentTab.id < 0) {
      elements.tabLabel.textContent = '无法读取当前标签页';
      render();
      return;
    }
    elements.tabLabel.textContent = currentTab.title || displayURL(currentTab.url || '') || '当前标签页';
    candidates = await store.get(currentTab.id);
    render();
  } catch {
    elements.tabLabel.textContent = '读取候选失败，请重新打开扩展';
    candidates = [];
    render();
  }
}

elements.clearButton.addEventListener('click', () => {
  void clearCurrentTab();
});

void load();
