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

.card-head { display: flex; justify-content: space-between; align-items: flex-start; gap: 12px; flex-wrap: wrap; margin-bottom: 12px; }
.card-head h2 { margin-bottom: 2px; }
.card-sub { margin: 0; font-size: 13px; color: var(--text-secondary); max-width: 52ch; }

.alias-hint { display: flex; align-items: flex-start; gap: 8px; margin: 0 0 14px; padding: 8px 12px; border-radius: 6px; background: var(--bg-tertiary); color: var(--text-secondary); font-size: 12.5px; }
.alias-hint svg { flex-shrink: 0; margin-top: 4px; color: var(--text-tertiary); }

.alias-row { display: flex; align-items: flex-end; gap: 10px; padding: 12px 0; border-bottom: 1px solid var(--border-color); }
.alias-index { flex-shrink: 0; padding-bottom: 9px; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 12.5px; color: var(--text-tertiary); }
.alias-field { display: flex; flex-direction: column; flex: 1 1 140px; min-width: 0; }
.alias-field-name { flex: 1.4 1 170px; }
.alias-field > label { margin: 0 0 4px; }
.alias-arrow { flex-shrink: 0; padding-bottom: 9px; color: var(--text-tertiary); }
.alias-keep { display: flex; flex-direction: column; flex-shrink: 0; }
.alias-keep > label { margin: 0 0 6px; }
.icon-btn { width: 34px; height: 34px; padding: 0; display: inline-flex; align-items: center; justify-content: center; flex-shrink: 0; color: var(--text-secondary); }
.icon-btn:hover { color: var(--warning-text); border-color: var(--warning-border); }

.switch { position: relative; display: inline-block; width: 34px; height: 20px; flex-shrink: 0; }
.switch input { position: absolute; top: 0; left: 0; width: 100%; height: 100%; margin: 0; padding: 0; border: none; border-radius: 999px; background: transparent; opacity: 0; cursor: pointer; }
.switch-track { position: absolute; top: 0; left: 0; width: 100%; height: 100%; border-radius: 999px; background: var(--bg-tertiary); border: 1px solid var(--border-primary); pointer-events: none; transition: background .15s var(--ease-out-strong), border-color .15s var(--ease-out-strong); }
.switch-track::after { content: ""; position: absolute; top: 2px; left: 2px; width: 14px; height: 14px; border-radius: 50%; background: var(--primary-contrast); transition: transform .15s var(--ease-out-strong); }
.switch input:checked + .switch-track { background: var(--primary-color); border-color: var(--primary-color); }
.switch input:checked + .switch-track::after { transform: translateX(14px); }
.switch input:focus-visible + .switch-track { outline: 2px solid var(--primary-color); outline-offset: 2px; }

#aliases-card button:focus-visible { outline: 2px solid var(--primary-color); outline-offset: 2px; }
.alias-row input:focus-visible { outline: 2px solid var(--primary-color); outline-offset: 2px; }
.alias-row input[aria-invalid="true"] { border-color: var(--warning-color); }

.alias-actions { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; margin-top: 14px; }
.alias-actions .status { margin-top: 0; }

@media (max-width: 639px) {
  .alias-row { flex-wrap: wrap; align-items: flex-start; row-gap: 4px; }
  .alias-arrow { display: none; }
  .alias-index { order: 1; padding-bottom: 0; }
  .alias-keep { order: 2; flex: 1 1 auto; }
  .alias-keep > label { margin-bottom: 2px; }
  .alias-row .icon-btn { order: 3; }
  .alias-field { order: 4; flex: 1 1 100%; }
}

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

  <section class="card" id="aliases-card">
    <div class="card-head">
      <div>
        <h2>模型别名</h2>
        <p class="card-sub">选择原模型，并填写客户端使用的别名。</p>
      </div>
      <button type="button" id="alias-add">+ 添加别名</button>
    </div>
    <p class="alias-hint">
      <svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true">
        <circle cx="8" cy="8" r="6.75" fill="none" stroke="currentColor" stroke-width="1.5"></circle>
        <path d="M8 7.2v4" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"></path>
        <circle cx="8" cy="4.9" r="0.95" fill="currentColor"></circle>
      </svg>
      <span id="alias-hint-text"></span>
    </p>
    <div class="empty" id="alias-load-error" hidden></div>
    <div id="alias-rows"></div>
    <div class="empty" id="alias-empty" hidden>还没有别名。点“添加别名”新增一行。</div>
    <datalist id="alias-models"></datalist>
    <div class="alias-actions">
      <button type="button" class="primary" id="alias-save" disabled>保存别名</button>
      <span class="status" id="alias-status" aria-live="polite"></span>
    </div>
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

  // ---- aliases --------------------------------------------------------

  var aliasDirty = false;
  var aliasSaving = false;
  var aliasLoading = false;

  function trashIcon() {
    var NS = "http://www.w3.org/2000/svg";
    var svg = document.createElementNS(NS, "svg");
    svg.setAttribute("viewBox", "0 0 16 16");
    svg.setAttribute("width", "15");
    svg.setAttribute("height", "15");
    svg.setAttribute("aria-hidden", "true");
    var p1 = document.createElementNS(NS, "path");
    p1.setAttribute("d", "M2.5 4h11M6.5 4V2.6h3V4M4.3 4l.6 9h6.2l.6-9");
    p1.setAttribute("fill", "none");
    p1.setAttribute("stroke", "currentColor");
    p1.setAttribute("stroke-width", "1.4");
    p1.setAttribute("stroke-linecap", "round");
    p1.setAttribute("stroke-linejoin", "round");
    var p2 = document.createElementNS(NS, "path");
    p2.setAttribute("d", "M6.6 6.8v3.6M9.4 6.8v3.6");
    p2.setAttribute("fill", "none");
    p2.setAttribute("stroke", "currentColor");
    p2.setAttribute("stroke-width", "1.4");
    p2.setAttribute("stroke-linecap", "round");
    svg.appendChild(p1);
    svg.appendChild(p2);
    return svg;
  }

  function markAliasDirty() {
    aliasDirty = true;
    updateAliasStatus();
  }

  function updateAliasStatus() {
    var btn = document.getElementById("alias-save");
    var errEl = document.getElementById("alias-load-error");
    if (errEl && !errEl.hidden) {
      btn.disabled = true;
      setStatus("alias-status", "");
      return;
    }
    if (aliasSaving || aliasLoading) { btn.disabled = true; return; }
    btn.disabled = !aliasDirty;
    if (aliasDirty) {
      var st = document.getElementById("alias-status");
      if (!st.classList.contains("err")) setStatus("alias-status", "有未保存的修改");
    }
  }

  function updateAliasEmpty() {
    var rows = document.getElementById("alias-rows");
    var errEl = document.getElementById("alias-load-error");
    document.getElementById("alias-empty").hidden =
      rows.children.length > 0 || (errEl && !errEl.hidden);
  }

  function renumberAliases() {
    var rows = document.querySelectorAll("#alias-rows .alias-row");
    Array.prototype.forEach.call(rows, function (row, i) {
      row.querySelector(".alias-index").textContent = (i + 1 < 10 ? "0" : "") + (i + 1);
      var btn = row.querySelector(".icon-btn");
      if (btn) btn.setAttribute("aria-label", "删除第 " + (i + 1) + " 行");
    });
    updateAliasEmpty();
  }

  function fieldLabel(text, input) {
    var label = document.createElement("label");
    label.textContent = text;
    var suffix = "-a" + Math.random().toString(36).slice(2, 8);
    input.id = input.id + suffix;
    label.setAttribute("for", input.id);
    return label;
  }

  function addAliasRow(entry, focus) {
    var row = document.createElement("div");
    row.className = "alias-row";

    var index = document.createElement("span");
    index.className = "alias-index";
    row.appendChild(index);

    var nameWrap = document.createElement("div");
    nameWrap.className = "alias-field alias-field-name";
    var nameInput = document.createElement("input");
    nameInput.className = "alias-name";
    nameInput.setAttribute("list", "alias-models");
    nameInput.setAttribute("autocomplete", "off");
    nameInput.setAttribute("spellcheck", "false");
    nameInput.value = (entry && entry.name) || "";
    nameWrap.appendChild(fieldLabel("原模型名称", nameInput));
    nameWrap.appendChild(nameInput);
    row.appendChild(nameWrap);

    var arrow = document.createElement("span");
    arrow.className = "alias-arrow";
    arrow.setAttribute("aria-hidden", "true");
    arrow.textContent = "→";
    row.appendChild(arrow);

    var aliasWrap = document.createElement("div");
    aliasWrap.className = "alias-field";
    var aliasInput = document.createElement("input");
    aliasInput.className = "alias-alias";
    aliasInput.setAttribute("autocomplete", "off");
    aliasInput.setAttribute("spellcheck", "false");
    aliasInput.value = (entry && entry.alias) || "";
    aliasWrap.appendChild(fieldLabel("别名（必填）", aliasInput));
    aliasWrap.appendChild(aliasInput);
    row.appendChild(aliasWrap);

    var keepWrap = document.createElement("div");
    keepWrap.className = "alias-keep";
    var keepInput = document.createElement("input");
    keepInput.type = "checkbox";
    keepInput.className = "alias-fork";
    keepInput.setAttribute("role", "switch");
    keepInput.checked = !entry || entry.fork !== false;
    var keepLabel = fieldLabel("保留原名", keepInput);
    var switchEl = document.createElement("span");
    switchEl.className = "switch";
    var track = document.createElement("span");
    track.className = "switch-track";
    track.setAttribute("aria-hidden", "true");
    switchEl.appendChild(keepInput);
    switchEl.appendChild(track);
    keepWrap.appendChild(keepLabel);
    keepWrap.appendChild(switchEl);
    row.appendChild(keepWrap);

    var delBtn = document.createElement("button");
    delBtn.type = "button";
    delBtn.className = "icon-btn";
    delBtn.appendChild(trashIcon());
    delBtn.addEventListener("click", function () {
      row.remove();
      renumberAliases();
      markAliasDirty();
    });
    row.appendChild(delBtn);

    nameInput.addEventListener("input", function () {
      nameInput.removeAttribute("aria-invalid");
      markAliasDirty();
    });
    aliasInput.addEventListener("input", function () {
      aliasInput.removeAttribute("aria-invalid");
      markAliasDirty();
    });
    keepInput.addEventListener("change", markAliasDirty);

    document.getElementById("alias-rows").appendChild(row);
    renumberAliases();
    if (focus) nameInput.focus();
  }

  function aliasHintText(count) {
    var prefix = typeof count === "number"
      ? "已加载 " + count + " 个模型，可在“原模型名称”中下拉选择，也可手动输入。"
      : "可在“原模型名称”中下拉选择已加载的模型，也可手动输入。";
    return prefix + "开启“保留原名”时原模型名和别名都可用，关闭时只保留别名。别名与其他渠道的模型同名时会合并进同一候选池。";
  }

  function clearAliasInvalid() {
    Array.prototype.forEach.call(
      document.querySelectorAll("#alias-rows input[aria-invalid]"),
      function (input) { input.removeAttribute("aria-invalid"); }
    );
  }

  function markAliasLineError(text) {
    var m = /第\s*(\d+)\s*行/.exec(text || "");
    if (!m) return;
    var n = parseInt(m[1], 10);
    var rows = document.querySelectorAll("#alias-rows .alias-row");
    if (n < 1 || n > rows.length) return;
    Array.prototype.forEach.call(rows[n - 1].querySelectorAll("input"), function (input) {
      input.setAttribute("aria-invalid", "true");
    });
  }

  function collectAliasRows() {
    var rows = [];
    var error = "";
    Array.prototype.forEach.call(document.querySelectorAll("#alias-rows .alias-row"), function (row, i) {
      var name = row.querySelector(".alias-name").value.trim();
      var alias = row.querySelector(".alias-alias").value.trim();
      var fork = row.querySelector(".alias-fork").checked;
      if (name && !alias && !error) {
        error = "第 " + (i + 1) + " 行：别名必填";
        row.querySelector(".alias-alias").setAttribute("aria-invalid", "true");
      }
      // Submit every DOM row, blanks included: the server reports error
      // line numbers as the 1-based index in this array, so it must match
      // the row numbering the user sees.
      rows.push({ name: name, alias: alias, fork: fork });
    });
    return { rows: rows, error: error };
  }

  function saveAliases() {
    clearAliasInvalid();
    var collected = collectAliasRows();
    if (collected.error) {
      setStatus("alias-status", collected.error, "err");
      return;
    }
    var btn = document.getElementById("alias-save");
    aliasSaving = true;
    btn.disabled = true;
    setStatus("alias-status", "保存中…");
    getJSON("/aliases/set?data=" + encodeURIComponent(JSON.stringify(collected.rows))).then(function (resp) {
      if (resp && resp.status === "success") {
        aliasDirty = false;
        setStatus("alias-status", "");
        toast("已保存 " + resp.count + " 条别名，/v1/models 已刷新");
        loadAliases();
      } else {
        var message = (resp && resp.error) || "保存失败";
        setStatus("alias-status", message, "err");
        markAliasLineError(message);
      }
    }).catch(function () {
      setStatus("alias-status", "保存失败", "err");
    }).finally(function () {
      aliasSaving = false;
      updateAliasStatus();
    });
  }

  function loadAliases() {
    var addBtn = document.getElementById("alias-add");
    var host = document.getElementById("alias-rows");
    aliasLoading = true;
    addBtn.disabled = true;
    if (!aliasDirty) {
      host.innerHTML = '<div class="skeleton"><span></span><span></span><span></span></div>';
      updateAliasEmpty();
    }
    updateAliasStatus();
    getJSON("/aliases").then(function (resp) {
      document.getElementById("alias-load-error").hidden = true;
      var list = (resp && resp.aliases) || [];
      var models = (resp && resp.models) || [];
      var datalist = document.getElementById("alias-models");
      datalist.textContent = "";
      models.forEach(function (id) {
        var opt = document.createElement("option");
        opt.value = id;
        datalist.appendChild(opt);
      });
      document.getElementById("alias-hint-text").textContent = aliasHintText(models.length);
      if (aliasDirty) {
        // Keep the user's unsaved rows; only the datalist and the hint
        // text refresh. Rebuilding here would drop their edits.
        return;
      }
      host.textContent = "";
      list.forEach(function (entry) { addAliasRow(entry, false); });
      updateAliasEmpty();
      aliasDirty = false;
      setStatus("alias-status", "");
      updateAliasStatus();
    }).catch(function (err) {
      if (!aliasDirty) {
        host.textContent = "";
      }
      var errEl = document.getElementById("alias-load-error");
      errEl.textContent = "加载失败：" + String(err);
      errEl.hidden = false;
      updateAliasEmpty();
      updateAliasStatus();
    }).finally(function () {
      aliasLoading = false;
      addBtn.disabled = false;
      updateAliasStatus();
    });
  }

  document.getElementById("alias-add").addEventListener("click", function () {
    addAliasRow(null, true);
    markAliasDirty();
  });
  document.getElementById("alias-save").addEventListener("click", saveAliases);

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
    return '<span class="badge ok">已导入</span>';
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
        var sub = entry.name + (entry.machine ? " · machine " + escapeHTML(entry.machine.slice(0, 8) + "…") : "");
        return '<div class="account-row">' +
          '<div class="account-meta">' +
            '<div class="account-name">' + escapeHTML(entry.label) + " " + badge(entry) + "</div>" +
            '<div class="account-sub mono">' + escapeHTML(sub) + "</div>" +
            '<div class="account-sub">' + escapeHTML(entry.token || "") + (entry.updated ? " · 更新于 " + escapeHTML(entry.updated) : "") + "</div>" +
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
  loadAliases();
})();
</script>
</body>
</html>
`
