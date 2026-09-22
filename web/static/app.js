/* =========================================================================
   Avdb 搜索 · 前端交互
   原则：
     1. 页面内容全部由服务端直出，本脚本只做"增强"，禁用 JS 后完全可用；
     2. 绝不使用 innerHTML 拼接外部数据（上游标题是不可信输入）；
     3. 不使用内联事件属性，全部走事件委托，以配合严格的 CSP。
   ========================================================================= */

(function () {
  'use strict';

  /* ------------------------------------------------------------ 工具函数 */

  var toastEl = document.getElementById('toast');
  var toastTimer = null;

  function toast(message) {
    if (!toastEl) return;
    toastEl.textContent = message;      // 用 textContent，杜绝注入
    toastEl.hidden = false;
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { toastEl.hidden = true; }, 2000);
  }

  /* -------------------------------------------------- 缩略图加载失败回退 */
  // error 事件不冒泡，必须在捕获阶段监听。
  document.addEventListener('error', function (event) {
    var el = event.target;
    if (!el || el.tagName !== 'IMG') return;
    var fallback = el.parentElement && el.parentElement.querySelector('.thumb-fallback');
    if (fallback) fallback.hidden = false;
    el.hidden = true;                  // 隐藏而非移除，保持图片占位不跳动
  }, true);

  /* -------------------------------------------------------- 搜索框快捷键 */
  var searchInput = document.getElementById('q');
  document.addEventListener('keydown', function (event) {
    if (event.key !== '/' || event.ctrlKey || event.metaKey || event.altKey) return;
    var tag = (document.activeElement && document.activeElement.tagName) || '';
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
    event.preventDefault();
    if (searchInput) searchInput.focus();
  });

  /* ---------------------------------------------------------------- 列表 */

  var grid = document.getElementById('results');
  if (!grid) return;                   // 无结果页无需后续逻辑

  var cards = Array.prototype.slice.call(grid.querySelectorAll('.card'));
  var siteSel = document.getElementById('f-site');
  var sectionSel = document.getElementById('f-section');
  var categorySel = document.getElementById('f-category');
  var sortSel = document.getElementById('f-sort');
  var resetBtn = document.getElementById('f-reset');
  var countEl = document.getElementById('result-count');
  var noMatchEl = document.getElementById('no-match');
  var activeFlags = [];

  function numberAttr(el, name) {
    var value = parseFloat(el.getAttribute('data-' + name));
    return isFinite(value) ? value : 0;
  }

  function matchesFilters(card) {
    if (siteSel.value && card.getAttribute('data-site') !== siteSel.value) return false;
    if (sectionSel.value && card.getAttribute('data-section') !== sectionSel.value) return false;
    if (categorySel.value && card.getAttribute('data-category') !== categorySel.value) return false;

    if (activeFlags.length) {
      // 前后补空格后用整词匹配，避免 "hd" 误命中 "uhd"。
      var flags = ' ' + (card.getAttribute('data-flags') || '') + ' ';
      for (var i = 0; i < activeFlags.length; i++) {
        if (flags.indexOf(' ' + activeFlags[i] + ' ') === -1) return false;
      }
    }
    return true;
  }

  function sortVisible(visible) {
    var mode = sortSel.value;
    if (mode === 'default') {
      visible.sort(function (a, b) { return numberAttr(a, 'order') - numberAttr(b, 'order'); });
    } else if (mode === 'seeders') {
      visible.sort(function (a, b) { return numberAttr(b, 'seeders') - numberAttr(a, 'seeders'); });
    } else if (mode === 'size-desc') {
      visible.sort(function (a, b) { return numberAttr(b, 'size') - numberAttr(a, 'size'); });
    } else if (mode === 'size-asc') {
      visible.sort(function (a, b) { return numberAttr(a, 'size') - numberAttr(b, 'size'); });
    } else if (mode === 'time-desc') {
      visible.sort(function (a, b) { return numberAttr(b, 'ts') - numberAttr(a, 'ts'); });
    } else if (mode === 'time-asc') {
      // ts 为 0 表示时间解析失败，排到最后而不是冒充"最早"。
      visible.sort(function (a, b) {
        var ta = numberAttr(a, 'ts'), tb = numberAttr(b, 'ts');
        if (ta === 0 && tb === 0) return 0;
        if (ta === 0) return 1;
        if (tb === 0) return -1;
        return ta - tb;
      });
    }
  }

  function apply() {
    var visible = [];
    for (var i = 0; i < cards.length; i++) {
      var ok = matchesFilters(cards[i]);
      cards[i].hidden = !ok;
      if (ok) visible.push(cards[i]);
    }

    sortVisible(visible);

    // 用文档片段一次性重排，避免逐个 append 触发多次重排。
    var fragment = document.createDocumentFragment();
    for (var j = 0; j < visible.length; j++) fragment.appendChild(visible[j]);
    grid.appendChild(fragment);

    if (countEl) countEl.textContent = String(visible.length);
    if (noMatchEl) noMatchEl.hidden = visible.length > 0;
  }

  [siteSel, sectionSel, categorySel, sortSel].forEach(function (el) {
    if (el) el.addEventListener('change', apply);
  });

  var chips = Array.prototype.slice.call(document.querySelectorAll('.chip'));
  chips.forEach(function (chip) {
    chip.addEventListener('click', function () {
      var flag = chip.getAttribute('data-flag');
      var index = activeFlags.indexOf(flag);
      if (index >= 0) {
        activeFlags.splice(index, 1);
        chip.classList.remove('is-on');
        chip.setAttribute('aria-pressed', 'false');
      } else {
        activeFlags.push(flag);
        chip.classList.add('is-on');
        chip.setAttribute('aria-pressed', 'true');
      }
      apply();
    });
  });

  if (resetBtn) {
    resetBtn.addEventListener('click', function () {
      if (siteSel) siteSel.value = '';
      if (sectionSel) sectionSel.value = '';
      if (categorySel) categorySel.value = '';
      if (sortSel) sortSel.value = 'default';
      activeFlags.length = 0;
      chips.forEach(function (chip) {
        chip.classList.remove('is-on');
        chip.setAttribute('aria-pressed', 'false');
      });
      apply();
    });
  }

  /* ------------------------------------------------------- 下载 / 复制 */

  function setStatus(el, kind, text) {
    if (!el) return;
    el.className = 'status' + (kind ? ' is-' + kind : '');
    el.textContent = text || '';
  }

  function submitDownload(button) {
    var tid = button.getAttribute('data-tid');
    if (!tid) return;

    var card = button.closest('.card');
    var statusEl = card ? card.querySelector('.status') : null;
    var original = button.textContent;

    button.disabled = true;
    button.textContent = '提交中…';
    setStatus(statusEl, 'loading', '正在提交…');

    fetch('/download?tid=' + encodeURIComponent(tid), {
      headers: { 'Accept': 'application/json' },
      credentials: 'same-origin'
    })
      .then(function (response) { return response.json(); })
      .then(function (data) {
        if (data && data.success) {
          setStatus(statusEl, 'ok', data.message || '已提交');
          button.textContent = '已提交';
        } else {
          setStatus(statusEl, 'err', (data && data.message) || '提交失败');
          button.textContent = original;
          button.disabled = false;
        }
      })
      .catch(function () {
        setStatus(statusEl, 'err', '网络错误，请重试');
        button.textContent = original;
        button.disabled = false;
      });
  }

  function legacyCopy(text) {
    // http 明文环境下 navigator.clipboard 不可用（非安全上下文），必须降级。
    var area = document.createElement('textarea');
    area.value = text;
    area.setAttribute('readonly', '');
    area.style.position = 'fixed';
    area.style.top = '-1000px';
    area.style.opacity = '0';
    document.body.appendChild(area);
    area.select();
    var ok = false;
    try { ok = document.execCommand('copy'); } catch (e) { ok = false; }
    document.body.removeChild(area);
    return ok;
  }

  function copyMagnet(text) {
    if (!text) return;
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(
        function () { toast('已复制磁力链接'); },
        function () { toast(legacyCopy(text) ? '已复制磁力链接' : '复制失败，请手动选择'); }
      );
      return;
    }
    toast(legacyCopy(text) ? '已复制磁力链接' : '复制失败，请手动选择');
  }

  grid.addEventListener('click', function (event) {
    var button = event.target.closest ? event.target.closest('button[data-act]') : null;
    if (!button) return;
    var action = button.getAttribute('data-act');
    if (action === 'download') {
      submitDownload(button);
    } else if (action === 'copy') {
      copyMagnet(button.getAttribute('data-magnet'));
    }
  });

  // 首次渲染后同步一次计数与空态（服务端已给出默认值，这里保持一致）。
  apply();
})();
