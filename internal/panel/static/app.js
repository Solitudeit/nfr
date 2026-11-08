// NFR console — progressive enhancement only. The panel is fully usable without
// JavaScript: this file adds theme switching, copy buttons, live status polling,
// a traffic chart and small motion touches (press feedback, materialize-in).
(function () {
  "use strict";

  var root = document.documentElement;
  var reduceQuery = window.matchMedia ? window.matchMedia("(prefers-reduced-motion: reduce)") : null;
  function reduce() { return !!(reduceQuery && reduceQuery.matches); }

  // ── theme ──────────────────────────────────────────────────────────
  var saved = null;
  try { saved = localStorage.getItem("nfr-theme"); } catch (e) {}
  if (saved === "light" || saved === "dark") root.setAttribute("data-theme", saved);

  var themeTimer = null;
  function currentTheme() {
    return root.getAttribute("data-theme") ||
      (window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark");
  }
  function setTheme(next) {
    if (!reduce()) root.classList.add("theme-anim");
    root.setAttribute("data-theme", next);
    try { localStorage.setItem("nfr-theme", next); } catch (e) {}
    clearTimeout(themeTimer);
    themeTimer = setTimeout(function () { root.classList.remove("theme-anim"); }, 320);
  }

  // iOS only paints :active if a touch listener exists somewhere.
  document.addEventListener("touchstart", function () {}, { passive: true });

  // ── delegated clicks ───────────────────────────────────────────────
  document.addEventListener("click", function (ev) {
    var t = ev.target.closest("[data-theme-toggle]");
    if (t) { setTheme(currentTheme() === "light" ? "dark" : "light"); return; }

    var c = ev.target.closest("[data-copy]");
    if (c) {
      var text = c.getAttribute("data-copy");
      var done = function () {
        var old = c.getAttribute("data-label") || c.textContent;
        if (!c.hasAttribute("data-label")) c.setAttribute("data-label", old);
        c.textContent = "已复制";
        c.classList.add("copied");
        setTimeout(function () {
          c.textContent = c.getAttribute("data-label");
          c.classList.remove("copied");
        }, 1400);
      };
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(done, function () { fallbackCopy(text); done(); });
      } else {
        fallbackCopy(text);
        done();
      }
      return;
    }

    var x = ev.target.closest("[data-dismiss]");
    if (x) { dismiss(x.closest(".flash"), 0); }
  });

  document.addEventListener("submit", function (ev) {
    var msg = ev.target.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) ev.preventDefault();
  });

  function fallbackCopy(text) {
    var ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand("copy"); } catch (e) {}
    document.body.removeChild(ta);
  }

  // ── flash: materialize in, fade out along the same path ────────────
  function dismiss(el, delay) {
    if (!el || el.hasAttribute("data-leaving")) return;
    el.setAttribute("data-leaving", "");
    setTimeout(function () { el.remove(); }, reduce() ? 120 : 200);
  }

  Array.prototype.forEach.call(document.querySelectorAll(".flash"), function (el) {
    setTimeout(function () { dismiss(el); }, 6000);
  });

  // ── live state ─────────────────────────────────────────────────────
  var chart = document.querySelector("[data-chart]");
  var MAX = 48;
  var target = { in: [], out: [] };
  var shown = { in: [], out: [] };
  var anim = null;

  function fmtBytes(n) {
    n = Number(n) || 0;
    if (n < 1024) return n + " B";
    var units = ["KB", "MB", "GB", "TB", "PB"];
    var i = -1;
    do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
    return (n >= 100 ? n.toFixed(0) : n.toFixed(n >= 10 ? 1 : 2)) + " " + units[i];
  }
  function fmtRate(n) { return fmtBytes(n) + "/s"; }
  function setText(el, v) { if (el && el.textContent !== v) el.textContent = v; }

  function apply(state) {
    var tunnels = state.tunnels || [];
    var nodes = state.nodes || [];

    var onlineNodes = nodes.filter(function (n) { return n.online; }).length;
    setText(document.querySelector('[data-stat="nodes-online"]'), onlineNodes + " / " + nodes.length);

    var enabled = tunnels.filter(function (t) { return t.enabled; }).length;
    setText(document.querySelector('[data-stat="tunnels-enabled"]'), enabled + " / " + tunnels.length);

    var tin = 0, tout = 0;
    tunnels.forEach(function (t) {
      tin += t.in || 0;
      tout += t.out || 0;

      var row = document.querySelector('[data-tid="' + t.id + '"]');
      if (row) {
        var dot = row.querySelector(".dot");
        if (dot) {
          dot.classList.toggle("on", !!(t.enabled && t.online));
          dot.classList.toggle("err", !!(t.enabled && !t.online));
          dot.classList.toggle("off", !t.enabled);
        }
        setText(row.querySelector("[data-live-rate]"), fmtRate(t.in_rate || 0) + " / " + fmtRate(t.out_rate || 0));
        var chip = row.querySelector("[data-live-chip]");
        if (chip) {
          if (!t.enabled) { chip.textContent = "已停用"; chip.className = "chip off"; }
          else if (t.online) { chip.textContent = "在线"; chip.className = "chip on"; }
          else { chip.textContent = "节点离线"; chip.className = "chip err"; }
        }
      }
    });
    setText(document.querySelector('[data-stat="total-in"]'), fmtBytes(tin));
    setText(document.querySelector('[data-stat="total-out"]'), fmtBytes(tout));

    nodes.forEach(function (n) {
      var row = document.querySelector('[data-nid="' + n.id + '"]');
      if (!row) return;
      var dot = row.querySelector(".dot");
      if (dot) {
        dot.classList.toggle("on", !!n.online);
        dot.classList.toggle("off", !n.online);
      }
      var chip = row.querySelector("[data-live-chip]");
      if (chip) {
        chip.textContent = n.online ? "在线" : "离线";
        chip.className = "chip " + (n.online ? "on" : "off");
      }
      setText(row.querySelector("[data-live-seen]"), n.online ? "刚刚" : rel(n.last_seen));
    });

    if (chart) {
      var tinRate = tunnels.reduce(function (a, t) { return a + (t.in_rate || 0); }, 0);
      var toutRate = tunnels.reduce(function (a, t) { return a + (t.out_rate || 0); }, 0);
      pushSample(tinRate, toutRate);
    }
  }

  function rel(ts) {
    if (!ts) return "从未";
    var d = (Date.now() - new Date(ts).getTime()) / 1000;
    if (d < 60) return Math.round(d) + " 秒前";
    if (d < 3600) return Math.round(d / 60) + " 分钟前";
    if (d < 86400) return Math.round(d / 3600) + " 小时前";
    return Math.round(d / 86400) + " 天前";
  }

  function pushSample(tin, tout) {
    target.in.push(tin); target.out.push(tout);
    // Start the new point from the current on-screen value so the curve never jumps.
    shown.in.push(shown.in.length ? shown.in[shown.in.length - 1] : tin);
    shown.out.push(shown.out.length ? shown.out[shown.out.length - 1] : tout);
    while (target.in.length > MAX) {
      target.in.shift(); target.out.shift();
      shown.in.shift(); shown.out.shift();
    }
    if (reduce()) {
      shown.in = target.in.slice();
      shown.out = target.out.slice();
      drawChart();
      return;
    }
    animateChart();
  }

  function animateChart() {
    if (anim) cancelAnimationFrame(anim);
    var fromIn = shown.in.slice(), fromOut = shown.out.slice();
    var toIn = target.in.slice(), toOut = target.out.slice();
    var t0 = performance.now(), dur = 260;
    function frame(now) {
      var k = Math.min(1, (now - t0) / dur);
      var e = 1 - Math.pow(1 - k, 3);
      for (var i = 0; i < toIn.length; i++) {
        shown.in[i] = fromIn[i] + (toIn[i] - fromIn[i]) * e;
        shown.out[i] = fromOut[i] + (toOut[i] - fromOut[i]) * e;
      }
      drawChart();
      if (k < 1) anim = requestAnimationFrame(frame); else anim = null;
    }
    anim = requestAnimationFrame(frame);
  }

  var NS = "http://www.w3.org/2000/svg";
  function el(name, attrs) {
    var e = document.createElementNS(NS, name);
    for (var k in attrs) e.setAttribute(k, attrs[k]);
    return e;
  }

  function drawChart() {
    var w = 300, h = 96, padT = 10, padB = 8;
    var all = shown.in.concat(shown.out);
    var max = Math.max.apply(null, all.length ? all : [0]);
    if (!max || max <= 0) {
      if (!chart.querySelector(".chart-empty")) {
        chart.innerHTML = '<div class="chart-empty">正在采样…</div>';
      }
      return;
    }
    var n = Math.max(shown.in.length, 2);
    function path(arr, close) {
      var d = "";
      for (var i = 0; i < arr.length; i++) {
        var x = (i / (n - 1)) * w;
        var y = h - padB - (arr[i] / max) * (h - padT - padB);
        d += (i === 0 ? "M" : "L") + x.toFixed(2) + " " + y.toFixed(2);
      }
      if (close) d += " L" + w + " " + h + " L0 " + h + " Z";
      return d;
    }
    var svg = el("svg", { viewBox: "0 0 " + w + " " + h, preserveAspectRatio: "none", "aria-hidden": "true" });
    var defs = el("defs", {});
    var grad = el("linearGradient", { id: "nfr-fill", x1: 0, y1: 0, x2: 0, y2: 1 });
    grad.appendChild(el("stop", { offset: "0%", "stop-color": "var(--brand)", "stop-opacity": "0.14" }));
    grad.appendChild(el("stop", { offset: "100%", "stop-color": "var(--brand)", "stop-opacity": "0" }));
    defs.appendChild(grad);
    svg.appendChild(defs);
    [0.25, 0.5, 0.75].forEach(function (f) {
      var y = (h - padB) - f * (h - padT - padB);
      svg.appendChild(el("line", { class: "grid", x1: 0, y1: y.toFixed(1), x2: w, y2: y.toFixed(1) }));
    });
    svg.appendChild(el("line", { class: "axis", x1: 0, y1: h - padB, x2: w, y2: h - padB }));
    if (shown.in.length > 1) {
      svg.appendChild(el("path", { class: "area-in", d: path(shown.in, true) }));
      if (shown.out.some(function (v) { return v > 0; })) {
        svg.appendChild(el("path", { class: "line-out", d: path(shown.out, false), "vector-effect": "non-scaling-stroke" }));
      }
      svg.appendChild(el("path", { class: "line-in", d: path(shown.in, false), "vector-effect": "non-scaling-stroke" }));
    } else {
      var lastY = h - padB - (shown.in[0] / max) * (h - padT - padB);
      svg.appendChild(el("circle", { class: "dot-in", cx: w, cy: lastY.toFixed(1), r: 1.6, "vector-effect": "non-scaling-stroke" }));
    }
    chart.innerHTML = "";
    chart.appendChild(svg);
    setText(document.querySelector("[data-peak]"), "最近几分钟 · 峰值 " + fmtRate(max));
  }

  var live = document.querySelector("[data-live]");
  if (live) {
    var timer = null, stop = false;
    function tick() {
      if (stop) return;
      fetch("/_nfr/api/state", { credentials: "same-origin", cache: "no-store" })
        .then(function (res) { return res.ok ? res.json() : null; })
        .then(function (state) { if (state) apply(state); })
        .catch(function () { /* offline for a moment; the next tick will retry */ })
        .then(function () { timer = setTimeout(tick, document.hidden ? 15000 : 4000); });
    }
    tick();
    document.addEventListener("visibilitychange", function () {
      if (!document.hidden && timer) { clearTimeout(timer); tick(); }
    });
    window.addEventListener("beforeunload", function () { stop = true; });
  }
})();
