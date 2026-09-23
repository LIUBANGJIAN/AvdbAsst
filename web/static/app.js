/* =========================================================================
   资源搜索 · 前端交互
   原则：
     1. 页面内容全部由服务端直出，本脚本只做"增强"，禁用 JS 后检索仍然可用；
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
    toastTimer = setTimeout(function () { toastEl.hidden = true; }, 2400);
  }

  /* -------------------------------------------------------- 搜索框快捷键 */
  var searchInput = document.getElementById('q');
  document.addEventListener('keydown', function (event) {
    if (event.key !== '/' || event.ctrlKey || event.metaKey || event.altKey) return;
    var tag = (document.activeElement && document.activeElement.tagName) || '';
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
    event.preventDefault();
    if (searchInput) searchInput.focus();
  });

  /* ------------------------------------------------------ 每页条数即时生效 */
  // 服务端会把选择落盘，所以这里只是"少点一次按钮"。
  var pageSizeSel = document.getElementById('page_size');
  if (pageSizeSel && pageSizeSel.form) {
    pageSizeSel.addEventListener('change', function () { pageSizeSel.form.submit(); });
  }

  /* ---------------------------------------------------------------- 列表 */

  var list = document.getElementById('results');
  if (!list) return;                   // 无结果页无需后续逻辑

  var rows = Array.prototype.slice.call(list.querySelectorAll('.row'));
  var siteSel = document.getElementById('f-site');
  var sectionSel = document.getElementById('f-section');
  var categorySel = document.getElementById('f-category');
  var sortSel = document.getElementById('f-sort');
  var resetBtn = document.getElementById('f-reset');
  var countEl = document.getElementById('result-count');
  var noMatchEl = document.getElementById('no-match');
  var activeFlags = [];

  var batchBar = document.getElementById('batchbar');
  // 操作条常驻底部，吐司要一直往上让位——这个标记只在有批量条的页面上出现。
  if (batchBar) document.body.classList.add('has-batchbar');

  var checkAll = document.getElementById('check-all');
  var selCountEl = document.getElementById('sel-count');
  var batchStatusEl = document.getElementById('batch-status');
  var batchDownloadBtn = document.getElementById('batch-download');
  var batchCopyBtn = document.getElementById('batch-copy');
  var batchClearBtn = document.getElementById('batch-clear');

  function numberAttr(el, name) {
    var value = parseFloat(el.getAttribute('data-' + name));
    return isFinite(value) ? value : 0;
  }

  function matchesFilters(row) {
    if (siteSel.value && row.getAttribute('data-site') !== siteSel.value) return false;
    if (sectionSel.value && row.getAttribute('data-section') !== sectionSel.value) return false;
    if (categorySel.value && row.getAttribute('data-category') !== categorySel.value) return false;

    if (activeFlags.length) {
      // 前后补空格后用整词匹配，避免 "hd" 误命中 "uhd"。
      var flags = ' ' + (row.getAttribute('data-flags') || '') + ' ';
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
    for (var i = 0; i < rows.length; i++) {
      var ok = matchesFilters(rows[i]);
      rows[i].hidden = !ok;
      if (ok) visible.push(rows[i]);
    }

    sortVisible(visible);

    // 用文档片段一次性重排，避免逐个 append 触发多次重排。
    var fragment = document.createDocumentFragment();
    for (var j = 0; j < visible.length; j++) fragment.appendChild(visible[j]);
    list.appendChild(fragment);

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

  function statusOf(tid) {
    return list.querySelector('.status[data-status-for="' + tid + '"]');
  }

  function rowOf(tid) {
    return list.querySelector('.row[data-tid="' + tid + '"]');
  }

  function submitDownload(button) {
    var tid = button.getAttribute('data-tid');
    if (!tid) return;

    var row = button.closest('.row');
    var statusEl = row ? row.querySelector('.status') : null;
    var original = button.textContent;

    button.disabled = true;
    button.textContent = '提交中…';
    setStatus(statusEl, 'loading', '正在提交…');

    // 必须用 POST：提交下载会改变上游状态，服务端也只接受 POST 并校验同源。
    fetch('/download', {
      method: 'POST',
      headers: {
        'Accept': 'application/json',
        'Content-Type': 'application/x-www-form-urlencoded;charset=UTF-8'
      },
      body: 'tid=' + encodeURIComponent(tid),
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

  function writeClipboard(text, label) {
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(
        function () { toast(label); },
        function () { toast(legacyCopy(text) ? label : '复制失败，请手动选择'); }
      );
      return;
    }
    toast(legacyCopy(text) ? label : '复制失败，请手动选择');
  }

  function copyMagnet(text) {
    if (!text) return;
    writeClipboard(text, '已复制磁力链接');
  }

  list.addEventListener('click', function (event) {
    var button = event.target.closest ? event.target.closest('button[data-act]') : null;
    if (!button) return;
    var action = button.getAttribute('data-act');
    if (action === 'download') {
      submitDownload(button);
    } else if (action === 'copy') {
      copyMagnet(button.getAttribute('data-magnet'));
    }
  });

  /* ------------------------------------------------------------ 多选 */

  function checks() {
    return Array.prototype.slice.call(list.querySelectorAll('.row-check'));
  }

  function selectedChecks() {
    return checks().filter(function (box) { return box.checked; });
  }

  function visibleChecks() {
    return checks().filter(function (box) {
      var row = box.closest('.row');
      return row && !row.hidden;
    });
  }

  function syncSelection() {
    var selected = selectedChecks();
    var visible = visibleChecks();
    var visibleSelected = visible.filter(function (box) { return box.checked; }).length;

    if (selCountEl) selCountEl.textContent = String(selected.length);

    // 「全选」按钮的文案随状态切换，比三态复选框直白：
    // 用户永远知道再点一下会发生什么。
    if (checkAll) {
      var allPicked = visible.length > 0 && visibleSelected === visible.length;
      checkAll.textContent = allPicked ? '取消全选' : '全选本页';
      checkAll.setAttribute('aria-pressed', allPicked ? 'true' : 'false');
    }
  }

  list.addEventListener('change', function (event) {
    if (!event.target.classList || !event.target.classList.contains('row-check')) return;
    syncSelection();
  });

  if (checkAll) {
    checkAll.addEventListener('click', function () {
      // 只影响**当前可见**的行：筛选之后全选，用户想选的是他看到的那些。
      // 已经全选时再点一下即取消，按钮文案已经写明了这一点。
      var visible = visibleChecks();
      var allPicked = visible.length > 0 && visible.every(function (box) { return box.checked; });
      visible.forEach(function (box) { box.checked = !allPicked; });
      syncSelection();
    });
  }

  if (batchClearBtn) {
    batchClearBtn.addEventListener('click', function () {
      checks().forEach(function (box) { box.checked = false; });
      syncSelection();
    });
  }

  /* -------------------------------------------------------- 批量下载 */

  // 单批 50 条：服务端一次最多收 100，留一半余量，
  // 这样进度是按"批"推进的，失败也不会一次牵连太多。
  var BATCH_SIZE = 50;

  function chunk(values, size) {
    var out = [];
    for (var i = 0; i < values.length; i += size) out.push(values.slice(i, i + size));
    return out;
  }

  function postBatch(tids) {
    return fetch('/download/batch', {
      method: 'POST',
      headers: {
        'Accept': 'application/json',
        'Content-Type': 'application/x-www-form-urlencoded;charset=UTF-8'
      },
      body: 'tids=' + encodeURIComponent(tids.join(',')),
      credentials: 'same-origin'
    }).then(function (response) { return response.json(); });
  }

  function markResult(item) {
    var statusEl = statusOf(item.tid);
    if (!statusEl) return;
    setStatus(statusEl, item.success ? 'ok' : 'err', item.message || (item.success ? '已提交' : '提交失败'));
  }

  function batchDownload() {
    var selected = selectedChecks();
    if (!selected.length) return;

    if (selected.length > 10) {
      var ok = window.confirm('将向下载器提交 ' + selected.length + ' 条资源，继续吗？');
      if (!ok) return;
    }

    var tids = selected.map(function (box) { return box.value; });

    batchDownloadBtn.disabled = true;
    batchCopyBtn.disabled = true;

    var batches = chunk(tids, BATCH_SIZE);
    var done = 0, okCount = 0, failCount = 0;
    var firstErrors = [];

    function step(index) {
      if (index >= batches.length) {
        var summary = '提交完成：成功 ' + okCount + ' 条，失败 ' + failCount + ' 条';
        if (firstErrors.length) summary += '（' + firstErrors[0] + '）';
        setStatus(batchStatusEl, failCount ? 'err' : 'ok', summary);
        toast(summary);
        batchDownloadBtn.disabled = false;
        batchCopyBtn.disabled = false;
        return;
      }

      var batch = batches[index];
      setStatus(batchStatusEl, 'loading',
        '正在提交 ' + (done + 1) + '–' + (done + batch.length) + ' / ' + tids.length + ' …');

      postBatch(batch)
        .then(function (data) {
          if (data && data.results) {
            data.results.forEach(function (item) {
              markResult(item);
              if (item.success) {
                okCount++;
              } else {
                failCount++;
                if (firstErrors.length < 3 && item.message) firstErrors.push(item.message);
              }
            });
          } else {
            // 整批被拒（例如保存路径没配）：把服务端的原话显示出来，不假装成功。
            failCount += batch.length;
            if (data && data.message && firstErrors.length < 3) firstErrors.push(data.message);
            batch.forEach(function (tid) {
              setStatus(statusOf(tid), 'err', (data && data.message) || '提交失败');
            });
          }
        })
        .catch(function () {
          failCount += batch.length;
          if (firstErrors.length < 3) firstErrors.push('网络错误');
          batch.forEach(function (tid) { setStatus(statusOf(tid), 'err', '网络错误，请重试'); });
        })
        .then(function () {
          done += batch.length;
          step(index + 1);
        });
    }

    setStatus(batchStatusEl, 'loading', '准备提交 ' + tids.length + ' 条…');
    step(0);
  }

  function batchCopy() {
    var selected = selectedChecks();
    if (!selected.length) return;

    var links = [];
    selected.forEach(function (box) {
      var magnet = box.getAttribute('data-magnet');
      if (magnet) links.push(magnet);
    });

    if (!links.length) {
      setStatus(batchStatusEl, 'err', '选中的条目里没有可复制的磁力链接');
      return;
    }

    writeClipboard(links.join('\n'), '已复制 ' + links.length + ' 条磁力链接');
    if (links.length < selected.length) {
      setStatus(batchStatusEl, 'ok',
        '已复制 ' + links.length + ' 条；另有 ' + (selected.length - links.length) + ' 条没有磁链');
      return;
    }
    setStatus(batchStatusEl, 'ok', '已复制 ' + links.length + ' 条磁力链接');
  }

  if (batchDownloadBtn) batchDownloadBtn.addEventListener('click', batchDownload);
  if (batchCopyBtn) batchCopyBtn.addEventListener('click', batchCopy);

  // 首次渲染后同步一次计数、选中态与空态（服务端已给出默认值，这里保持一致）。
  apply();
  syncSelection();
})();
