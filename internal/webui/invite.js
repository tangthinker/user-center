/* ============================================================================
 * 邀请落地页脚本：零依赖、无内联、无外部资源。
 *
 * 页面由服务端渲染（GET 只读、不消费 token，因此邮件安全网关的预取不会把
 * 邀请链接烧掉）；本脚本只负责两件事：
 *   1. 输入用户名时就地检查可用性（每 token 限 20 次）；
 *   2. 提交激活，并把成功/失败反馈在页面里（不用 alert）。
 * ========================================================================== */
(function () {
  'use strict';

  var api = document.body.getAttribute('data-api') || '';
  var token = new URLSearchParams(window.location.search).get('token') || '';

  // 读到 token 后立刻从地址栏抹掉，减少历史记录与 Referer 泄漏。
  if (token && window.history.replaceState) {
    window.history.replaceState({}, '', window.location.pathname);
  }

  var form = document.getElementById('form');
  if (!form) return;

  var errorBox = document.getElementById('error');
  var doneBox = document.getElementById('done');
  var uidEl = document.getElementById('uid');
  var stateEl = document.getElementById('uidState');
  var submitBtn = document.getElementById('submit');
  var needsUID = uidEl && !uidEl.hasAttribute('hidden');   // 服务端按情况注入 hidden 属性
  var submitLabel = submitBtn.textContent;   // 由服务端渲染（激活账号 / 保存用户名）

  function showError(message) {
    errorBox.textContent = message;
    errorBox.hidden = false;
  }
  function clearError() { errorBox.hidden = true; }

  function post(path, payload) {
    return fetch(api + path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload)
    }).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (data) {
        return { status: res.status, data: data };
      });
    });
  }

  if (!token) {
    showError('链接不完整，请从邮件中重新打开的链接进入本页。');
    submitBtn.disabled = true;
    return;
  }

  /* ---------- 用户名可用性：输入停止 350ms 后就地检查 ---------- */

  var timer = null;
  if (needsUID) {
    uidEl.addEventListener('input', function () {
      var value = uidEl.value.trim();
      stateEl.className = 'status';
      stateEl.textContent = '';
      uidEl.removeAttribute('aria-invalid');
      if (timer) clearTimeout(timer);
      if (!value) return;

      timer = setTimeout(function () {
        post('/invite/check-uid', { token: token, uid: value }).then(function (res) {
          if (res.status === 200 && res.data.code === 0) {
            var ok = res.data.data && res.data.data.available;
            stateEl.className = 'status ' + (ok ? 'status--ok' : 'status--err');
            stateEl.textContent = ok ? '✓ 可以使用' : '✗ 已被占用，换一个';
            if (!ok) uidEl.setAttribute('aria-invalid', 'true');
            return;
          }
          if (res.status === 400) {
            // 规则不合规是唯一会解释细节的接口
            stateEl.className = 'status status--err';
            stateEl.textContent = (res.data && res.data.msg) || '用户名不合规';
            uidEl.setAttribute('aria-invalid', 'true');
            return;
          }
          if (res.status === 429) {
            stateEl.className = 'status';
            stateEl.textContent = '检查过于频繁，稍后会自动恢复';
            return;
          }
          stateEl.textContent = '';
        });
      }, 350);
    });
  }

  /* ---------- 提交 ---------- */

  form.addEventListener('submit', function (e) {
    e.preventDefault();
    clearError();

    var uid = needsUID ? uidEl.value.trim() : '';
    if (needsUID && !uid) {
      showError('请先填写用户名。');
      uidEl.focus();
      return;
    }

    submitBtn.disabled = true;
    submitBtn.setAttribute('aria-busy', 'true');
    submitBtn.textContent = '处理中…';

    post('/invite/accept', { token: token, uid: uid }).then(function (res) {
      if (res.status === 200 && res.data.code === 0) {
        form.hidden = true;
        doneBox.hidden = false;
        doneBox.focus && doneBox.focus();
        return;
      }
      submitBtn.disabled = false;
      submitBtn.removeAttribute('aria-busy');
      submitBtn.textContent = submitLabel;
      showError((res.data && res.data.msg) || '操作失败，请稍后重试或联系管理员。');
    }).catch(function () {
      submitBtn.disabled = false;
      submitBtn.removeAttribute('aria-busy');
      submitBtn.textContent = submitLabel;
      showError('网络异常，请稍后重试。');
    });
  });
})();
