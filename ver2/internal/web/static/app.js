// Progress arrives over one event stream for every job at once. Opening one
// per job would hit the browser's per-host connection limit and starve the
// video element that shares it.
(function () {
  var fallback = null;

  function fmtPct(p) { return p < 0 ? "—" : Math.round(p * 100) + "%"; }
  function fmtRate(r) { return r > 0 ? r.toFixed(2) + "x" : "—"; }

  function fmtDur(sec) {
    if (sec == null || sec < 0 || !isFinite(sec)) return "—";
    var s = Math.round(sec);
    if (s < 60) return s + "초";
    if (s < 3600) return Math.floor(s / 60) + "분 " + (s % 60) + "초";
    return Math.floor(s / 3600) + "시간 " + Math.floor((s % 3600) / 60) + "분";
  }

  function paint(job) {
    document.querySelectorAll('[data-job="' + job.id + '"]').forEach(function (el) {
      var bar = el.querySelector(".bar > span");
      if (bar) bar.style.width = Math.max(0, Math.min(job.percent, 1)) * 100 + "%";

      var pct = el.querySelector(".j-pct");
      if (pct) pct.textContent = fmtPct(job.percent);
      else if (el.classList.contains("tag")) el.textContent = fmtPct(job.percent);

      var rate = el.querySelector(".j-rate");
      if (rate) rate.textContent = fmtRate(job.rate);

      var eta = el.querySelector(".j-eta");
      if (eta) eta.textContent = "남은 시간 " + fmtDur(job.eta_sec);

      var state = el.querySelector(".state");
      if (state) state.textContent = job.state;

      if (el.classList.contains("job")) {
        el.className = el.className.replace(/\b(queued|running|done|failed|canceled)\b/g, "");
        el.classList.add("job", job.state);
      }
    });
  }

  function badge(jobs) {
    var el = document.getElementById("queue-badge");
    if (!el) return;
    var busy = jobs.filter(function (j) { return j.state === "running" || j.state === "queued"; }).length;
    el.textContent = busy;
    el.hidden = busy === 0;
  }

  // A job finishing changes what the page should offer — a play link where
  // there was a progress bar — and that is more than a text swap.
  function reloadOn(job) {
    if (job.state === "done" || job.state === "failed") {
      var mine = document.querySelector('[data-job="' + job.id + '"]');
      if (mine) setTimeout(function () { location.reload(); }, 800);
    }
  }

  function startPolling() {
    if (fallback) return;
    fallback = setInterval(function () {
      fetch("/api/jobs").then(function (r) { return r.json(); }).then(function (d) {
        badge(d.jobs); d.jobs.forEach(paint);
      }).catch(function () {});
    }, 2000);
  }

  if (!window.EventSource) { startPolling(); return; }

  var es = new EventSource("/api/events");
  var failures = 0;

  es.addEventListener("snapshot", function (e) {
    var d = JSON.parse(e.data);
    badge(d.jobs);
    d.jobs.forEach(paint);
  });
  es.addEventListener("progress", function (e) { paint(JSON.parse(e.data).job); });
  es.addEventListener("state", function (e) {
    var job = JSON.parse(e.data).job;
    paint(job);
    reloadOn(job);
    fetch("/api/jobs").then(function (r) { return r.json(); }).then(function (d) { badge(d.jobs); }).catch(function () {});
  });
  es.onerror = function () {
    // EventSource reconnects by itself; only give up after it keeps failing.
    if (++failures > 5) { es.close(); startPolling(); }
  };
})();
