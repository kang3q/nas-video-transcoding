// Progress arrives over one event stream for every job at once. Opening one
// per job would hit the browser's per-host connection limit and starve the
// video element that shares it.
(function () {
  var fallback = null;

  // Safari and iOS play HLS natively, so hls.js is only fetched where it is
  // actually needed — it is 415KB.
  function loadHls() {
    return new Promise(function (resolve, reject) {
      if (window.Hls) return resolve(window.Hls);
      var s = document.createElement("script");
      s.src = "/static/vendor/hls-1.5.20.min.js";
      s.onload = function () { resolve(window.Hls); };
      s.onerror = reject;
      document.head.appendChild(s);
    });
  }

  // A job still encoding is watched over HLS. The playlist is an EVENT
  // playlist, so it grows as segments land and seeking works anywhere already
  // written; when the encode ends ffmpeg writes the end marker and the player
  // switches to a normal recording on its own.
  document.querySelectorAll("video[data-hls]").forEach(function (video) {
    var url = video.getAttribute("data-hls");
    if (video.canPlayType("application/vnd.apple.mpegurl")) {
      video.src = url;
      return;
    }
    loadHls().then(function (Hls) {
      if (!Hls || !Hls.isSupported()) return;
      var hls = new Hls({
        lowLatencyMode: false,
        backBufferLength: Infinity, // scrub back over everything encoded so far
        manifestLoadingMaxRetry: 8, // the playlist lags the first segments
        levelLoadingMaxRetry: 8,
      });
      hls.loadSource(url);
      hls.attachMedia(video);
      hls.on(Hls.Events.ERROR, function (_, data) {
        // Running out of buffer means the encoder is behind, not that
        // playback has failed. Wait rather than tearing the player down.
        if (data.fatal && data.type !== Hls.ErrorTypes.MEDIA_ERROR) hls.startLoad();
      });
    }).catch(function () {});
  });

  // Count down to the point where playback can run to the end without
  // overtaking the encoder.
  document.querySelectorAll(".j-ready[data-ready]").forEach(function (el) {
    var left = parseFloat(el.getAttribute("data-ready"));
    if (!(left > 0)) return;
    var tick = setInterval(function () {
      left -= 1;
      if (left <= 0) {
        clearInterval(tick);
        el.textContent = "지금부터 끝까지 끊김 없이 볼 수 있습니다.";
        return;
      }
      el.textContent = "약 " + fmtDur(left) + " 뒤부터는 끝까지 볼 수 있습니다.";
    }, 1000);
  });

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
