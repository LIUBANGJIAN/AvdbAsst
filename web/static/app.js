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

/* ------------------------------------------------------------ 筛选条 */
// 筛选条件由**服务端**在全量结果上生效（见 search_filter.go），
// 前端只负责把条件送出去。所以这里不做任何本地过滤：
// 本地过滤只能管到当前页这 100 条，一翻页条件就没了，
// 用户还得重选一遍——那正是这次要修掉的行为。
var filterForm = document.getElementById('filter-form');
if (filterForm) {
  // GET 表单提交会**整体替换** query，所以"少发一个字段"就等于把它清掉。
  // 借这一点把空值与默认值摘掉，否则每选一次站点，地址里都会攒出
  // &section=&category=&filter=&sort=default&page_size=100 这样一串。
  //
  // 但"省略"对每个字段的含义并不相同，规则要分开写：
  //   · 空值：省略 = 这一项没筛，两种语义都成立；
  //   · sort=default：省略 = 用默认排序，也对；
  //   · 每页条数：它是**已持久化的偏好**，没改就别发（见下面的 data-current）。
  // 反过来，一个**非默认**的排序绝不能省——GET 会整体替换 query，
  // 省掉它等于用户改个站点就把排序悄悄重置了。
  //
  // 用摘掉 name 而不是设 disabled：disabled 会让控件变灰，
  // 万一这次提交没发生（被拦下、脚本报错），用户会对着一个点不动的下拉框发呆。
  function dropUntouchedFields() {
    Array.prototype.forEach.call(filterForm.elements, function (el) {
      if (!el.name || el.tagName === 'BUTTON') return;
      var value = el.value;

      if (value === '') {
        el.removeAttribute('name');
        return;
      }
      if (el.name === 'sort' && value === 'default') {
        el.removeAttribute('name');
        return;
      }
      // data-current 是服务端渲染时写下的"当前值"，只有持久化的偏好带它。
      var current = el.getAttribute('data-current');
      if (current !== null && value === current) {
        el.removeAttribute('name');
      }
    });
  }

  // 覆盖回车触发的隐式提交（浏览器自己发起的提交不会走下面几个分支）。
  filterForm.addEventListener('submit', dropUntouchedFields);

  // 下拉框改选即提交，少点一次「应用」。
  Array.prototype.forEach.call(filterForm.querySelectorAll('select'), function (sel) {
    sel.addEventListener('change', function () {
      dropUntouchedFields();
      filterForm.submit();
    });
  });

  var filterInput = document.getElementById('f-filter');
  if (filterInput) {
    // 原生「×」清除按钮不触发提交，这里补一次——否则输入框看着清空了、
    // 结果还是旧的，比没有清除按钮更让人困惑。
    // 按回车时也会触发 search 事件，但那时 value 非空，不会重复提交。
    filterInput.addEventListener('search', function () {
      if (filterInput.value === '') {
        dropUntouchedFields();
        filterForm.submit();
      }
    });
  }
}

/* ---------------------------------------------------------------- 列表 */

var list = document.getElementById('results');
if (!list) return;                   // 无结果页无需后续逻辑

var batchBar = document.getElementById('batchbar');
  // 操作条常驻底部，吐司要一直往上让位——这个标记只在有批量条的页面上出现。
  if (batchBar) document.body.classList.add('has-batchbar');

  var checkAll = document.getElementById('check-all');
  var selCountEl = document.getElementById('sel-count');
  var batchStatusEl = document.getElementById('batch-status');
  var batchDownloadBtn = document.getElementById('batch-download');
  var batchCopyBtn = document.getElementById('batch-copy');
  var batchClearBtn = document.getElementById('batch-clear');

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

  // 筛选已在服务端完成，页面上渲染出来的每一行都是"当前条件下该显示的"，
  // 所以它与全部行等价；仍判断 hidden 是为了防御将来可能出现的客户端隐藏，
  // 否则「全选本页」会把看不见的行也选上。
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

  // 首次渲染后同步一次选中态。结果的条数、筛选后的总数都由服务端算好了，
  // 前端不再重复计算——同一件事有两个真相，迟早会不一致。
  syncSelection();
})();
