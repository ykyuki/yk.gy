// yk.gy front-end. No innerHTML is used anywhere: all server data is written
// with textContent / value, so it can never be interpreted as HTML (XSS).
(function () {
  "use strict";

  var form = document.getElementById("shorten-form");
  var input = document.getElementById("url-input");
  var submitBtn = document.getElementById("submit-btn");
  var errorEl = document.getElementById("error");
  var result = document.getElementById("result");
  var shortUrl = document.getElementById("short-url");
  var originalUrl = document.getElementById("original-url");
  var copyBtn = document.getElementById("copy-btn");
  var copyStatus = document.getElementById("copy-status");

  function showError(msg) {
    errorEl.textContent = msg;
    errorEl.hidden = false;
  }

  function clearError() {
    errorEl.textContent = "";
    errorEl.hidden = true;
  }

  function validateLocally(value) {
    if (!value) return "URLを入力してください";
    if (value.length > 2048) return "URLが長すぎます（最大2048文字）";
    var u;
    try { u = new URL(value); } catch (e) { return "URLの形式が正しくありません"; }
    if (u.protocol !== "http:" && u.protocol !== "https:") {
      return "http:// または https:// で始まるURLのみ登録できます";
    }
    return "";
  }

  form.addEventListener("submit", function (ev) {
    ev.preventDefault();
    clearError();
    copyStatus.textContent = "";
    var value = input.value.trim();
    var localErr = validateLocally(value);
    if (localErr) { showError(localErr); input.focus(); return; }

    submitBtn.disabled = true;
    submitBtn.textContent = "確認中…";

    fetch("/api/shorten", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ url: value }),
      credentials: "same-origin",
      cache: "no-store"
    })
      .then(function (res) {
        return res.json().catch(function () { return {}; }).then(function (body) {
          return { ok: res.ok, status: res.status, body: body };
        });
      })
      .then(function (r) {
        if (!r.ok) {
          var msg = (r.body && r.body.error && r.body.error.message) ||
            "エラーが発生しました（HTTP " + r.status + "）";
          showError(msg);
          result.hidden = true;
          return;
        }
        shortUrl.value = r.body.short_url;
        originalUrl.textContent = r.body.url;
        result.hidden = false;
        shortUrl.focus();
        shortUrl.select();
      })
      .catch(function () {
        showError("通信エラーが発生しました。接続を確認して再度お試しください");
      })
      .finally(function () {
        submitBtn.disabled = false;
        submitBtn.textContent = "短縮する";
      });
  });

  copyBtn.addEventListener("click", function () {
    var text = shortUrl.value;
    if (!text) return;
    var done = function () { copyStatus.textContent = "コピーしました"; };
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(done, function () { fallbackCopy(text, done); });
    } else {
      fallbackCopy(text, done);
    }
  });

  function fallbackCopy(text, done) {
    shortUrl.focus();
    shortUrl.select();
    try {
      if (document.execCommand("copy")) { done(); return; }
    } catch (e) { /* ignore */ }
    copyStatus.textContent = "コピーできませんでした。手動で選択してコピーしてください";
  }

  // Show a small badge outside production so environments are never confused.
  fetch("/healthz", { cache: "no-store" })
    .then(function (r) { return r.json(); })
    .then(function (h) {
      if (h && h.environment && h.environment !== "production") {
        var badge = document.getElementById("env-badge");
        badge.textContent = "環境: " + h.environment + (h.safebrowsing_mode === "mock" ? "（Safe Browsing: モック）" : "");
        badge.hidden = false;
      }
    })
    .catch(function () { /* ignore */ });
})();
