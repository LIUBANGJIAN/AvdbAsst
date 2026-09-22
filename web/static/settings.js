/* =========================================================================
   Avdb 搜索 · 设置页交互
   三件事，彼此独立，任一失败不影响其余：
     1. 测试连接：保存前先探一次"地址 + 令牌"能不能用；
     2. 校验下载器：用 API Key 问上游"这个标识存在吗、它下面有哪些目录"；
     3. 登录上游：用户名 + 密码（必要时补动态码）换 JWT，读出下载器清单。

   安全约定：
     - 密码只在提交时出现在请求体里，不写入 localStorage、不写进 URL；
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

  // 三个探测接口都吃同一组"上游地址 + 令牌 + 超时"，统一构造。
  // api_key 留空时服务端会沿用已保存的那个，用户不必为了测试重粘令牌。
  function basePayload() {
    var payload = new URLSearchParams();
    payload.set('api_base_url', valueOf('api_base_url'));
    payload.set('api_key', valueOf('api_key'));
    payload.set('timeout_seconds', valueOf('timeout_seconds'));
    return payload;
  }

  // 用服务端返回的候选重建 datalist；拿不到数据时保持原样，不清空已有提示。
  function fillDatalist(id, values) {
    var list = document.getElementById(id);
    if (!list || !values || !values.length) return;

    list.textContent = '';
    values.forEach(function (item) {
      var value = (typeof item === 'string') ? item : (item && item.id);
      if (!value) return;
      var option = document.createElement('option');
      option.value = value;
      var label = (typeof item === 'string') ? '' : (item.label || '');
      if (label) option.label = label;
      list.appendChild(option);
    });
  }

  function showTrace(id, trace) {
    var box = document.getElementById(id);
    if (!box) return;
    box.textContent = '';
    if (!trace || !trace.length) {
      box.classList.add('is-hidden');
      return;
    }
    var list = document.createElement('ul');
    trace.forEach(function (line) {
      var li = document.createElement('li');
      li.textContent = line;
      list.appendChild(li);
    });
    box.appendChild(list);
    box.classList.remove('is-hidden');
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

  /* ------------------------------------------------------ 2. 校验下载器 */

  var checkButton = document.getElementById('btn-check-downloader');
  var checkResult = document.getElementById('downloader-result');

  if (checkButton && checkResult) {
    checkButton.addEventListener('click', function () {
      checkButton.disabled = true;
      setResult(checkResult, 'loading', '正在校验下载器…');

      var payload = basePayload();
      payload.set('downloader_id', valueOf('default_downloader'));

      postJSON('/api/settings/directories', payload)
        .then(function (data) {
          if (data && data.success) {
            setResult(checkResult, 'ok', data.message || '校验通过');
            fillDatalist('save-path-options', data.directories);
          } else {
            setResult(checkResult, 'err', (data && data.message) || '校验失败');
          }
        })
        .catch(function () {
          setResult(checkResult, 'err', '请求失败，请检查网络或访问口令');
        })
        .then(function () { checkButton.disabled = false; });
    });
  }

  /* ------------------------------------------------------ 3. 登录上游 */

  var loginButton = document.getElementById('btn-login');
  var loginResult = document.getElementById('login-result');
  var otpRow = document.getElementById('otp-row');

  // 两步验证是"两段式"流程，这里记住第一步拿到的 otp_token。
  // 它只活在当前页面内存里，刷新即丢，不写任何持久存储。
  var otpToken = '';

  function reset2FA() {
    otpToken = '';
    if (otpRow) otpRow.classList.add('is-hidden');
    var otp = document.getElementById('upstream_otp');
    if (otp) otp.value = '';
  }

  function doLogin() {
    if (!loginButton) return;
    loginButton.disabled = true;
    setResult(loginResult, 'loading', otpToken ? '正在验证动态码…' : '正在登录上游…');

    var payload = basePayload();
    if (otpToken) {
      payload.set('otp_token', otpToken);
      payload.set('otp_code', valueOf('upstream_otp'));
    } else {
      payload.set('username', valueOf('upstream_username'));
      payload.set('password', valueOf('upstream_password'));
    }

    postJSON('/api/settings/login', payload)
      .then(function (data) {
        // 第一步：账号开了两步验证，展开验证码输入框，等用户补一次。
        if (data && data.need_2fa) {
          otpToken = data.otp_token || '';
          if (otpRow) otpRow.classList.remove('is-hidden');
          setResult(loginResult, 'ok', data.message || '请输入动态验证码后再次点击。');
          var otp = document.getElementById('upstream_otp');
          if (otp) otp.focus();
          return;
        }

        reset2FA();

        if (data && data.success) {
          setResult(loginResult, 'ok', data.message || '已获取下载器清单');
          fillDatalist('downloader-options', data.downloaders);
        } else {
          setResult(loginResult, 'err', (data && data.message) || '获取失败');
        }
        showTrace('login-trace', data && data.trace);
      })
      .catch(function () {
        setResult(loginResult, 'err', '请求失败，请检查网络或访问口令');
      })
      .then(function () { loginButton.disabled = false; });
  }

  if (loginButton) {
    loginButton.addEventListener('click', doLogin);

    // 这三个输入框没有 name，不在保存表单里；在这里吃掉回车，
    // 免得用户按回车意外触发"保存设置"。
    ['upstream_username', 'upstream_password', 'upstream_otp'].forEach(function (id) {
      var el = document.getElementById(id);
      if (!el) return;
      el.addEventListener('keydown', function (event) {
        if (event.key === 'Enter') {
          event.preventDefault();
          doLogin();
        }
      });
    });
  }
})();
