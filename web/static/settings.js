/* =========================================================================
   资源搜索 · 设置页交互
   两件事，彼此独立，任一失败不影响其余：
     1. 测试连接：保存前先探一次"地址 + 令牌"能不能用；
     2. 检测：一次问清"上游配了哪个下载器、它下面有哪些目录"，并回填。

   为什么只有一个「检测」按钮：
     早先拆成三个按钮（探测可用下载器 / 读取上游默认下载器 / 校验并列出目录），
     用户的第一反应是"该点哪个"。而这三件事对用户其实是同一个问题——
     "我这套下载设置到底能不能用"。所以合并成一次调用，结论列成一张清单。
     读上游默认下载器（default-rule）那条路实测在本机上游返回全空，故不再放在界面上，
     接口仍保留在 /api/settings/default-rule 供脚本调用。

   全部只用访问令牌，不需要上游账号密码。

   安全约定：
     - 所有响应都以 textContent / createElement 渲染，绝不拼 innerHTML；
     - 表单本身仍是原生提交，禁用 JS 时"保存设置"照常可用。
   ========================================================================= */

(function () {
  'use strict';

  var form = document.getElementById('settings-form');
  if (!form) return;

  /* ------------------------------------------------------------ 工具函数 */

  function valueOf(id) {
    var el = document.getElementById(id);
    return el ? el.value : '';
  }

  // 只在该字段确实有值时才写回。
  //
  // 刻意忽略空串：上游"没配某项"时返回的是空串，此时把控件清掉是错的——
  // 用户可能刚选好一个自己确认可用的值，一次检测就把它擦掉会很恼人。
  // 空值在这里的含义是"这条信息没有"，不是"请改成空"。
  function setValue(id, value) {
    var el = document.getElementById(id);
    if (el && typeof value === 'string' && value) el.value = value;
  }

  function setResult(el, kind, text) {
    if (!el) return;
    el.className = 'test-result' + (kind ? ' is-' + kind : '');
    el.textContent = text || '';
  }

  function postJSON(url, payload) {
    return fetch(url, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/x-www-form-urlencoded;charset=UTF-8',
        'Accept': 'application/json'
      },
      credentials: 'same-origin',
      body: payload.toString()
    }).then(function (response) { return response.json(); });
  }

  // 探测接口都吃同一组"上游地址 + 令牌"，统一构造。
  // api_key 留空时服务端会沿用已保存的那个，用户不必为了测试重粘令牌。
  function basePayload() {
    var payload = new URLSearchParams();
    payload.set('api_base_url', valueOf('api_base_url'));
    payload.set('api_key', valueOf('api_key'));
    return payload;
  }

  // 用服务端返回的候选重建 datalist；拿不到数据时保持原样，不清空已有提示。
  function fillDatalist(id, values) {
    var list = document.getElementById(id);
    if (!list || !values || !values.length) return;

    list.textContent = '';
    values.forEach(function (value) {
      if (!value) return;
      var option = document.createElement('option');
      option.value = value;
      list.appendChild(option);
    });
  }

  /* ---------------------------------------------------------- 1. 测试连接 */

  var testButton = document.getElementById('btn-test');
  var testResult = document.getElementById('test-result');

  if (testButton && testResult) {
    testButton.addEventListener('click', function () {
      testButton.disabled = true;
      setResult(testResult, 'loading', '正在测试…');

      postJSON('/api/settings/test', basePayload())
        .then(function (data) {
          if (data && data.success) {
            setResult(testResult, 'ok', data.message || '连接正常');
          } else {
            setResult(testResult, 'err', (data && data.message) || '连接失败');
          }
        })
        .catch(function () {
          setResult(testResult, 'err', '测试请求失败，请检查网络或访问口令');
        })
        .then(function () { testButton.disabled = false; });
    });
  }

  /* ------------------------------------------------------ 2. 检测下载设置 */

  // 探测状态 → 界面文案。服务端只给状态码，措辞放在前端，
  // 这样上游换了自己的报错文案，界面也不会冒出一句看不懂的英文。
  var STATUS_LABEL = {
    configured: '可用',
    known: '上游未配置',
    unsupported: '上游不支持此类型',
    error: '探测失败，结论未知'
  };

  var detectButton = document.getElementById('btn-detect');
  var detectStatus = document.getElementById('detect-status');
  var detectReport = document.getElementById('detect-report');

  function clearReport() {
    if (!detectReport) return;
    detectReport.textContent = '';
    detectReport.hidden = true;
  }

  function renderReport(probes, best) {
    if (!detectReport) return;
    detectReport.textContent = '';
    if (!probes.length) {
      detectReport.hidden = true;
      return;
    }

    probes.forEach(function (probe) {
      var item = document.createElement('li');
      item.className = 'detect-item' + (probe.status === 'configured' ? ' is-ok' : ' is-off');
      if (probe.id === best) item.classList.add('is-best');

      var name = document.createElement('span');
      name.className = 'detect-name';
      name.textContent = probe.id;
      item.appendChild(name);

      var label = document.createElement('span');
      label.className = 'detect-label';
      label.textContent = STATUS_LABEL[probe.status] || probe.status;
      item.appendChild(label);

      // 只有"可用但读不出目录"这类需要解释的情况才带 message；
      // 服务端已把它翻译成人话，不会再出现整段 gRPC 堆栈。
      if (probe.message) {
        var note = document.createElement('span');
        note.className = 'detect-note';
        note.textContent = probe.message;
        item.appendChild(note);
      }

      detectReport.appendChild(item);
    });

    detectReport.hidden = false;
  }

  if (detectButton && detectStatus) {
    detectButton.addEventListener('click', function () {
      detectButton.disabled = true;
      setResult(detectStatus, 'loading', '正在检测…');
      clearReport();

      postJSON('/api/settings/downloaders', basePayload())
        .then(function (data) {
          if (!data) {
            setResult(detectStatus, 'err', '检测失败');
            return;
          }
          renderReport(data.downloaders || [], data.best);
          // 探测到可用标识才回填，与 setValue 的"空值不覆盖"口径一致。
          setValue('default_downloader', data.best);
          fillDatalist('save-path-options', data.directories);
          setResult(detectStatus, data.success ? 'ok' : 'err',
            data.message || (data.success ? '检测完成' : '检测失败'));
        })
        .catch(function () {
          setResult(detectStatus, 'err', '请求失败，请检查网络或访问口令');
        })
        .then(function () { detectButton.disabled = false; });
    });
  }
})();
