/* =========================================================================
   Avdb 搜索 · 设置页交互
   只做一件事：保存之前先探测一下地址与令牌能不能用。
   表单本身是原生提交的，禁用 JS 也能正常保存。
   ========================================================================= */

(function () {
  'use strict';

  var form = document.getElementById('settings-form');
  var button = document.getElementById('btn-test');
  var result = document.getElementById('test-result');
  if (!form || !button || !result) return;

  function valueOf(id) {
    var el = document.getElementById(id);
    return el ? el.value : '';
  }

  function setResult(kind, text) {
    result.className = 'test-result' + (kind ? ' is-' + kind : '');
    result.textContent = text || '';
  }

  button.addEventListener('click', function () {
    button.disabled = true;
    setResult('loading', '正在测试…');

    // 只送测试必需的字段。API Key 留空时服务端会沿用已保存的那个，
    // 所以用户不必为了测试而重新粘贴令牌。
    var payload = new URLSearchParams();
    payload.set('api_base_url', valueOf('api_base_url'));
    payload.set('api_key', valueOf('api_key'));
    payload.set('timeout_seconds', valueOf('timeout_seconds'));

    fetch('/api/settings/test', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/x-www-form-urlencoded;charset=UTF-8',
        'Accept': 'application/json'
      },
      credentials: 'same-origin',
      body: payload.toString()
    })
      .then(function (response) { return response.json(); })
      .then(function (data) {
        if (data && data.success) {
          setResult('ok', data.message || '连接正常');
        } else {
          setResult('err', (data && data.message) || '连接失败');
        }
      })
      .catch(function () {
        setResult('err', '测试请求失败，请检查网络或访问口令');
      })
      .then(function () {
        button.disabled = false;
      });
  });
})();
