/* ============================================================================
 * user-center 管理界面脚本
 *
 * 约束与取舍：
 *   - 零依赖、无内联脚本（CSP: default-src 'none'; script-src 'self'）；
 *   - 会话令牌只存在于本页内存变量中，随 Authorization 头发送 ⇒ 没有环境凭据，
 *     因此没有 CSRF 面，也就不用 cookie；
 *   - 所有渲染走 textContent，绝不把接口数据拼进 innerHTML；
 *   - 破坏性动作走 <dialog>（有取消、说明后果），不使用 window.confirm/prompt；
 *   - 成功与失败用不同的通道：内联 status 与 toast 都区分语义，不借用错误位报喜；
 *   - 异步期间按钮进入 aria-busy + disabled，避免重复提交。
 * ========================================================================== */
(function () {
  'use strict';

  var api = document.body.getAttribute('data-api') || '';
  var token = ''; // 仅内存，刷新即失效
  var adminEmail = '';

  function el(id) { return document.getElementById(id); }
  // 模板用 hidden 属性控制显隐（样式表里 [hidden]{display:none!important} 保证
  // 作者样式盖不掉它）。这里统一走属性，避免"加了 class 却仍然显示"的静默失效。
  function show(node) { node.hidden = false; }
  function hide(node) { node.hidden = true; }

  /* ---------- 反馈通道 ---------- */

  var toastTimer = null;
  function toast(message, kind) {
    var box = el('toast');
    // 语义不同 → 样式不同：ok / err / info
    box.className = 'toast toast--' + (kind || 'info');
    el('toast-text').textContent = message;
    show(box);
    if (toastTimer) clearTimeout(toastTimer);
    // 成功/提示自动收起；错误保留，但**始终**可以用关闭按钮收起（键盘可达）
    if (kind !== 'err') {
      toastTimer = setTimeout(hideToast, 4000);
    }
  }
  function hideToast() {
    if (toastTimer) { clearTimeout(toastTimer); toastTimer = null; }
    hide(el('toast'));
  }
  el('toast-close').addEventListener('click', hideToast);

  // 表单旁的内联状态：不打断操作，也带语义
  function status(node, message, kind) {
    node.textContent = message || '';
    node.className = 'status' + (kind ? ' status--' + kind : '');
  }

  /* ---------- 请求 ---------- */

  function request(method, path, body) {
    var headers = { 'Content-Type': 'application/json' };
    if (token) headers.Authorization = 'Bearer ' + token;
    return fetch(api + path, {
      method: method,
      headers: headers,
      body: body === undefined ? undefined : JSON.stringify(body)
    }).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (data) {
        return { status: res.status, data: data, headers: res.headers };
      });
    });
  }

  function messageOf(res, fallback) {
    return (res.data && res.data.msg) || fallback;
  }

  // 异步期间禁用按钮并标记 busy，避免重复点击
  function busy(button, on) {
    if (!button) return;
    button.disabled = !!on;
    if (on) { button.setAttribute('aria-busy', 'true'); }
    else { button.removeAttribute('aria-busy'); }
  }

  /* ---------- 对话框（替代 confirm / prompt） ---------- */

  function ask(opts) {
    return new Promise(function (resolve) {
      var dlg = el('ask');
      var input = el('ask-input');
      var field = el('ask-field');
      var confirmBtn = el('ask-confirm');
      var cancelBtn = el('ask-cancel');
      var prevFocus = document.activeElement;

      el('ask-title').textContent = opts.title || '';
      el('ask-body').textContent = opts.body || '';
      confirmBtn.textContent = opts.confirmLabel || '确定';
      confirmBtn.className = 'btn ' + (opts.danger ? 'btn--danger' : 'btn--primary');
      cancelBtn.textContent = opts.cancelLabel || '取消';

      var wantsInput = typeof opts.label === 'string';
      if (wantsInput) {
        el('ask-label').textContent = opts.label;
        input.value = opts.value || '';
        input.placeholder = opts.placeholder || '';
        show(field);
      } else {
        hide(field);
        input.value = '';
      }

      function finish(value) {
        confirmBtn.removeEventListener('click', onConfirm);
        cancelBtn.removeEventListener('click', onCancel);
        input.removeEventListener('keydown', onKey);
        dlg.removeEventListener('close', onClose);
        if (dlg.open) dlg.close();
        if (prevFocus && typeof prevFocus.focus === 'function') prevFocus.focus();
        resolve(value);
      }
      function onConfirm() { finish(wantsInput ? input.value.trim() : true); }
      function onCancel() { finish(null); }
      function onClose() { finish(null); }          // Esc / 点击遮罩
      function onKey(e) { if (e.key === 'Enter') { e.preventDefault(); onConfirm(); } }

      confirmBtn.addEventListener('click', onConfirm);
      cancelBtn.addEventListener('click', onCancel);
      input.addEventListener('keydown', onKey);
      dlg.addEventListener('close', onClose);
      dlg.showModal();
      if (wantsInput) { input.focus(); input.select(); } else { confirmBtn.focus(); }
    });
  }

  /* ---------- 登录 ---------- */

  el('login-form').addEventListener('submit', function (e) { e.preventDefault(); sendCode(); });
  el('login-send').addEventListener('click', sendCode);
  el('login-verify').addEventListener('click', verifyCode);
  el('login-code').addEventListener('keydown', function (e) {
    if (e.key === 'Enter') { e.preventDefault(); verifyCode(); }
  });

  function sendCode() {
    var email = el('login-email').value.trim();
    if (!email) {
      status(el('login-status'), '请先填写管理员邮箱', 'err');
      el('login-email').focus();
      return;
    }
    var btn = el('login-send');
    if (cooldownTimer) return;            // 冷却中：按钮本身就是禁用的，这里再兜一次
    busy(btn, true);
    btn.textContent = '发送中…';
    status(el('login-status'), '正在发送…');
    request('POST', '/otp/send', { email: email }).then(function (res) {
      busy(btn, false);
      btn.textContent = '获取验证码';
      if (res.status === 200) {
        adminEmail = email;
        show(el('login-code-wrap'));
        status(el('login-status'), '验证码 5 分钟内有效。如果该邮箱是管理员邮箱，它已经发出。');
        startCooldown(60);
        el('login-code').focus();
        return;
      }
      if (res.status === 429) {
        var wait = parseInt(res.headers && res.headers.get && res.headers.get('Retry-After'), 10);
        status(el('login-status'), '请求过于频繁，请稍后再试', 'err');
        if (!isNaN(wait) && wait > 0) startCooldown(Math.min(wait, 3600));
        return;
      }
      status(el('login-status'), messageOf(res, '发送失败'), 'err');
    }).catch(function () {
      busy(btn, false);
      btn.textContent = '获取验证码';
      status(el('login-status'), '网络异常，请检查本机服务是否在运行', 'err');
    });
  }

  // startCooldown 接管"获取验证码"按钮：显示剩余秒数并在期间禁用。
  // 服务端在冷却期内不会重发（返回 200 但复用同一个码），按钮必须如实反映这件事。
  var cooldownTimer = null;
  function startCooldown(seconds) {
    var button = el('login-send');
    var left = Math.max(1, seconds | 0);
    if (cooldownTimer) clearInterval(cooldownTimer);
    button.disabled = true;
    button.textContent = left + ' 秒后可重发';
    cooldownTimer = setInterval(function () {
      left -= 1;
      if (left <= 0) {
        clearInterval(cooldownTimer);
        cooldownTimer = null;
        button.disabled = false;
        button.textContent = '获取验证码';
        return;
      }
      button.textContent = left + ' 秒后可重发';
    }, 1000);
  }

  function verifyCode() {
    var code = el('login-code').value.trim();
    if (!code) { el('login-code').focus(); return; }
    var btn = el('login-verify');
    busy(btn, true);
    status(el('login-status'), '正在校验…');
    request('POST', '/otp/verify', { email: adminEmail || el('login-email').value.trim(), code: code })
      .then(function (res) {
        busy(btn, false);
        if (res.status === 200 && res.data.code === 0) {
          token = (res.data.data && res.data.data.token) || '';
          enterApp();
          return;
        }
        status(el('login-status'), messageOf(res, '邮箱或验证码不正确'), 'err');
        el('login-code').select();
      }).catch(function () {
        busy(btn, false);
        status(el('login-status'), '网络异常', 'err');
      });
  }

  function enterApp() {
    hide(el('login'));
    show(el('app'));
    el('admin-who').textContent = adminEmail || '';
    refreshAll();
    el('new-email').focus();     // 登录后的第一件事就是建用户
    toast('已登录', 'ok');
  }

  el('sign-out').addEventListener('click', function () {
    request('POST', '/logout').then(function () {
      token = '';
      window.location.reload();
    });
  });

  /* ---------- 数据 ---------- */

  // refreshAll 会重渲染整个名单，因此要把"刚才点了哪个按钮"记下来，
  // 渲染完成后把焦点还回去——否则键盘用户每做一次操作就得从头 Tab。
  function refreshAll(focus) {
    if (focus) pendingFocus = { user: String(focus.user), action: focus.action };
    loadStats();
    loadUsers();
    loadAudit();
  }

  function loadStats() {
    el('stats-updated').textContent = '更新中…';
    request('GET', '/stats').then(function (res) {
      if (res.status !== 200 || res.data.code !== 0) return;
      var d = res.data.data || {};
      var users = d.users_by_status || {};
      var q = d.queue || {};

      var box = el('stats');
      box.textContent = '';
      [
        ['已激活', users.active || 0],
        ['待激活', users.invited || 0],
        ['已停用', users.disabled || 0],
        ['管理员', d.admins || 0],
        ['近 24 小时发信', d.mails_last_24h || 0]
      ].forEach(function (pair) {
        var wrap = document.createElement('div');
        wrap.className = 'stat';
        var value = document.createElement('span');
        value.className = 'stat__value';
        value.textContent = String(pair[1]);
        var label = document.createElement('span');
        label.className = 'stat__label';
        label.textContent = pair[0];
        wrap.appendChild(value);
        wrap.appendChild(label);
        box.appendChild(wrap);
      });

      var chip = el('queue-chip');
      var pending = (q.pending || 0);
      var failed = (q.failed || 0);
      chip.textContent = failed > 0
        ? ('待发 ' + pending + ' · 失败 ' + failed)
        : ('待发邮件 ' + pending);
      chip.className = 'chip ' + (failed > 0 ? 'chip--disabled' : (pending > 0 ? 'chip--invited' : 'chip--active'));
      show(chip);

      var now = new Date();
      el('stats-updated').textContent = '更新于 ' + pad(now.getHours()) + ':' + pad(now.getMinutes());
    });
  }

  var STATUS_LABEL = { invited: '待激活', active: '已激活', disabled: '已停用' };

  function loadUsers() {
    var container = el('users');
    container.setAttribute('aria-busy', 'true');   // 读屏器会等取数完成再朗读
    request('GET', '/users').then(function (res) {
      if (res.status === 401) { toast('登录状态已失效，请刷新页面重新登录', 'err'); return; }
      if (res.status !== 200 || res.data.code !== 0) {
        toast(messageOf(res, '加载用户失败'), 'err');
        return;
      }
      var list = (res.data.data && res.data.data.users) || [];
      var box = el('users');
      box.removeAttribute('aria-busy');
      box.textContent = '';
      el('users-count').textContent = list.length ? (list.length + ' 位') : '';

      if (!list.length) {
        box.appendChild(emptyState('还没有用户', '用上面的表单邀请第一位——对方点开链接设置用户名后即可登录。'));
        return;
      }

      var table = document.createElement('table');
      table.className = 'table';
      var thead = document.createElement('thead');
      var headRow = document.createElement('tr');
      [['用户', ''], ['用户名', ''], ['状态', ''], ['最近登录', ''], ['操作', 'col-actions']].forEach(function (col) {
        var th = document.createElement('th');
        th.textContent = col[0];
        if (col[1]) th.className = col[1];
        headRow.appendChild(th);
      });
      thead.appendChild(headRow);
      table.appendChild(thead);

      var tbody = document.createElement('tbody');
      list.forEach(function (u) { tbody.appendChild(userRow(u)); });
      table.appendChild(tbody);
      box.appendChild(table);
      restoreFocus();
    }).catch(function () {
      container.removeAttribute('aria-busy');
    });
  }

  function userRow(u) {
    var tr = document.createElement('tr');

    // 第一格：邮箱 + 状态徽标（徽标带文字，不靠颜色单独表达）
    var tdUser = document.createElement('td');
    var cell = document.createElement('div');
    cell.className = 'cell-user';
    var email = document.createElement('span');
    email.className = 'cell-user__email';
    email.textContent = u.email;
    cell.appendChild(email);
    cell.appendChild(chip(STATUS_LABEL[u.status] || u.status, u.status));
    if (u.is_admin) cell.appendChild(chip('管理员', 'admin'));
    tdUser.appendChild(cell);
    tr.appendChild(tdUser);

    // 第二格：用户名
    var tdUid = document.createElement('td');
    tdUid.className = 'mono';
    tdUid.textContent = u.uid ? ('@' + u.uid) : '未设置';
    if (!u.uid) tdUid.classList.add('muted');
    tr.appendChild(tdUid);

    // 第三格：状态（窄屏会自动收起第四列）
    var tdStatus = document.createElement('td');
    tdStatus.className = 'small muted';
    tdStatus.textContent = u.is_admin ? '管理员' : '普通用户';
    tr.appendChild(tdStatus);

    // 第四格：最近登录
    var tdLast = document.createElement('td');
    tdLast.className = 'num';
    tdLast.textContent = u.last_login_at ? shortTime(u.last_login_at) : '从未登录';
    tr.appendChild(tdLast);

    var actions = document.createElement('div');
    actions.className = 'row-actions';

    // 每个按钮都带上对象邮箱作为无障碍名：读屏用户才知道自己在对谁操作
    function action(label, onClick, opts) {
      opts = opts || {};
      opts.user = u.id;
      opts.ariaLabel = label + '：' + u.email;
      return btn(label, onClick, opts);
    }

    // 只要还没设置用户名就可以重发（管理员也是一样：它同样是"未命名"用户）
    if (!u.uid && u.status !== 'disabled') {
      actions.appendChild(action(u.status === 'invited' ? '重发邀请' : '重发设置链接', function () {
        withBusy(this, function (done) {
          request('POST', '/users/' + u.id + '/invite/resend').then(function (res) {
            done();
            if (res.status === 200 && res.data.code === 0) {
              var d = res.data.data || {};
              showReveal(d.invite_url, '已重发邀请 · 旧链接立即失效');
              toast('邀请邮件已重新发送给 ' + u.email, 'ok');
              refreshAll({ user: u.id, action: 'resend' });
            } else {
              toast(messageOf(res, '重发失败'), 'err');
            }
          }).catch(function () { done(); toast('网络异常', 'err'); });
        });
      }, { action: 'resend' }));

      actions.appendChild(action('重新生成链接', function () {
        // 这一步会让对方邮件里的旧链接失效，先说清后果
        ask({
          title: '重新生成邀请链接？',
          body: '对方邮件里的旧链接会立即失效。仅在你需要人工把链接发给对方时使用。',
          confirmLabel: '重新生成'
        }).then(function (ok) {
          if (!ok) return;
          request('POST', '/users/' + u.id + '/invite/link').then(function (res) {
            if (res.status === 200 && res.data.code === 0) {
              showReveal((res.data.data || {}).invite_url, '重新生成的链接 · 旧链接已失效');
              refreshAll({ user: u.id, action: 'regenerate' });
            } else {
              toast(messageOf(res, '生成失败'), 'err');
            }
          });
        });
      }, { action: 'regenerate' }));
    }

    actions.appendChild(action('踢下线', function () {
      ask({
        title: '踢出该用户的全部会话？',
        body: u.email + ' 当前的所有登录状态会立即失效，需要重新用验证码登录。',
        confirmLabel: '踢下线'
      }).then(function (ok) {
        if (!ok) return;
        request('POST', '/users/' + u.id + '/sessions/revoke').then(function (res) {
          if (res.status === 200 && res.data.code === 0) {
            var n = (res.data.data || {}).revoked || 0;
            toast('已吊销 ' + n + ' 个会话', 'ok');
            refreshAll({ user: u.id, action: 'revoke' });
          } else {
            toast(messageOf(res, '操作失败'), 'err');
          }
        });
      });
    }, { action: 'revoke' }));

    if (u.status === 'disabled') {
      actions.appendChild(action('启用', function () {
        request('POST', '/users/' + u.id + '/enable').then(function (res) {
          if (res.status === 200) {
            toast(u.email + ' 已启用', 'ok');
            refreshAll({ user: u.id, action: 'toggle' });
          } else {
            toast(messageOf(res, '启用失败'), 'err');
          }
        });
      }, { action: 'toggle' }));
    } else {
      actions.appendChild(action('停用', function () {
        ask({
          title: '停用该用户？',
          body: u.email + ' 将无法再登录，其全部会话会立即失效。之后可以再启用。',
          confirmLabel: '停用',
          danger: true
        }).then(function (ok) {
          if (!ok) return;
          request('POST', '/users/' + u.id + '/disable').then(function (res) {
            if (res.status === 200) {
              toast(u.email + ' 已停用', 'ok');
              refreshAll({ user: u.id, action: 'toggle' });
            } else {
              toast(messageOf(res, '停用失败'), 'err');
            }
          });
        });
      }, { action: 'toggle', danger: true }));
    }

    actions.appendChild(action('改邮箱', function () {
      ask({
        title: '修改登录邮箱',
        body: '立即生效，并吊销该用户全部会话；系统会同时通知旧地址。请仔细核对，写错会导致对方无法登录。',
        label: '新的登录邮箱',
        value: u.email,
        confirmLabel: '修改'
      }).then(function (next) {
        if (!next || next === u.email) return;
        request('POST', '/users/' + u.id + '/email', { email: next }).then(function (res) {
          if (res.status === 200 && res.data.code === 0) {
            toast('登录邮箱已改为 ' + next, 'ok');
            refreshAll({ user: u.id, action: 'email' });
          } else {
            toast(messageOf(res, '修改失败'), 'err');
          }
        });
      });
    }, { action: 'email' }));

    actions.appendChild(action('删除', function () {
      ask({
        title: '删除该用户？',
        body: u.email + ' 将被永久删除，无法撤销。若只是想暂时阻止登录，请改用「停用」。',
        confirmLabel: '永久删除',
        danger: true
      }).then(function (ok) {
        if (!ok) return;
        request('DELETE', '/users/' + u.id).then(function (res) {
          if (res.status === 200) {
            toast(u.email + ' 已删除', 'ok');
            refreshAll({ user: u.id, action: 'delete' });   // 行已消失 → 焦点落到面板标题
          } else {
            toast(messageOf(res, '删除失败'), 'err');
          }
        });
      });
    }, { action: 'delete', danger: true }));

    var tdActions = document.createElement('td');
    tdActions.className = 'col-actions';
    tdActions.appendChild(actions);
    tr.appendChild(tdActions);
    return tr;
  }


  function loadAudit() {
    var container = el('audit');
    container.setAttribute('aria-busy', 'true');
    request('GET', '/audit').then(function (res) {
      container.removeAttribute('aria-busy');
      if (res.status !== 200 || res.data.code !== 0) return;
      var list = (res.data.data && res.data.data.entries) || [];
      var box = el('audit');
      box.textContent = '';

      if (!list.length) {
        box.appendChild(emptyState('暂无记录', '你在这里的每一次操作都会留下可追溯的记录。'));
        return;
      }

      var table = document.createElement('table');
      table.className = 'table';
      var thead = document.createElement('thead');
      var headRow = document.createElement('tr');
      ['时间', '操作', '来源'].forEach(function (label) {
        var th = document.createElement('th');
        th.textContent = label;
        headRow.appendChild(th);
      });
      thead.appendChild(headRow);
      table.appendChild(thead);

      var tbody = document.createElement('tbody');
      list.slice(0, 50).forEach(function (e) {
        var tr = document.createElement('tr');

        var tdTime = document.createElement('td');
        tdTime.className = 'num';
        tdTime.textContent = shortTime(e.created_at);
        tr.appendChild(tdTime);

        var tdWhat = document.createElement('td');
        var actor = document.createElement('strong');
        actor.textContent = e.actor_email || '系统';
        tdWhat.appendChild(actor);
        tdWhat.appendChild(document.createTextNode(' · ' + actionLabel(e.action) +
          (e.target ? (' · ' + e.target) : '')));
        tr.appendChild(tdWhat);

        var tdIp = document.createElement('td');
        tdIp.className = 'mono muted';
        tdIp.textContent = e.ip || '';
        tr.appendChild(tdIp);

        tbody.appendChild(tr);
      });
      table.appendChild(tbody);
      box.appendChild(table);
    });
  }

  var ACTION_LABEL = {
    user_created: '创建用户', invite_sent: '发出邀请', invite_resent: '重发邀请',
    invite_link_regenerated: '重新生成链接', user_activated: '激活账号',
    user_disabled: '停用用户', user_enabled: '启用用户', email_changed: '修改邮箱',
    sessions_revoked: '踢出会话', user_deleted: '删除用户',
    admin_uid_invite_sent: '向管理员发送设置用户名链接',
    admin_login: '管理员登录', admin_login_failed: '管理员登录失败', admin_logout: '管理员登出'
  };
  function actionLabel(action) { return ACTION_LABEL[action] || action; }

  /* ---------- 创建用户 + 一次性链接揭示 ---------- */

  el('create-form').addEventListener('submit', function (e) {
    e.preventDefault();
    var email = el('new-email').value.trim();
    if (!email) { el('new-email').focus(); return; }

    var button = el('new-create');
    busy(button, true);
    status(el('create-status'), '正在创建…');
    hide(el('reveal'));

    request('POST', '/users', { email: email }).then(function (res) {
      busy(button, false);
      if (res.status === 200 && res.data.code === 0) {
        var d = res.data.data || {};
        el('new-email').value = '';
        var mailed = d.mail_queued !== false;
        status(el('create-status'),
          mailed
            ? '邀请已发送给 ' + email + '（有效期至 ' + shortTime(d.expires_at) + '）'
            : '用户已创建，但邮件未入队（未配置发信）：请复制下面的链接人工送达',
          mailed ? 'ok' : undefined);
        if (d.invite_url) showReveal(d.invite_url, mailed ? '邀请链接（备用）' : '请人工送达此链接');
        if (mailed) toast('邀请邮件已入队', 'ok');
        refreshAll();
        return;
      }
      status(el('create-status'), messageOf(res, '创建失败'), 'err');
      el('new-email').focus();
    }).catch(function () {
      busy(button, false);
      status(el('create-status'), '网络异常', 'err');
    });
  });

  function showReveal(url, badge) {
    if (!url) return;
    el('reveal-url').textContent = url;
    el('reveal-badge').textContent = badge || '一次性链接';
    show(el('reveal'));
    // 不抢焦点（用户可能正在输入），但让读屏用户听到
    status(el('create-status'), '一次性链接已生成，请在关闭页面前复制', 'ok');
  }

  el('reveal-copy').addEventListener('click', function () {
    var url = el('reveal-url').textContent;
    if (!url) return;
    var button = this;

    function copied() {
      button.textContent = '已复制';
      setTimeout(function () { button.textContent = '复制'; }, 1600);
      toast('链接已复制到剪贴板', 'ok');
    }
    function fallback() {
      // 剪贴板 API 不可用时：选中文本，让用户自己按快捷键
      var range = document.createRange();
      range.selectNodeContents(el('reveal-url'));
      var sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(range);
      toast('已选中链接，请按 ⌘C / Ctrl+C 复制', 'info');
    }

    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(url).then(copied).catch(fallback);
    } else {
      fallback();
    }
  });

  /* ---------- 小工具 ---------- */

  // btn 生成行内操作按钮。
  //
  // ariaLabel 必不可少：一行里有 6 个同名按钮，读屏用户如果只听到"踢下线"，
  // 根本不知道是对谁操作（WCAG 2.4.6 / 4.1.2）。
  // data-user / data-action 供重渲染后把焦点还回原处使用。
  function btn(label, onClick, opts) {
    opts = opts || {};
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'btn btn--sm' + (opts.danger ? ' btn--danger' : '');
    b.textContent = label;
    if (opts.ariaLabel) b.setAttribute('aria-label', opts.ariaLabel);
    if (opts.user) b.setAttribute('data-user', String(opts.user));
    if (opts.action) b.setAttribute('data-action', opts.action);
    b.addEventListener('click', onClick);
    return b;
  }

  /* ---------- 焦点：重渲染后把焦点还给用户 ---------- */

  var pendingFocus = null;   // { user, action }

  // 优先回到同一个按钮；该按钮不存在了（例如"停用"变成"启用"）就退到同一行的
  // 第一个操作按钮；连整行都没了（删除）则落到面板标题——关键是焦点**不能掉回 body**。
  function restoreFocus() {
    if (!pendingFocus) return;
    var pending = pendingFocus;
    pendingFocus = null;

    var scope = el('users');
    var exact = scope.querySelector(
      '[data-user="' + pending.user + '"][data-action="' + pending.action + '"]');
    if (exact) { exact.focus(); return; }

    var sameRow = scope.querySelector('[data-user="' + pending.user + '"]');
    if (sameRow) { sameRow.focus(); return; }

    var heading = el('users-title');
    heading.setAttribute('tabindex', '-1');
    heading.focus();
  }

  function withBusy(button, run) {
    var original = button.textContent;
    button.disabled = true;
    button.setAttribute('aria-busy', 'true');
    run(function done() {
      button.disabled = false;
      button.removeAttribute('aria-busy');
      button.textContent = original;
    });
  }

  // 空状态：一句"现在没有" + 一句"下一步做什么"
  function emptyState(title, desc) {
    var box = document.createElement('div');
    box.className = 'empty';
    var t = document.createElement('p');
    t.className = 'empty__title';
    t.textContent = title;
    var d = document.createElement('p');
    d.className = 'empty__desc';
    d.textContent = desc;
    box.appendChild(t);
    box.appendChild(d);
    return box;
  }

  function chip(text, kind) {
    var span = document.createElement('span');
    span.className = 'chip chip--' + kind;
    span.textContent = text;   // 状态永远带文字，不靠颜色单独表达
    return span;
  }

  function pad(n) { return (n < 10 ? '0' : '') + n; }

  function shortTime(iso) {
    if (!iso) return '';
    var d = new Date(iso);
    if (isNaN(d.getTime())) return String(iso);
    return pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes());
  }
})();
