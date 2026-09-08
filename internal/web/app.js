(function () {
  'use strict';

  var ACTIVE_STATUSES = { queued: true, running: true };
  var RETRYABLE_STATUSES = { failed: true, interrupted: true };
  var HANDOFF_SOURCE_LIMIT = 64 * 1024;
  var HANDOFF_REFERER_LIMIT = 8 * 1024;
  var HANDOFF_ORIGIN_LIMIT = 4 * 1024;
  var HANDOFF_USER_AGENT_LIMIT = 4 * 1024;
  var STATUS_LABELS = {
    queued: '排队中',
    running: '下载中',
    succeeded: '已完成',
    failed: '失败',
    canceled: '已取消',
    interrupted: '已中断'
  };
  var ERROR_LABELS = {
    forbidden_host: '仅支持从本机地址访问。',
    invalid_request: '请求参数不完整或无效。',
    not_implemented: '此功能尚未实现，请检查服务版本。',
    internal_error: '本地服务暂时不可用，请稍后重试。',
    task_not_found: '找不到该任务，可能已经被清理。',
    ffmpeg_unavailable: '找不到 ffmpeg，请先运行 riflo doctor。',
    ffprobe_unavailable: '找不到 ffprobe，请先运行 riflo doctor。',
    media_unavailable: '无法读取媒体，请检查地址和访问权限。',
    media_access_denied: '媒体服务器拒绝访问，请检查页面 Referer；如果需要登录，请在“高级请求信息”中手工补充 Cookie。Cookie 只在本次请求使用，不会保存。',
    media_rate_limited: '媒体服务器暂时限制了请求，请稍后重试或降低请求频率。',
    rate_limited: '媒体服务器暂时限制了请求，请稍后重试或降低请求频率。',
    media_timeout: '媒体请求超时，请检查网络后重试。',
    timeout: '媒体请求超时，请检查网络后重试。',
    media_network_error: '媒体网络请求失败，请检查网络后重试。',
    network: '媒体网络请求失败，请检查网络后重试。',
    media_http_error: '媒体服务器返回了 HTTP 错误，请稍后重试。',
    http: '媒体服务器返回了 HTTP 错误，请稍后重试。',
    media_url_expired: '媒体地址可能已经过期，请回到视频页重新播放并重新获取地址。',
    hls_invalid_playlist: 'HLS 播放列表无效或已失效，请重新获取媒体地址。',
    invalid_playlist: 'HLS 播放列表无效或已失效，请重新获取媒体地址。',
    hls_variant_unavailable: '选择的画质已不可用，请重新检查媒体并选择“自动”。',
    variant_unavailable: '选择的画质已不可用，请重新检查媒体并选择“自动”。',
    invalid_output: '输出目录或文件名无效，请检查保存设置。',
    output_exists: '输出文件已经存在，请更换文件名或目录。',
    media_tool_unavailable: '找不到 ffmpeg 或 ffprobe，请先运行 riflo doctor。',
    ffmpeg_failed: 'ffmpeg 处理失败，请检查媒体格式或重试。',
    unauthorized: '媒体服务器要求身份验证，请检查或补充 Referer 或 Cookie。',
    forbidden: '媒体服务器拒绝访问，请检查或补充 Referer 或 Cookie。',
    media_unauthorized: '媒体服务器要求身份验证，请检查或补充 Referer 或 Cookie。',
    media_forbidden: '媒体服务器拒绝访问，请检查或补充 Referer 或 Cookie。',
    http_401: '媒体服务器要求身份验证，请检查或补充 Referer 或 Cookie。',
    http_403: '媒体服务器拒绝访问，请检查或补充 Referer 或 Cookie。',
    http_429: '媒体服务器暂时限制了请求，请稍后重试或降低请求频率。',
    http_408: '媒体请求超时，请检查网络后重试。',
    http_500: '媒体服务器暂时不可用，请稍后重试。',
    http_502: '媒体服务器网关暂时不可用，请稍后重试。',
    http_503: '媒体服务器暂时不可用，请稍后重试。',
    http_504: '媒体服务器网关请求超时，请稍后重试。',
    canceled: '任务已取消。'
  };

  var mediaUrl = document.getElementById('mediaUrl');
  var referer = document.getElementById('referer');
  var origin = document.getElementById('origin');
  var userAgent = document.getElementById('userAgent');
  var cookie = document.getElementById('cookie');
  var outputDir = document.getElementById('outputDir');
  var outputName = document.getElementById('outputName');
  var format = document.getElementById('format');
  var taskForm = document.getElementById('taskForm');
  var inspectButton = document.getElementById('inspectButton');
  var startButton = document.getElementById('startButton');
  var formMessage = document.getElementById('formMessage');
  var globalMessage = document.getElementById('globalMessage');
  var inspectPanel = document.getElementById('inspectPanel');
  var inspectResult = document.getElementById('inspectResult');
  var refreshButton = document.getElementById('refreshButton');
  var taskMessage = document.getElementById('taskMessage');
  var activeTasks = document.getElementById('activeTasks');
  var historyTasks = document.getElementById('historyTasks');
  var activeCount = document.getElementById('activeCount');
  var healthStatus = document.getElementById('healthStatus');
  var healthText = document.getElementById('healthText');

  var taskData = [];
  var taskRequestInFlight = false;
  var healthRequestInFlight = false;
  var formRequestInFlight = false;
  var pollTimer = 0;
  var inspectedMediaURL = '';
  var inspectedVariantIndexes = [];
  // This map deliberately lives only in this page's JavaScript heap. It
  // is cleared when a task reaches a terminal success/cancel state, and
  // disappears automatically when the page is refreshed or closed.
  var transientTaskRequests = Object.create(null);

  function createElement(tag, className, text) {
    var element = document.createElement(tag);
    if (className) {
      element.className = className;
    }
    if (text !== undefined && text !== null) {
      element.textContent = String(text);
    }
    return element;
  }

  function setMessage(element, message, kind) {
    element.replaceChildren();
    if (!message) {
      element.hidden = true;
      element.className = element === globalMessage ? 'global-message' : 'inline-message';
      return;
    }
    element.textContent = String(message);
    element.hidden = false;
    element.className = element === globalMessage ? 'global-message' : 'inline-message';
    if (kind) {
      element.classList.add(kind);
    }
  }

  function setHealth(healthy, message) {
    healthStatus.classList.toggle('ok', healthy);
    healthStatus.classList.toggle('error', !healthy);
    healthText.textContent = message;
  }

  function friendlyError(error) {
    if (!error) {
      return '请求失败，请稍后重试。';
    }
    if (error.code === 'network_error') {
      return '无法连接到本地服务，请确认 riflo serve 正在运行。';
    }
    if (isAuthenticationError(error)) {
      return '媒体服务器拒绝了本次请求（401/403）。请检查页面 Referer；如果需要登录，请在“高级请求信息”中手工补充 Cookie。Cookie 只在本次检查或下载请求中使用，不会保存。';
    }
    if (ERROR_LABELS[error.code]) {
      return ERROR_LABELS[error.code];
    }
    var status = Number(error.status);
    if (status === 429) {
      return ERROR_LABELS.http_429;
    }
    if (status === 408) {
      return ERROR_LABELS.http_408;
    }
    if (status >= 500 && status <= 599) {
      return '本地服务或媒体服务器暂时不可用，请稍后重试。';
    }
    var message = redactError(error.message);
    return message || '请求失败，请稍后重试。';
  }

  function isAuthenticationError(error) {
    if (!error || typeof error !== 'object') {
      return false;
    }
    var code = typeof error.code === 'string' ? error.code.toLowerCase() : '';
    var status = Number(error.status);
    if (status === 401 || status === 403) {
      return true;
    }
    if (/(?:^|[_-])(401|403)(?:$|[_-])/.test(code) || /(?:unauthori[sz]ed|forbidden|access[_ -]?denied|auth(?:entication)?[_ -]?required)/.test(code)) {
      return true;
    }
    var message = typeof error.message === 'string' ? error.message : '';
    return /(?:\b401\b|\b403\b|unauthori[sz]ed|forbidden|access denied|authentication required)/i.test(message);
  }

  function redactError(value) {
    var message = typeof value === 'string' ? value : '';
    if (!message) {
      return '';
    }
    message = message.replace(/https?:\/\/[^\s"'<>]+/gi, function (value) {
      try {
        var url = new URL(value);
        return url.origin + url.pathname;
      } catch (error) {
        return '[地址已隐藏]';
      }
    });
    message = message.replace(/\b(cookie|authorization|proxy-authorization|referer|origin|user-agent)\s*[:=]\s*[^\r\n;,]+/gi, '$1: [已隐藏]');
    message = message.replace(/\b(token|signature|sig|auth|key|session|password|passwd)(?:[_-]?(?:token|id))?\s*=\s*[^&\s;,]+/gi, '$1=[已隐藏]');
    message = message.replace(/[\r\n\t]+/g, ' ').trim();
    if (message.length > 240) {
      message = message.slice(0, 237) + '…';
    }
    return message;
  }

  function makeAPIError(code, message, status) {
    var error = new Error('api request failed');
    error.code = typeof code === 'string' ? code : '';
    error.message = typeof message === 'string' ? message : '';
    error.status = status || 0;
    return error;
  }

  async function apiRequest(path, options) {
    var response;
    try {
      response = await fetch(path, options || {});
    } catch (error) {
      throw makeAPIError('network_error', '', 0);
    }

    var payload = null;
    try {
      payload = await response.json();
    } catch (error) {
      payload = null;
    }

    if (!response.ok) {
      var apiError = payload && payload.error ? payload.error : {};
      throw makeAPIError(apiError.code, apiError.message, response.status);
    }
    return payload;
  }

  function jsonOptions(method, body) {
    return {
      method: method,
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body)
    };
  }

  function readTransientRequest() {
    return {
      url: mediaUrl.value.trim(),
      referer: referer.value.trim(),
      origin: origin.value.trim(),
      user_agent: userAgent.value.trim(),
      cookie: cookie.value
    };
  }

  function readTaskRequest() {
    var request = readTransientRequest();
    request.output_dir = outputDir.value.trim();
    request.output_name = outputName.value.trim();
    request.format = format.value;
    request.hls_variant_index = selectedVariantIndex();
    return request;
  }

  function selectedVariantIndex() {
    if (inspectedMediaURL !== mediaUrl.value.trim()) {
      return null;
    }
    var selected = inspectResult.querySelector('input[name="hlsVariantIndex"]:checked');
    if (!selected || selected.value === 'auto') {
      return null;
    }
    var index = Number(selected.value);
    if (!Number.isInteger(index) || inspectedVariantIndexes.indexOf(index) < 0) {
      return null;
    }
    return index;
  }

  function validateURL() {
    if (mediaUrl.value.trim()) {
      mediaUrl.setCustomValidity('');
      return true;
    }
    mediaUrl.setCustomValidity('请输入媒体 URL');
    mediaUrl.reportValidity();
    return false;
  }

  function setFormBusy(busy) {
    formRequestInFlight = busy;
    inspectButton.disabled = busy;
    startButton.disabled = busy;
    taskForm.setAttribute('aria-busy', busy ? 'true' : 'false');
    startButton.textContent = busy ? '提交中…' : '开始下载';
  }

  function clearSensitiveInputs() {
    mediaUrl.value = '';
    referer.value = '';
    origin.value = '';
    userAgent.value = '';
    cookie.value = '';
    inspectedMediaURL = '';
    inspectedVariantIndexes = [];
    inspectPanel.hidden = true;
    inspectResult.replaceChildren();
  }

  function utf8ByteLength(value) {
    if (typeof TextEncoder === 'function') {
      return new TextEncoder().encode(value).length;
    }
    try {
      return encodeURIComponent(value).replace(/%[0-9A-F]{2}|./g, 'x').length;
    } catch (error) {
      return Infinity;
    }
  }

  function validHTTPURL(value, maxBytes) {
    if (typeof value !== 'string' || !value || utf8ByteLength(value) > maxBytes) {
      return '';
    }
    var candidate = value.trim();
    if (!candidate || utf8ByteLength(candidate) > maxBytes) {
      return '';
    }
    try {
      var parsed = new URL(candidate);
      if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
        return '';
      }
    } catch (error) {
      return '';
    }
    return candidate;
  }

  function validHTTPOrigin(value, maxBytes) {
    if (value === 'null') {
      return value;
    }
    var candidate = validHTTPURL(value, maxBytes);
    if (!candidate) {
      return '';
    }
    try {
      var parsed = new URL(candidate);
      if (parsed.username || parsed.password || parsed.pathname !== '/' || parsed.search || parsed.hash) {
        return '';
      }
      return parsed.origin;
    } catch (error) {
      return '';
    }
  }

  function validHeaderValue(value, maxBytes) {
    if (typeof value !== 'string' || !value || utf8ByteLength(value) > maxBytes) {
      return '';
    }
    var candidate = value.trim();
    if (!candidate || utf8ByteLength(candidate) > maxBytes || /[\r\n]/.test(candidate)) {
      return '';
    }
    return candidate;
  }

  function clearFragment() {
    window.history.replaceState(null, document.title, window.location.pathname + window.location.search);
  }

  function receiveExtensionHandoff() {
    var hash = window.location.hash;
    if (!hash || hash === '#') {
      return;
    }

    var params;
    try {
      params = new URLSearchParams(hash.slice(1));
    } catch (error) {
      return;
    }
    if (params.get('from') !== 'extension') {
      return;
    }

    // Remove the handoff before touching any values so a reload or
    // navigation cannot replay the signed candidate from the hash.
    clearFragment();

    var source = validHTTPURL(params.get('source') || '', HANDOFF_SOURCE_LIMIT);
    var incomingReferer = params.get('referer') || '';
    var handoffReferer = incomingReferer ? validHTTPURL(incomingReferer, HANDOFF_REFERER_LIMIT) : '';
    var incomingOrigin = params.get('origin') || '';
    var handoffOrigin = incomingOrigin ? validHTTPOrigin(incomingOrigin, HANDOFF_ORIGIN_LIMIT) : '';
    var incomingUserAgent = params.get('user_agent') || '';
    var handoffUserAgent = incomingUserAgent ? validHeaderValue(incomingUserAgent, HANDOFF_USER_AGENT_LIMIT) : '';

    if (!source || (incomingReferer && !handoffReferer) || (incomingOrigin && !handoffOrigin) || (incomingUserAgent && !handoffUserAgent)) {
      setMessage(globalMessage, '扩展候选无效或过长，已忽略。', 'error');
      return;
    }

    mediaUrl.value = source;
    if (handoffReferer) {
      referer.value = handoffReferer;
    }
    if (handoffOrigin) {
      origin.value = handoffOrigin;
    }
    if (handoffUserAgent) {
      userAgent.value = handoffUserAgent;
    }
    setMessage(globalMessage, '已从扩展接收候选，请确认后检查或下载。', 'success');
  }

  function formatDuration(value) {
    if (value === null || value === undefined || value === '') {
      return '未知';
    }
    var milliseconds = Number(value);
    if (!Number.isFinite(milliseconds) || milliseconds < 0) {
      return '未知';
    }
    var totalSeconds = Math.round(milliseconds / 1000);
    var hours = Math.floor(totalSeconds / 3600);
    var minutes = Math.floor((totalSeconds % 3600) / 60);
    var seconds = totalSeconds % 60;
    if (hours > 0) {
      return hours + ':' + String(minutes).padStart(2, '0') + ':' + String(seconds).padStart(2, '0');
    }
    return minutes + ':' + String(seconds).padStart(2, '0');
  }

  function formatBytes(value) {
    if (value === null || value === undefined || value === '') {
      return '未知';
    }
    var bytes = Number(value);
    if (!Number.isFinite(bytes) || bytes < 0) {
      return '未知';
    }
    if (bytes < 1024) {
      return Math.round(bytes) + ' B';
    }
    var units = ['KB', 'MB', 'GB', 'TB'];
    var size = bytes;
    var unit = 'B';
    for (var i = 0; i < units.length && size >= 1024; i += 1) {
      size /= 1024;
      unit = units[i];
    }
    return size.toFixed(size >= 100 ? 0 : size >= 10 ? 1 : 2) + ' ' + unit;
  }

  function formatTime(value) {
    if (!value) {
      return '时间未知';
    }
    var date = new Date(value);
    if (Number.isNaN(date.getTime())) {
      return '时间未知';
    }
    return date.toLocaleString('zh-CN', { year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' });
  }

  function appendInfoItem(parent, label, value) {
    var item = createElement('div', 'info-item');
    item.appendChild(createElement('dt', '', label));
    item.appendChild(createElement('dd', '', value === undefined || value === null || value === '' ? '未知' : value));
    parent.appendChild(item);
  }

  function streamKind(value) {
    var labels = { video: '视频', audio: '音频', subtitle: '字幕', data: '数据' };
    return labels[value] || (value ? '其他' : '未知');
  }

  function streamDetails(stream) {
    if (!stream || typeof stream !== 'object') {
      return '未知';
    }
    if (stream.kind === 'video' && Number(stream.width) > 0 && Number(stream.height) > 0) {
      return String(stream.width) + ' × ' + String(stream.height);
    }
    if (stream.kind === 'audio' && Number(stream.channels) > 0) {
      return String(stream.channels) + ' 声道';
    }
    return '—';
  }

  function isObject(value) {
    return value !== null && typeof value === 'object' && !Array.isArray(value);
  }

  function firstValue() {
    for (var i = 0; i < arguments.length; i += 1) {
      var value = arguments[i];
      if (value !== undefined && value !== null && value !== '') {
        return value;
      }
    }
    return undefined;
  }

  function hlsSummary(info) {
    if (!isObject(info)) {
      return null;
    }
    var nested = firstValue(info.hls, info.hls_summary, info.hls_info, info.playlist);
    if (isObject(nested)) {
      return nested;
    }
    var fields = ['playlist_type', 'playlistType', 'availability', 'is_live', 'isLive', 'live', 'vod', 'variants', 'segment_count', 'segmentCount', 'encrypted', 'encryption', 'encryption_methods', 'encryptionMethods', 'segment_type', 'segmentType', 'segment_format', 'segmentFormat', 'fmp4', 'has_fmp4', 'byterange', 'byte_range', 'has_byterange'];
    return fields.some(function (field) { return Object.prototype.hasOwnProperty.call(info, field); }) ? info : null;
  }

  function booleanValue() {
    for (var i = 0; i < arguments.length; i += 1) {
      var value = arguments[i];
      if (typeof value === 'boolean') {
        return value;
      }
      if (typeof value === 'number' && (value === 0 || value === 1)) {
        return value === 1;
      }
      if (typeof value === 'string') {
        if (/^(true|yes|1|live|vod)$/i.test(value)) {
          return !/^vod$/i.test(value);
        }
        if (/^(false|no|0)$/i.test(value)) {
          return false;
        }
      }
    }
    return undefined;
  }

  function formatPlaylistMode(summary) {
    var playlistType = firstValue(summary.playlist_type, summary.playlistType, summary.type);
    var normalizedType = typeof playlistType === 'string' ? playlistType.toLowerCase() : '';
    var availability = firstValue(summary.availability, summary.mode, summary.live_vod, summary.liveVOD);
    var normalizedAvailability = typeof availability === 'string' ? availability.toLowerCase() : '';
    var isLive = booleanValue(summary.is_live, summary.isLive, summary.live);
    if (isLive === undefined && summary.vod !== undefined) {
      var isVOD = booleanValue(summary.vod);
      if (isVOD !== undefined) {
        isLive = !isVOD;
      }
    }
    if (isLive === true || normalizedAvailability === 'live' || normalizedAvailability === 'event' || normalizedType === 'live' || normalizedType === 'event') {
      return 'Live';
    }
    if (isLive === false || normalizedAvailability === 'vod' || normalizedAvailability === 'video-on-demand' || normalizedType === 'vod' || normalizedType === 'video-on-demand') {
      return 'VOD';
    }
    if (normalizedAvailability === 'unknown' || normalizedType === 'unknown') {
      return '未知';
    }
    return availability || playlistType || '未知';
  }

  function formatPlaylistType(summary) {
    var value = firstValue(summary.playlist_type, summary.playlistType, summary.type, summary.kind);
    if (typeof value !== 'string') {
      return 'HLS';
    }
    var normalized = value.toLowerCase();
    if (normalized === 'master' || normalized === 'multivariant' || normalized === 'multivariant_playlist') {
      return '主清单（多画质）';
    }
    if (normalized === 'media' || normalized === 'media_playlist') {
      return '媒体清单';
    }
    return value;
  }

  function formatBandwidth(value) {
    var bandwidth = Number(value);
    if (!Number.isFinite(bandwidth) || bandwidth <= 0) {
      return '未知';
    }
    if (bandwidth >= 1000000) {
      return (bandwidth / 1000000).toFixed(bandwidth >= 10000000 ? 0 : 1) + ' Mbps';
    }
    if (bandwidth >= 1000) {
      return Math.round(bandwidth / 1000) + ' kbps';
    }
    return Math.round(bandwidth) + ' bps';
  }

  function formatResolution(variant) {
    if (!isObject(variant)) {
      return '未知';
    }
    var resolution = firstValue(variant.resolution, variant.resolution_label, variant.resolutionLabel);
    if (resolution) {
      return String(resolution);
    }
    var width = Number(firstValue(variant.width, variant.video_width));
    var height = Number(firstValue(variant.height, variant.video_height));
    if (Number.isFinite(width) && width > 0 && Number.isFinite(height) && height > 0) {
      return Math.round(width) + ' × ' + Math.round(height);
    }
    return '未知';
  }

  function variantList(summary) {
    var values = firstValue(summary.variants, summary.variant_streams, summary.renditions);
    return Array.isArray(values) ? values : [];
  }

  function variantIndex(variant, fallback) {
    if (!isObject(variant)) {
      return null;
    }
    var value = Number(variant.index);
    if (Number.isInteger(value) && value >= 0) {
      return value;
    }
    return Number.isInteger(fallback) && fallback >= 0 ? fallback : null;
  }

  function variantLabel(variant, index) {
    var resolution = formatResolution(variant);
    if (resolution !== '未知') {
      return resolution;
    }
    return '画质 ' + String(index + 1);
  }

  function renderVariantTable(parent, variants) {
    if (!variants.length) {
      return;
    }
    var validVariants = [];
    variants.forEach(function (variant, position) {
      var index = variantIndex(variant, position);
      if (!isObject(variant) || index === null || inspectedVariantIndexes.indexOf(index) >= 0) {
        return;
      }
      inspectedVariantIndexes.push(index);
      validVariants.push({ variant: variant, index: index, position: position });
    });
    if (!validVariants.length) {
      return;
    }

    var picker = createElement('fieldset', 'variant-picker');
    picker.appendChild(createElement('legend', '', '下载画质'));
    var automatic = createElement('label', 'variant-choice');
    var automaticInput = document.createElement('input');
    automaticInput.type = 'radio';
    automaticInput.name = 'hlsVariantIndex';
    automaticInput.value = 'auto';
    automaticInput.checked = true;
    automatic.appendChild(automaticInput);
    automatic.appendChild(createElement('span', '', '自动（最佳可用流）'));
    picker.appendChild(automatic);
    picker.appendChild(createElement('p', 'variant-choice-note', '选择具体画质后，任务会把对应的 variant index 交给服务端；签名地址仍不会显示或保存。'));
    parent.appendChild(picker);

    var tableWrap = createElement('div', 'stream-table-wrap');
    var table = createElement('table', 'stream-table variant-table');
    table.appendChild(createElement('caption', 'sr-only', 'HLS 可用画质'));
    var head = createElement('thead');
    var headRow = createElement('tr');
    ['选择', '分辨率', '带宽', '编码'].forEach(function (label) { headRow.appendChild(createElement('th', '', label)); });
    head.appendChild(headRow);
    table.appendChild(head);
    var body = createElement('tbody');
    validVariants.forEach(function (entry) {
      var variant = entry.variant;
      var row = createElement('tr');
      var choiceCell = createElement('td');
      var choice = createElement('label', 'variant-choice');
      var choiceInput = document.createElement('input');
      choiceInput.type = 'radio';
      choiceInput.name = 'hlsVariantIndex';
      choiceInput.value = String(entry.index);
      choiceInput.setAttribute('aria-label', '选择 ' + variantLabel(variant, entry.position));
      choice.appendChild(choiceInput);
      choiceCell.appendChild(choice);
      row.appendChild(choiceCell);
      row.appendChild(createElement('td', '', formatResolution(variant)));
      row.appendChild(createElement('td', '', formatBandwidth(firstValue(variant.bandwidth, variant.bandwidth_bps, variant.average_bandwidth, variant.averageBandwidth))));
      row.appendChild(createElement('td', '', firstValue(variant.codecs, variant.codec, variant.video_codec, variant.videoCodec) || '—'));
      body.appendChild(row);
    });
    if (!body.childNodes.length) {
      return;
    }
    parent.appendChild(createElement('h3', 'stream-heading', '可用画质'));
    table.appendChild(body);
    tableWrap.appendChild(table);
    parent.appendChild(tableWrap);
  }

  function formatEncryption(summary) {
    var value = firstValue(summary.encryption, summary.encryption_method, summary.key_method, summary.keyMethod);
    if (isObject(value)) {
      value = firstValue(value.method, value.type, value.name);
    }
    if (value) {
      return String(value);
    }
    var methods = firstValue(summary.encryption_methods, summary.encryptionMethods);
    if (Array.isArray(methods)) {
      var methodLabels = methods.filter(function (method) { return method !== undefined && method !== null && method !== ''; }).map(function (method) { return String(method); });
      return methodLabels.length ? methodLabels.join('、') : '未检测到';
    }
    var encrypted = booleanValue(summary.encrypted, summary.is_encrypted, summary.has_key);
    if (encrypted === true) {
      return '已检测到加密';
    }
    if (encrypted === false) {
      return '未检测到';
    }
    if (summary.playlist_type || summary.playlistType) {
      return '未检测到';
    }
    return '未知';
  }

  function formatSegmentType(summary) {
    var types = firstValue(summary.segment_types, summary.segmentTypes);
    var labels = [];
    if (Array.isArray(types)) {
      types.forEach(function (type) {
        if (type !== undefined && type !== null && type !== '') { labels.push(String(type)); }
      });
    } else if (types) {
      labels.push(String(types));
    }
    var segmentFormat = firstValue(summary.segment_format, summary.segmentFormat);
    if (segmentFormat) {
      var normalizedFormat = String(segmentFormat).toLowerCase();
      if (normalizedFormat !== 'unknown') {
        var formatLabel = normalizedFormat === 'fmp4' || normalizedFormat === 'fragmented_mp4' || normalizedFormat === 'fragmented-mp4' ? 'fMP4' : normalizedFormat === 'ts' || normalizedFormat === 'mpegts' || normalizedFormat === 'mpeg-ts' ? 'TS' : String(segmentFormat);
        if (!labels.some(function (value) { return value.toLowerCase() === formatLabel.toLowerCase(); })) { labels.push(formatLabel); }
      }
    }
    var fmp4 = booleanValue(summary.fmp4, summary.is_fmp4, summary.has_fmp4, summary.fragmented_mp4);
    var byterange = booleanValue(summary.byterange, summary.byte_range, summary.has_byterange, summary.hasByteRange);
    if (fmp4 === true && !labels.some(function (value) { return /fmp4|fragmented/i.test(value); })) { labels.push('fMP4'); }
    if (byterange === true && !labels.some(function (value) { return /byte.?range/i.test(value); })) { labels.push('BYTERANGE'); }
    return labels.length ? labels.join('、') : '未知';
  }

  function formatSegmentCount(summary) {
    var value = firstValue(summary.segment_count, summary.segmentCount, summary.segments);
    if (Array.isArray(value)) {
      value = value.length;
    }
    var count = Number(value);
    var playlistType = String(firstValue(summary.playlist_type, summary.playlistType, summary.type) || '').toLowerCase();
    if ((playlistType === 'master' || playlistType === 'multivariant') && count === 0) {
      return '不适用';
    }
    return Number.isFinite(count) && count >= 0 ? String(Math.round(count)) : '未知';
  }

  function renderHLSInfo(parent, info) {
    var summary = hlsSummary(info);
    if (!summary) {
      return;
    }
    var variants = variantList(summary);
    var hasSummary = firstValue(summary.playlist_type, summary.playlistType, summary.type, summary.is_live, summary.isLive, summary.live, summary.vod, summary.segment_count, summary.segmentCount, summary.segments, summary.encryption, summary.encrypted, summary.segment_type, summary.segmentType, summary.fmp4, summary.has_fmp4, summary.byterange, summary.has_byterange) !== undefined;
    if (!hasSummary && !variants.length) {
      return;
    }
    parent.appendChild(createElement('h3', 'stream-heading', 'HLS 预检'));
    var summaryGrid = createElement('dl', 'info-grid');
    appendInfoItem(summaryGrid, '播放列表', formatPlaylistType(summary));
    appendInfoItem(summaryGrid, '类型', formatPlaylistMode(summary));
    appendInfoItem(summaryGrid, '加密', formatEncryption(summary));
    appendInfoItem(summaryGrid, '分片', formatSegmentType(summary));
    appendInfoItem(summaryGrid, '分片数量', formatSegmentCount(summary));
    parent.appendChild(summaryGrid);
    renderVariantTable(parent, variants);
    if (variants.length > 1) {
      parent.appendChild(createElement('p', 'inspect-note', '检测到多档画质；默认使用自动模式，也可以在上方选择具体 variant。'));
    }
  }

  function renderInspect(info) {
    inspectPanel.hidden = false;
    inspectResult.replaceChildren();
    inspectedMediaURL = mediaUrl.value.trim();
    inspectedVariantIndexes = [];
    var summary = createElement('dl', 'info-grid');
    appendInfoItem(summary, '格式', info && info.format);
    appendInfoItem(summary, '时长', formatDuration(info && info.duration_ms));
    appendInfoItem(summary, '大小', formatBytes(info && info.size_bytes));
    inspectResult.appendChild(summary);

    renderHLSInfo(inspectResult, info || {});

    inspectResult.appendChild(createElement('h3', 'stream-heading', '媒体轨道'));
    var streams = info && Array.isArray(info.streams) ? info.streams : [];
    if (!streams.length) {
      inspectResult.appendChild(createElement('p', 'empty-state', '服务没有返回轨道信息。'));
      return;
    }

    var tableWrap = createElement('div', 'stream-table-wrap');
    var table = createElement('table', 'stream-table');
    var caption = createElement('caption', 'sr-only', '检测到的媒体轨道');
    table.appendChild(caption);
    var head = createElement('thead');
    var headRow = createElement('tr');
    ['类型', '编码', '语言', '详情'].forEach(function (label) { headRow.appendChild(createElement('th', '', label)); });
    head.appendChild(headRow);
    table.appendChild(head);
    var body = createElement('tbody');
    streams.forEach(function (stream) {
      var row = createElement('tr');
      row.appendChild(createElement('td', '', streamKind(stream && stream.kind)));
      row.appendChild(createElement('td', '', stream && stream.codec ? stream.codec : '—'));
      row.appendChild(createElement('td', '', stream && stream.language ? stream.language : '—'));
      row.appendChild(createElement('td', '', streamDetails(stream)));
      body.appendChild(row);
    });
    table.appendChild(body);
    tableWrap.appendChild(table);
    inspectResult.appendChild(tableWrap);
  }

  function cloneTaskRequest(request) {
    var copy = {};
    if (!request || typeof request !== 'object') {
      return copy;
    }
    Object.keys(request).forEach(function (key) {
      var value = request[key];
      if (typeof value === 'string' || typeof value === 'number' || value === null || typeof value === 'boolean') {
        copy[key] = value;
      }
    });
    return copy;
  }

  function rememberTaskRequest(id, request) {
    if (!id || !request || typeof request !== 'object') {
      return;
    }
    transientTaskRequests[id] = cloneTaskRequest(request);
  }

  function forgetTaskRequest(id) {
    if (id) {
      delete transientTaskRequests[id];
    }
  }

  function taskRequestFor(id) {
    return id && transientTaskRequests[id] ? transientTaskRequests[id] : null;
  }

  function taskErrorMessage(task) {
    if (!task || (!task.error_code && !task.error_message)) {
      return '';
    }
    return friendlyError({
      code: task.error_code,
      message: task.error_message,
      status: task.error_status
    });
  }

  function taskStatus(task) {
    var value = task && typeof task.status === 'string' ? task.status : '';
    return STATUS_LABELS[value] || '状态未知';
  }

  function outputPath(task) {
    var directory = task && typeof task.output_dir === 'string' ? task.output_dir : '';
    var name = task && typeof task.output_name === 'string' ? task.output_name : '';
    if (!directory) { return name || '未指定'; }
    if (!name) { return directory; }
    return directory.replace(/[\\/]$/, '') + '/' + name;
  }

  function progressKnown(task) {
    if (!task || typeof task !== 'object') { return false; }
    if (task.status === 'succeeded') { return true; }
    var progress = Number(task.progress);
    return Number.isFinite(progress) && progress > 0 && progress <= 1;
  }

  function appendTaskMeta(parent, label, value) {
    var line = createElement('div', 'task-line');
    line.appendChild(createElement('span', 'label', label));
    line.appendChild(createElement('span', '', value));
    parent.appendChild(line);
  }

  function renderProgress(parent, task) {
    var line = createElement('div', 'progress-line');
    if (progressKnown(task)) {
      var value = task.status === 'succeeded' ? 1 : Number(task.progress);
      value = Math.max(0, Math.min(1, value));
      var progress = document.createElement('progress');
      progress.max = 100;
      progress.value = Math.round(value * 100);
      progress.setAttribute('aria-label', '下载进度');
      line.appendChild(progress);
      line.appendChild(createElement('span', 'progress-value', Math.round(value * 100) + '%'));
    } else {
      var unknown = task && task.status === 'queued' ? '等待开始' : task && task.status === 'running' ? '处理中（进度未知）' : '进度未知';
      line.appendChild(createElement('span', 'progress-value', unknown));
    }
    parent.appendChild(line);
  }

  function renderTaskCard(task) {
    var card = createElement('article', 'task-card');
    var head = createElement('div', 'task-card-head');
    head.appendChild(createElement('h4', 'task-name', task && task.output_name ? task.output_name : '未命名文件'));
    var statusClass = task && STATUS_LABELS[task.status] ? task.status : '';
    head.appendChild(createElement('span', 'status-badge ' + statusClass, taskStatus(task)));
    card.appendChild(head);
    appendTaskMeta(card, '来源', task && task.source_display ? task.source_display : '未提供');
    appendTaskMeta(card, '文件', outputPath(task));
    appendTaskMeta(card, '时间', '创建于 ' + formatTime(task && task.created_at) + ' · 更新于 ' + formatTime(task && task.updated_at));
    if (task && Number(task.bytes_done) > 0) { appendTaskMeta(card, '已处理', formatBytes(task.bytes_done)); }
    renderProgress(card, task);

    var errorMessage = taskErrorMessage(task);
    if (errorMessage) {
      var error = createElement('p', 'task-error');
      error.appendChild(createElement('strong', '', '错误：'));
      error.appendChild(document.createTextNode(errorMessage));
      card.appendChild(error);
    }

    if (task && task.id && (ACTIVE_STATUSES[task.status] || (RETRYABLE_STATUSES[task.status] && taskRequestFor(String(task.id))))) {
      var actionRow = createElement('div', 'task-actions');
      if (ACTIVE_STATUSES[task.status]) {
        var cancel = createElement('button', 'button small', '取消任务');
        cancel.type = 'button';
        cancel.addEventListener('click', function () { cancelTask(String(task.id), cancel); });
        actionRow.appendChild(cancel);
      } else if (RETRYABLE_STATUSES[task.status] && taskRequestFor(String(task.id))) {
        var retry = createElement('button', 'button small', '重试');
        retry.type = 'button';
        retry.addEventListener('click', function () { retryTask(String(task.id), retry); });
        actionRow.appendChild(retry);
      }
      card.appendChild(actionRow);
    }
    return card;
  }

  function renderTaskList(container, tasks, emptyText) {
    container.replaceChildren();
    if (!tasks.length) {
      container.appendChild(createElement('p', 'empty-state', emptyText));
      return;
    }
    tasks.forEach(function (task) { container.appendChild(renderTaskCard(task)); });
  }

  function renderTasks(tasks) {
    taskData = Array.isArray(tasks) ? tasks : [];
    taskData.forEach(function (task) {
      if (task && task.id) {
        var id = String(task.id);
        if (task.status === 'succeeded' || task.status === 'canceled') {
          forgetTaskRequest(id);
        }
      }
    });
    var current = taskData.filter(function (task) { return task && ACTIVE_STATUSES[task.status]; });
    var history = taskData.filter(function (task) { return !task || !ACTIVE_STATUSES[task.status]; });
    activeCount.textContent = current.length ? '(' + current.length + ')' : '';
    renderTaskList(activeTasks, current, '暂无进行中的任务。');
    renderTaskList(historyTasks, history, '还没有历史任务。');
  }

  function hasActiveTasks() {
    return taskData.some(function (task) { return task && ACTIVE_STATUSES[task.status]; });
  }

  function scheduleTaskPolling() {
    if (pollTimer) { window.clearTimeout(pollTimer); }
    var delay = document.visibilityState === 'visible' && hasActiveTasks() ? 1000 : 5000;
    pollTimer = window.setTimeout(function () { pollTimer = 0; loadTasks(true); }, delay);
  }

  function mergeTaskLists() {
    var merged = new Map();
    Array.prototype.slice.call(arguments).forEach(function (tasks) {
      if (!Array.isArray(tasks)) { return; }
      tasks.forEach(function (task) {
        if (!task || !task.id) { return; }
        merged.set(String(task.id), task);
      });
    });
    return Array.from(merged.values()).sort(function (left, right) {
      return new Date(right.created_at || 0).getTime() - new Date(left.created_at || 0).getTime();
    });
  }

  async function loadTasks(silent) {
    if (taskRequestInFlight) { return; }
    taskRequestInFlight = true;
    refreshButton.disabled = true;
    try {
      var payloads = await Promise.all([
        apiRequest('/api/tasks?status=queued', { method: 'GET' }),
        apiRequest('/api/tasks?status=running', { method: 'GET' }),
        apiRequest('/api/tasks?limit=200', { method: 'GET' })
      ]);
      var tasks = mergeTaskLists(
        payloads[0] && payloads[0].tasks,
        payloads[1] && payloads[1].tasks,
        payloads[2] && payloads[2].tasks
      );
      renderTasks(tasks);
      if (!silent) { setMessage(taskMessage, '任务列表已更新。', 'success'); }
    } catch (error) {
      if (!taskData.length) { renderTasks([]); }
      setMessage(taskMessage, friendlyError(error), 'error');
    } finally {
      taskRequestInFlight = false;
      refreshButton.disabled = false;
      scheduleTaskPolling();
    }
  }

  async function loadHealth() {
    if (healthRequestInFlight) { return; }
    healthRequestInFlight = true;
    try {
      var payload = await apiRequest('/api/health', { method: 'GET' });
      if (payload && payload.status === 'ok' && payload.available !== false) {
        var version = payload.version ? ' · ' + String(payload.version) : '';
        setHealth(true, '本地服务正常' + version);
      } else {
        setHealth(false, '本地服务不可用');
      }
    } catch (error) {
      setHealth(false, friendlyError(error));
    } finally {
      healthRequestInFlight = false;
    }
  }

  async function inspectMedia() {
    if (formRequestInFlight || !validateURL()) { return; }
    setMessage(formMessage, '正在检查媒体…');
    setFormBusy(true);
    try {
      var info = await apiRequest('/api/inspect', jsonOptions('POST', readTransientRequest()));
      renderInspect(info || {});
      setMessage(formMessage, '检查完成。', 'success');
    } catch (error) {
      setMessage(formMessage, friendlyError(error), 'error');
    } finally {
      setFormBusy(false);
    }
  }

  async function createTask(event) {
    event.preventDefault();
    if (formRequestInFlight || !validateURL()) { return; }
    var request = readTaskRequest();
    setMessage(formMessage, '正在创建任务…');
    setFormBusy(true);
    try {
      var task = await apiRequest('/api/tasks', jsonOptions('POST', request));
      clearSensitiveInputs();
      var taskID = task && task.id ? String(task.id) : '';
      if (taskID) {
        rememberTaskRequest(taskID, request);
      }
      setMessage(formMessage, taskID ? '任务已创建（' + taskID + '）。敏感请求信息已清空。' : '任务已创建，敏感请求信息已清空。', 'success');
      await loadTasks(true);
    } catch (error) {
      setMessage(formMessage, friendlyError(error), 'error');
    } finally {
      setFormBusy(false);
    }
  }

  async function retryTask(id, button) {
    var request = taskRequestFor(id);
    if (!id || !request || (button && button.disabled)) { return; }
    if (button) { button.disabled = true; button.textContent = '重试中…'; }
    try {
      var task = await apiRequest('/api/tasks', jsonOptions('POST', cloneTaskRequest(request)));
      var taskID = task && task.id ? String(task.id) : '';
      forgetTaskRequest(id);
      if (taskID) {
        rememberTaskRequest(taskID, request);
      }
      setMessage(taskMessage, taskID ? '已创建重试任务（' + taskID + '）。' : '已创建重试任务。', 'success');
      await loadTasks(true);
    } catch (error) {
      setMessage(taskMessage, friendlyError(error), 'error');
      if (button) { button.disabled = false; button.textContent = '重试'; }
    }
  }

  async function cancelTask(id, button) {
    if (!id || (button && button.disabled)) { return; }
    if (button) { button.disabled = true; button.textContent = '取消中…'; }
    try {
      await apiRequest('/api/tasks/' + encodeURIComponent(id) + '/cancel', { method: 'POST' });
      forgetTaskRequest(id);
      setMessage(taskMessage, '取消请求已提交。', 'success');
      await loadTasks(true);
    } catch (error) {
      setMessage(taskMessage, friendlyError(error), 'error');
      if (button) { button.disabled = false; button.textContent = '取消任务'; }
    }
  }

  mediaUrl.addEventListener('input', function () {
    if (inspectedMediaURL && mediaUrl.value.trim() !== inspectedMediaURL) {
      inspectedMediaURL = '';
      inspectedVariantIndexes = [];
      inspectPanel.hidden = true;
    }
  });
  inspectButton.addEventListener('click', inspectMedia);
  taskForm.addEventListener('submit', createTask);
  refreshButton.addEventListener('click', function () { loadTasks(false); });
  document.addEventListener('visibilitychange', function () {
    scheduleTaskPolling();
    if (document.visibilityState === 'visible') { loadTasks(true); }
  });

  receiveExtensionHandoff();
  loadHealth();
  window.setInterval(loadHealth, 30000);
  loadTasks(false);
}());
