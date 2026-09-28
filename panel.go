package main

// panelHTML is the self-contained management page served at
// /v0/resource/plugins/cpa-cursor/panel. The console embeds it as a
// same-origin iframe, so it reuses the console's design tokens (copied from
// management.html, same as cpa-qoder) and must read as part of the console.
const panelHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Cursor 账号</title>
<style>
:root {
  --bg-primary: #f0eee8;
  --bg-secondary: #faf9f5;
  --bg-tertiary: #e9e6df;
  --text-primary: #2d2a26;
  --text-secondary: #6d6760;
  --text-tertiary: #9c958d;
  --border-color: #e3e1db;
  --border-primary: #d5d2cb;
  --border-hover: #cecac4;
  --primary-color: #8b8680;
  --primary-hover: #7f7a74;
  --primary-contrast: #fff;
  --success-color: #10b981;
  --warning-color: #c65746;
  --warning-bg: #c657461f;
  --warning-border: #c6574659;
  --warning-text: #c65746;
  --ease-out-strong: cubic-bezier(.23, 1, .32, 1);
}
:root[data-theme="dark"] {
  --bg-primary: #1d1b18;
  --bg-secondary: #151412;
  --bg-tertiary: #262320;
  --text-primary: #f6f4f1;
  --text-secondary: #c9c3bb;
  --text-tertiary: #a29c95;
  --border-color: #3a3530;
  --border-primary: #4a453f;
  --border-hover: #5a544d;
  --primary-color: #8b8680;
  --primary-hover: #9a948e;
  --success-color: #10b981;
  --warning-color: #c65746;
  --warning-bg: #c6574638;
  --warning-border: #c6574673;
  --warning-text: #f1b0a6;
  --ease-out-strong: cubic-bezier(.23, 1, .32, 1);
}

* { box-sizing: border-box; }
[hidden] { display: none !important; }

body {
  margin: 0;
  padding: 24px 16px 56px;
  background: var(--bg-primary);
  color: var(--text-primary);
  font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', 'PingFang SC',
    'Hiragino Sans GB', 'Microsoft YaHei', 'Helvetica Neue', Helvetica, Arial, sans-serif;
  font-size: 14px;
  line-height: 1.6;
}

main { max-width: 920px; margin: 0 auto; }

h1 { font-size: 20px; font-weight: 600; margin: 0 0 4px; }
.subtitle { margin: 0 0 24px; color: var(--text-secondary); max-width: 62ch; }

section.card {
  background: var(--bg-secondary);
  border: 1px solid var(--border-color);
  border-radius: 10px;
  padding: 20px;
  margin-bottom: 20px;
}

h2 { font-size: 15px; font-weight: 600; margin: 0 0 12px; }

label { display: block; font-size: 13px; color: var(--text-secondary); margin: 10px 0 4px; }

input, textarea {
  width: 100%;
  padding: 8px 10px;
  border: 1px solid var(--border-primary);
  border-radius: 6px;
  background: var(--bg-primary);
  color: var(--text-primary);
  font: inherit;
  transition: border-color .15s var(--ease-out-strong);
}
textarea { min-height: 72px; resize: vertical; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 12.5px; }
input:focus, textarea:focus { outline: none; border-color: var(--primary-color); }

button {
  padding: 7px 14px;
  border: 1px solid var(--border-primary);
  border-radius: 6px;
  background: var(--bg-tertiary);
  color: var(--text-primary);
  font: inherit;
  font-size: 13px;
  cursor: pointer;
  transition: background .15s var(--ease-out-strong), border-color .15s var(--ease-out-strong);
}
button:hover { border-color: var(--border-hover); }
button.primary { background: var(--primary-color); border-color: var(--primary-color); color: var(--primary-contrast); }
button.primary:hover { background: var(--primary-hover); }
button:disabled { opacity: .55; cursor: not-allowed; }

.actions { display: flex; gap: 8px; margin-top: 14px; flex-wrap: wrap; }
.toolbar { display: flex; gap: 8px; align-items: center; margin-bottom: 10px; flex-wrap: wrap; }
.toolbar .status { margin-left: 4px; }
#toast { position: fixed; left: 50%; bottom: 24px; transform: translateX(-50%); background: var(--bg-elevated, #1f2430); color: var(--text-primary, #e6e9f0); border: 1px solid var(--border, #2a3040); border-radius: 8px; padding: 8px 14px; font-size: 13px; z-index: 50; box-shadow: 0 6px 24px rgba(0,0,0,.25); }

.status { min-height: 20px; margin-top: 8px; font-size: 13px; color: var(--text-secondary); }
.status.ok { color: var(--success-color); }
.status.err { color: var(--warning-text); }

.account-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 12px 14px;
  border: 1px solid var(--border-color);
  border-radius: 8px;
  margin-bottom: 10px;
  background: var(--bg-primary);
  flex-wrap: wrap;
}
.account-meta { display: flex; flex-direction: column; gap: 2px; min-width: 0; flex: 1; }
.account-name { font-weight: 600; }
.account-sub { font-size: 12.5px; color: var(--text-tertiary); font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; overflow-wrap: anywhere; }
.account-actions { display: flex; gap: 8px; flex-shrink: 0; }

.badge {
  display: inline-block; padding: 1px 8px; border-radius: 999px;
  font-size: 12px; border: 1px solid var(--border-color); color: var(--text-secondary);
}
.badge.ok { color: var(--success-color); border-color: color-mix(in srgb, var(--success-color) 40%, transparent); }
.badge.warn { color: var(--warning-text); border-color: var(--warning-border); background: var(--warning-bg); }

.models-table { width: 100%; border-collapse: collapse; font-size: 13px; }
.models-table th, .models-table td { text-align: left; padding: 7px 10px; border-bottom: 1px solid var(--border-color); vertical-align: top; }
.models-table th { color: var(--text-tertiary); font-weight: 500; font-size: 12.5px; }
.models-table td.mono { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 12.5px; }

.empty { color: var(--text-tertiary); padding: 18px 0; text-align: center; }

.skeleton { display: flex; flex-direction: column; gap: 8px; padding: 4px 0; }
.skeleton span { height: 14px; border-radius: 4px; background: linear-gradient(90deg, var(--bg-tertiary) 25%, var(--bg-secondary) 50%, var(--bg-tertiary) 75%); background-size: 200% 100%; animation: shimmer 1.4s infinite; }
.skeleton span:nth-child(2) { width: 75%; }
.skeleton span:nth-child(3) { width: 55%; }
@keyframes shimmer { to { background-position: -200% 0; } }

details.raw { margin-top: 10px; }
details.raw summary { cursor: pointer; color: var(--text-tertiary); font-size: 12.5px; }
details.raw pre { overflow: auto; font-size: 12px; background: var(--bg-primary); border: 1px solid var(--border-color); border-radius: 6px; padding: 10px; }
</style>
</head>
<body>
<main>
  <h1>Cursor 账号</h1>
  <p class="subtitle">导入 Cursor IDE 的 access token，管理账号并查看可用模型。token 从登录过 Cursor 的机器上的 state.vscdb 中提取。</p>

  <section class="card">
    <h2>导入账号</h2>
    <form id="import-form">
      <label for="token">AccessToken（必填，取自 state.vscdb 的 cursorAuth/accessToken）</label>
      <textarea id="token" placeholder="粘贴 accessToken，支持带 :: 前缀的完整值" autocomplete="off"></textarea>
      <label for="machine-id">Machine ID（可选，缺省由 token 派生）</label>
      <input id="machine-id" placeholder="64 位 hex，state.vscdb 的 storage.serviceMachineId" autocomplete="off">
      <label for="email">备注 Email（可选）</label>
      <input id="email" placeholder="选填，仅作备注" autocomplete="off">
      <div class="actions">
        <button type="submit" class="primary" id="import-btn">导入</button>
        <button type="button" id="test-noauth">测试上游连通</button>
      </div>
      <div class="status" id="import-status"></div>
    </form>
  </section>

  <section class="card">
    <h2>已导入账号</h2>
    <div id="accounts" class="skeleton"><span></span><span></span><span></span></div>
    <div class="actions"><button type="button" id="refresh">刷新</button></div>
  </section>

  <section class="card" id="models-card" hidden>
    <h2 id="models-title">可用模型</h2>
    <div id="models-body"></div>
    <details class="raw" id="models-raw" hidden>
      <summary>原始返回</summary>
      <pre id="models-raw-pre"></pre>
    </details>
  </section>
</main>
<script>
(function () {
  var base = window.location.pathname.replace(/\/panel$/, "");
  var selected = null;

  applyTheme();

  function applyTheme() {
    var value = "auto";
    try {
      var raw = window.localStorage.getItem("cli-proxy-theme");
      if (raw) {
        try { value = (JSON.parse(raw).state || {}).theme || raw; } catch (e) { value = raw; }
      }
    } catch (e) { /* private mode: keep the default */ }
    var dark = value === "dark" ||
      (value !== "white" && value !== "light" &&
        window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches);
    document.documentElement.setAttribute("data-theme", dark ? "dark" : "light");
  }

  function escapeHTML(value) {
    return String(value == null ? "" : value).replace(/[&<>"']/g, function (ch) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[ch];
    });
  }

  function setStatus(id, text, kind) {
    var el = document.getElementById(id);
    el.textContent = text || "";
    el.className = "status" + (kind ? " " + kind : "");
  }

  function getJSON(path) {
    return fetch(base + path).then(function (r) { return r.json(); });
  }

  // ---- import ---------------------------------------------------------

  document.getElementById("import-form").addEventListener("submit", function (event) {
    event.preventDefault();
    var token = document.getElementById("token").value.trim();
    var machineId = document.getElementById("machine-id").value.trim();
    var email = document.getElementById("email").value.trim();
    if (!token) { setStatus("import-status", "AccessToken 不能为空", "err"); return; }
    var btn = document.getElementById("import-btn");
    btn.disabled = true;
    setStatus("import-status", "导入中…");
    var qs = "?token=" + encodeURIComponent(token) +
      "&machine_id=" + encodeURIComponent(machineId) +
      "&email=" + encodeURIComponent(email);
    getJSON("/import" + qs).then(function (resp) {
      if (resp.error) {
        setStatus("import-status", "导入失败：" + resp.error, "err");
      } else {
        setStatus("import-status", "已导入 " + resp.label + " → " + resp.file, "ok");
        document.getElementById("token").value = "";
        document.getElementById("machine-id").value = "";
        document.getElementById("email").value = "";
        loadAccounts();
      }
    }).catch(function (err) {
      setStatus("import-status", "请求失败：" + err, "err");
    }).finally(function () { btn.disabled = false; });
  });

  document.getElementById("test-noauth").addEventListener("click", function () {
    var btn = this;
    btn.disabled = true;
    setStatus("import-status", "探测中…");
    getJSON("/test").then(function (resp) {
      setStatus("import-status", resp.message, resp.status === "ok" ? "ok" : "err");
    }).catch(function (err) {
      setStatus("import-status", "请求失败：" + err, "err");
    }).finally(function () { btn.disabled = false; });
  });

  // ---- accounts -------------------------------------------------------

  function badge(entry) {
    if (entry.disabled) return '<span class="badge warn">已禁用</span>';
    if (entry.unavailable) return '<span class="badge warn">不可用</span>';
    if (entry.status === "active" || entry.status === "valid" || !entry.status) return '<span class="badge ok">正常</span>';
    return '<span class="badge">' + escapeHTML(entry.status) + "</span>";
  }

  function loadAccounts() {
    getJSON("/accounts").then(function (resp) {
      var host = document.getElementById("accounts");
      var accounts = resp.accounts || [];
      if (resp.error && !accounts.length) {
        host.className = "";
        host.innerHTML = '<div class="empty">加载失败：' + escapeHTML(resp.error) + "</div>";
        return;
      }
      host.className = "";
      if (!accounts.length) {
        host.innerHTML = '<div class="empty">还没有导入任何 Cursor 账号</div>';
        return;
      }
      host.innerHTML = accounts.map(function (entry) {
        return '<div class="account-row">' +
          '<div class="account-meta">' +
            '<div class="account-name">' + escapeHTML(entry.label) + " " + badge(entry) + "</div>" +
            '<div class="account-sub">' + escapeHTML(entry.name) + "</div>" +
          "</div>" +
          '<div class="account-actions">' +
            '<button type="button" data-auth="' + escapeHTML(entry.name) + '" data-act="models">查看模型</button>' +
            '<button type="button" data-auth="' + escapeHTML(entry.name) + '" data-act="test">测试</button>' +
          "</div>" +
        "</div>";
      }).join("");
      Array.prototype.forEach.call(host.querySelectorAll("button[data-act]"), function (btn) {
        btn.addEventListener("click", function () {
          if (btn.dataset.act === "models") showModels(btn.dataset.auth);
          else runTest(btn.dataset.auth, btn);
        });
      });
    }).catch(function (err) {
      var host = document.getElementById("accounts");
      host.className = "";
      host.innerHTML = '<div class="empty">加载失败：' + escapeHTML(String(err)) + "</div>";
    });
  }

  // ---- models ---------------------------------------------------------

  function showModels(auth) {
    selected = auth;
    var card = document.getElementById("models-card");
    var body = document.getElementById("models-body");
    card.hidden = false;
    document.getElementById("models-title").textContent = "可用模型 — " + auth;
    document.getElementById("models-raw").hidden = true;
    body.innerHTML = '<div class="skeleton"><span></span><span></span><span></span></div>';
    getJSON("/models?auth=" + encodeURIComponent(auth)).then(function (resp) {
      var models = resp.models || [];
      window.__cursorModels = models;
      var rows = models.map(function (m) {
        var id = m.id || m.ID;
        return "<tr>" +
          '<td><input type="checkbox" class="model-check" data-id="' + escapeHTML(id) + '"' + (m.enabled ? " checked" : "") + "></td>" +
          '<td class="mono">' + escapeHTML(id) + "</td>" +
          "<td>" + escapeHTML(m.name || m.display_name || m.DisplayName || "") + "</td>" +
          '<td style="color:var(--text-tertiary)">' + escapeHTML(m.aliases || "") + "</td>" +
        "</tr>";
      });
      var toolbar =
        '<div class="toolbar">' +
          '<button type="button" id="btn-check-all">全选</button>' +
          '<button type="button" id="btn-check-none">取消全选</button>' +
          '<button type="button" id="btn-save-selection" class="primary">保存选择</button>' +
          '<button type="button" id="btn-reset-selection">恢复全部</button>' +
          '<span class="status" id="selection-status"></span>' +
        "</div>";
      body.innerHTML = rows.length
        ? toolbar + '<table class="models-table"><tr><th></th><th>模型 ID</th><th>名称</th><th>别名</th></tr>' + rows.join("") + "</table>" +
          '<div class="status">共 ' + rows.length + " 个模型（来源：" + escapeHTML(resp.source || "static") + "；选择模式：" + escapeHTML(resp.mode || "all") + "）</div>"
        : '<div class="empty">该账号没有返回任何模型</div>';
      function checkedIds() {
        return Array.prototype.filter.call(document.querySelectorAll(".model-check"), function (c) { return c.checked; })
          .map(function (c) { return c.getAttribute("data-id"); });
      }
      function updateCount() {
        document.getElementById("selection-status").textContent = "已选 " + checkedIds().length + " / " + models.length;
      }
      document.getElementById("btn-check-all").onclick = function () {
        Array.prototype.forEach.call(document.querySelectorAll(".model-check"), function (c) { c.checked = true; });
        updateCount();
      };
      document.getElementById("btn-check-none").onclick = function () {
        Array.prototype.forEach.call(document.querySelectorAll(".model-check"), function (c) { c.checked = false; });
        updateCount();
      };
      document.getElementById("btn-save-selection").onclick = function () {
        var btn = this;
        btn.disabled = true;
        getJSON("/models/set?ids=" + encodeURIComponent(checkedIds().join(","))).then(function (resp2) {
          btn.disabled = false;
          updateCount();
          toast(resp2.status === "success" ? "已保存：选中的模型才会出现在 /v1/models" : "保存失败");
        }).catch(function () { btn.disabled = false; toast("保存失败"); });
      };
      document.getElementById("btn-reset-selection").onclick = function () {
        getJSON("/models/set?reset=1").then(function () {
          showModels(selected);
          toast("已恢复全部启用");
        }).catch(function () { toast("恢复失败"); });
      };
      Array.prototype.forEach.call(document.querySelectorAll(".model-check"), function (c) { c.onchange = updateCount; });
      updateCount();
      document.getElementById("models-raw-pre").textContent = JSON.stringify(resp, null, 2);
      document.getElementById("models-raw").hidden = false;
    }).catch(function (err) {
      body.innerHTML = '<div class="empty">加载失败：' + escapeHTML(String(err)) + "</div>";
    });
  }

  var toastTimer = null;
  function toast(message) {
    var el = document.getElementById("toast");
    if (!el) { el = document.createElement("div"); el.id = "toast"; document.body.appendChild(el); }
    el.textContent = message;
    el.hidden = false;
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { el.hidden = true; }, 2600);
  }

  function runTest(auth, btn) {
    btn.disabled = true;
    btn.textContent = "测试中…";
    getJSON("/test?auth=" + encodeURIComponent(auth)).then(function (resp) {
      btn.textContent = resp.status === "ok" ? resp.latency + " ms" : "失败";
      btn.disabled = false;
    }).catch(function () {
      btn.textContent = "失败";
      btn.disabled = false;
    });
  }

  document.getElementById("refresh").addEventListener("click", loadAccounts);

  loadAccounts();
})();
</script>
</body>
</html>
`
