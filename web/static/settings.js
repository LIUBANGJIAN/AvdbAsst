/* =========================================================================
   资源搜索 · 设置页交互
   四件事，彼此独立，任一失败不影响其余：
     1. 测试连接：保存前先探一次"地址 + 令牌"能不能用；
     2. 探测可用下载器：问上游"你到底配了哪个下载器"，返回可用的标识；
     3. 读上游默认下载器：把上游配好的下载器 + 目录取回来填进输入框；
     4. 校验下载器：用 API Key 问上游"这个标识存在吗、它下面有哪些目录"。

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
  // 刻意忽略空串：上游"没配某项"时返回的是空串，此时把输入框清掉是错的——
  // 用户可能刚手填了一个自己确认可用的标识，一次读取就把它擦掉会很恼人。
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

  var ruleResult = document.getElementById('downloader-result');

  /* -------------------------------------------------- 2. 探测可用下载器 */

  var probeButton = document.getElementById('btn-probe-downloaders');

  if (probeButton && ruleResult) {
    probeButton.addEventListener('click', function () {
      probeButton.disabled = true;
      setResult(ruleResult, 'loading', '正在探测上游配置了哪些下载器…');

      postJSON('/api/settings/downloaders', basePayload())
        .then(function (data) {
          if (!data) {
            setResult(ruleResult, 'err', '探测失败');
            return;
          }
          if (data.downloaders) {
            fillDatalist('downloader-options', data.downloaders.map(function (p) { return p.id; }));
          }
          // 探测到可用标识才回填：没探测到时把字段清空是错误的，
          // 用户可能已经手填了一个自认为可用的值。
          setValue('default_downloader', data.best);
          fillDatalist('save-path-options', data.directories);
          setResult(ruleResult, data.success ? 'ok' : 'err',
            data.message || (data.success ? '探测完成' : '探测失败'));
        })
        .catch(function () {
          setResult(ruleResult, 'err', '请求失败，请检查网络或访问口令');
        })
        .then(function () { probeButton.disabled = false; });
    });
  }

  /* ------------------------------------------------- 3. 读上游默认下载器 */

  var ruleButton = document.getElementById('btn-default-rule');

  if (ruleButton && ruleResult) {
    ruleButton.addEventListener('click', function () {
      ruleButton.disabled = true;
      setResult(ruleResult, 'loading', '正在读取上游默认下载目标…');

      postJSON('/api/settings/default-rule', basePayload())
        .then(function (data) {
          if (!data) {
            setResult(ruleResult, 'err', '读取失败');
            return;
          }
          // 回填与 success 解耦：上游"只配了目录、没配下载器"时 success 为 false，
          // 但那个目录是真实可用的，不能跟着一起丢掉。
          // 服务端已经把这种半成功情形写进了 message，用户看得懂。
          setValue('default_downloader', data.downloader);
          setValue('default_save_path', data.save_path);
          setResult(ruleResult, data.success ? 'ok' : 'err',
            data.message || (data.success ? '已读取' : '读取失败'));
        })
        .catch(function () {
          setResult(ruleResult, 'err', '请求失败，请检查网络或访问口令');
        })
        .then(function () { ruleButton.disabled = false; });
    });
  }

  /* ------------------------------------------------------ 4. 校验下载器 */

  var checkButton = document.getElementById('btn-check-downloader');

  if (checkButton && ruleResult) {
    checkButton.addEventListener('click', function () {
      checkButton.disabled = true;
      setResult(ruleResult, 'loading', '正在校验下载器…');

      var payload = basePayload();
      payload.set('downloader_id', valueOf('default_downloader'));

      postJSON('/api/settings/directories', payload)
        .then(function (data) {
          if (data && data.success) {
            setResult(ruleResult, 'ok', data.message || '校验通过');
            fillDatalist('save-path-options', data.directories);
          } else {
            setResult(ruleResult, 'err', (data && data.message) || '校验失败');
          }
        })
        .catch(function () {
          setResult(ruleResult, 'err', '请求失败，请检查网络或访问口令');
        })
        .then(function () { checkButton.disabled = false; });
    });
  }
})();
