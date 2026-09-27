// Admin panel helpers. The pages work without this file; it only adds
// comfort: copy buttons, confirmations, local times, auto-refresh.
(function () {
  "use strict";

  // Copy buttons: <button class="copy" data-copy="text">.
  document.addEventListener("click", function (e) {
    var btn = e.target.closest("[data-copy]");
    if (!btn) return;
    e.preventDefault();
    navigator.clipboard.writeText(btn.getAttribute("data-copy")).then(function () {
      var old = btn.textContent;
      btn.textContent = "✓";
      btn.classList.add("done");
      setTimeout(function () { btn.textContent = old; btn.classList.remove("done"); }, 1200);
    });
  });

  // Confirmations: <form data-confirm="Are you sure?">.
  document.addEventListener("submit", function (e) {
    var msg = e.target.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) e.preventDefault();
  });

  // Times: <time datetime="RFC3339"> shows local time; data-ago adds "5 min ago".
  function ago(d) {
    var s = Math.round((Date.now() - d.getTime()) / 1000);
    var future = s < 0;
    s = Math.abs(s);
    var out;
    if (s < 60) out = s + " s";
    else if (s < 3600) out = Math.round(s / 60) + " min";
    else if (s < 86400) out = Math.round(s / 3600) + " h";
    else out = Math.round(s / 86400) + " d";
    return future ? "in " + out : out + " ago";
  }
  function renderTimes() {
    document.querySelectorAll("time[datetime]").forEach(function (t) {
      var d = new Date(t.getAttribute("datetime"));
      if (isNaN(d)) return;
      if (!t.title) t.title = d.toLocaleString();
      t.textContent = t.hasAttribute("data-ago") ? ago(d) : d.toLocaleString([], { dateStyle: "medium", timeStyle: "short" });
    });
  }
  renderTimes();
  setInterval(renderTimes, 30000);

  // Auto-refresh: <body data-refresh="30">, paused while you type or have a dialog open.
  var every = parseInt(document.body.getAttribute("data-refresh") || "0", 10);
  if (every > 0) {
    setInterval(function () {
      var a = document.activeElement;
      var typing = a && (a.tagName === "INPUT" || a.tagName === "TEXTAREA" || a.tagName === "SELECT");
      if (!typing && !document.hidden) window.location.reload();
    }, every * 1000);
  }
})();
