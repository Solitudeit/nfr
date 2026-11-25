// NFR console — progressive enhancement only. The panel is fully usable without
// JavaScript: this script adds theme switching, copy buttons, password reveal,
// tunnel filtering, join-code countdowns, and 4s live status polling.
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
    return root.getAttribute("data-theme") || "dark";
  }
  function setTheme(next) {
    if (!reduce()) root.classList.add("theme-anim");
    root.setAttribute("data-theme", next);
    try { localStorage.setItem("nfr-theme", next); } catch (e) {}
    clearTimeout(themeTimer);
    themeTimer = setTimeout(function () { root.classList.remove("theme-anim"); }, 260);
  }

  document.addEventListener("touchstart", function () {}, { passive: true });

  // ── delegated clicks ───────────────────────────────────────────────
  var checkSVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="m5 12.5 4.5 4.5L19 7.5"/></svg>';

  document.addEventListener("click", function (ev) {
    var t = ev.target.closest("[data-theme-toggle]");
    if (t) { setTheme(currentTheme() === "light" ? "dark" : "light"); return; }

    var pw = ev.target.closest("[data-pw-toggle]");
    if (pw) {
      var input = document.getElementById(pw.getAttribute("data-pw-toggle"));
      if (input) {
        var reveal = input.type === "password";
        input.type = reveal ? "text" : "password";
        pw.setAttribute("aria-pressed", reveal ? "true" : "false");
        input.focus();
      }
      return;
    }

    var c = ev.target.closest("[data-copy]");
    if (c) {
      var text = c.getAttribute("data-copy");
      var isIconOnly = c.classList.contains("mini-copy");
      var done = function () {
        if (isIconOnly) {
          if (!c.hasAttribute("data-orig-html")) c.setAttribute("data-orig-html", c.innerHTML);
          c.innerHTML = checkSVG;
          c.classList.add("copied");
          setTimeout(function () {
            c.innerHTML = c.getAttribute("data-orig-html");
            c.classList.remove("copied");
          }, 1400);
          return;
        }
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
    if (x) { dismiss(x.closest(".flash")); return; }

    var et = ev.target.closest("[data-events-toggle]");
    if (et) { toggleEventsDrawer(); return; }

    if (ev.target.closest("[data-events-close]") || ev.target.id === "events-backdrop") {
      closeEventsDrawer();
      return;
    }
  });

  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape") closeEventsDrawer();
  });

  document.addEventListener("submit", function (ev) {
    var msg = ev.target.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) {
      ev.preventDefault();
      return;
    }
    if (ev.target.hasAttribute("data-keep-scroll")) {
      try { sessionStorage.setItem("nfr-scroll-y", String(window.scrollY || 0)); } catch (e) {}
    }
  });

  // Restore scroll position after quick toggle/delete reloads.
  try {
    var sy = sessionStorage.getItem("nfr-scroll-y");
    if (sy !== null) {
      sessionStorage.removeItem("nfr-scroll-y");
      var y = parseInt(sy, 10);
      if (!isNaN(y) && y > 0) window.scrollTo(0, y);
    }
  } catch (e) {}

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

  // ── flash notices (auto-dismiss page-level flashes only) ───────────
  function dismiss(el) {
    if (!el || el.hasAttribute("data-leaving")) return;
    el.setAttribute("data-leaving", "");
    setTimeout(function () { el.remove(); }, reduce() ? 100 : 200);
  }

  Array.prototype.forEach.call(document.querySelectorAll(".content > .flash"), function (el) {
    setTimeout(function () { dismiss(el); }, 6000);
  });

  // ── tunnel quick filter & search ───────────────────────────────────
  var filterBar = document.querySelector("[data-tunnel-filter]");
  if (filterBar) {
    var protoBtns = Array.prototype.slice.call(filterBar.querySelectorAll("[data-filter-proto]"));
    var qInput = filterBar.querySelector("[data-filter-q]");
    var cards = Array.prototype.slice.call(document.querySelectorAll(".tunnel-grid .tcard"));
    var emptyEl = document.querySelector("[data-filter-empty]");
    var activeProto = "all";

    function applyFilter() {
      var q = (qInput ? qInput.value : "").trim().toLowerCase();
      var shown = 0;
      cards.forEach(function (card) {
        var proto = (card.getAttribute("data-proto") || "").toLowerCase();
        var hay = (card.getAttribute("data-search") || "").toLowerCase();
        var ok = (activeProto === "all" || proto === activeProto) && (!q || hay.indexOf(q) !== -1);
        card.hidden = !ok;
        if (ok) shown++;
      });
      if (emptyEl) emptyEl.hidden = shown > 0;
    }

    protoBtns.forEach(function (btn) {
      btn.addEventListener("click", function () {
        activeProto = btn.getAttribute("data-filter-proto") || "all";
        protoBtns.forEach(function (b) { b.classList.toggle("active", b === btn); });
        applyFilter();
      });
    });
    if (qInput) qInput.addEventListener("input", applyFilter);
  }

  // ── live state polling ─────────────────────────────────────────────
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

  // Track initial awaiting tunnels and rendered nodes so the page refreshes
  // automatically the moment a newly joined device connects (no manual F5).
  var awaitingOnPage = {};
  Array.prototype.forEach.call(document.querySelectorAll('[data-tid][data-awaiting="1"]'), function (el) {
    awaitingOnPage[el.getAttribute("data-tid")] = true;
  });
  var initialNodeCount = document.querySelectorAll("[data-nid]").length;
  var hasJoinBanner = !!document.querySelector(".join-banner");
  var reloading = false;

  function apply(state) {
    var tunnels = state.tunnels || [];
    var nodes = state.nodes || [];

    if (!reloading) {
      // If a user is looking at a freshly generated join banner on /nfr/nodes
      // and a new node registers, reload so the new node card appears immediately.
      if (hasJoinBanner && nodes.length > initialNodeCount) {
        reloading = true;
        window.location.href = "/nfr/nodes";
        return;
      }
      // If an awaiting tunnel on /nfr/tunnels just got claimed by a joining device,
      // reload so the join box disappears and the live tunnel card takes over.
      for (var i = 0; i < tunnels.length; i++) {
        if (awaitingOnPage[tunnels[i].id] && !tunnels[i].awaiting) {
          reloading = true;
          window.location.reload();
          return;
        }
      }
    }

    var onlineNodes = nodes.filter(function (n) { return n.online; }).length;
    setText(document.querySelector('[data-stat="nodes-online"]'), onlineNodes + " / " + nodes.length);

    var enabled = tunnels.filter(function (t) { return t.enabled; }).length;
    setText(document.querySelector('[data-stat="tunnels-enabled"]'), enabled + " / " + tunnels.length);

    var tin = 0, tout = 0, rin = 0, rout = 0;
    tunnels.forEach(function (t) {
      tin += t.in || 0;
      tout += t.out || 0;
      rin += t.in_rate || 0;
      rout += t.out_rate || 0;

      var row = document.querySelector('[data-tid="' + t.id + '"]');
      if (row) {
        row.setAttribute("data-kind", t.status_kind || "off");
        var dot = row.querySelector(".dot");
        if (dot) dot.className = "dot " + (t.status_kind || "off");
        setText(row.querySelector("[data-live-rate]"), fmtRate(t.in_rate || 0) + " / " + fmtRate(t.out_rate || 0));
        var chip = row.querySelector("[data-live-chip]");
        if (chip && t.status) {
          chip.textContent = t.status;
          chip.className = "chip " + (t.status_kind || "off");
        }
        var meter = row.querySelector("[data-live-meter]");
        if (meter && t.quota_mb > 0) {
          var p = Math.min(100, Math.round((t.used_bytes || 0) / (t.quota_mb * 1048576) * 100));
          var bar = meter.querySelector("i");
          if (bar) bar.style.width = p + "%";
          var lv = p >= 85 ? "lv3" : p >= 60 ? "lv2" : p >= 30 ? "lv1" : "lv0";
          meter.className = "meter " + lv;
          setText(row.querySelector("[data-live-pct]"), p + "%");
        }
      }
    });
    setText(document.querySelector('[data-stat="total-in"]'), fmtBytes(tin));
    setText(document.querySelector('[data-stat="total-out"]'), fmtBytes(tout));
    setText(document.querySelector('[data-stat="rate-in"]'), "实时 " + fmtRate(rin));
    setText(document.querySelector('[data-stat="rate-out"]'), "实时 " + fmtRate(rout));

    if (Array.isArray(state.events)) renderEvents(state.events);

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
  }

  function rel(ts) {
    if (!ts) return "从未";
    var d = (Date.now() - new Date(ts).getTime()) / 1000;
    if (d < 60) return Math.round(d) + " 秒前";
    if (d < 3600) return Math.round(d / 60) + " 分钟前";
    if (d < 86400) return Math.round(d / 3600) + " 小时前";
    return Math.round(d / 86400) + " 天前";
  }

  var live = document.querySelector("[data-live]");
  if (live) {
    var timer = null, stop = false;
    function tick() {
      if (stop) return;
      fetch("/nfr/api/state", { credentials: "same-origin", cache: "no-store" })
        .then(function (res) { return res.ok ? res.json() : null; })
        .then(function (state) { if (state) apply(state); })
        .catch(function () {})
        .then(function () { timer = setTimeout(tick, document.hidden ? 15000 : 4000); });
    }
    tick();
    document.addEventListener("visibilitychange", function () {
      if (!document.hidden && timer) { clearTimeout(timer); tick(); }
    });
    window.addEventListener("beforeunload", function () { stop = true; });
  }

  // ── live activity events drawer ────────────────────────────────────
  var drawerEl = document.getElementById("events-drawer");
  var backdropEl = document.getElementById("events-backdrop");
  var eventsListEl = document.getElementById("events-list");
  var eventDots = Array.prototype.slice.call(document.querySelectorAll("[data-events-dot]"));
  var eventItems = [];
  var lastSeenEventKey = "";

  function eventKey(ev) { return (ev.at || "") + "|" + (ev.type || "") + "|" + (ev.message || ""); }
  function eventKind(t) {
    if (!t) return "idle";
    if (t.indexOf("online") !== -1 || t.indexOf("joined") !== -1 || t.indexOf("bound") !== -1) return "on";
    if (t.indexOf("offline") !== -1 || t.indexOf("down") !== -1 || t.indexOf("err") !== -1) return "err";
    if (t.indexOf("quota") !== -1 || t.indexOf("warn") !== -1) return "warn";
    return "idle";
  }
  function eventLabel(t) {
    var map = {
      "node-online": "节点上线",
      "node-offline": "节点离线",
      "node-joined": "新节点接入",
      "tunnel-bound": "隧道已绑定",
      "tunnel-target-down": "内网目标不可达",
      "change": "状态变更"
    };
    return map[t] || t || "事件";
  }
  function fmtTime(ts) {
    if (!ts) return "";
    var d = new Date(ts);
    if (isNaN(d.getTime())) return "";
    return d.toLocaleTimeString([], { hour12: false });
  }
  function renderEvents(list) {
    if (!eventsListEl) return;
    eventItems = list.slice(-80);
    if (!eventItems.length) {
      eventsListEl.innerHTML = '<div class="events-empty">暂无活动记录</div>';
      return;
    }
    var latest = eventKey(eventItems[eventItems.length - 1]);
    if (drawerEl && drawerEl.hidden && lastSeenEventKey && latest !== lastSeenEventKey) {
      eventDots.forEach(function (d) { d.hidden = false; });
    }
    if (!lastSeenEventKey || !drawerEl || !drawerEl.hidden) lastSeenEventKey = latest;
    var frag = document.createDocumentFragment();
    for (var i = eventItems.length - 1; i >= 0; i--) {
      var ev = eventItems[i];
      var row = document.createElement("div");
      row.className = "ev-row";
      var dot = document.createElement("span");
      dot.className = "dot " + eventKind(ev.type);
      var main = document.createElement("div");
      main.className = "ev-main";
      var head = document.createElement("div");
      head.className = "ev-head";
      var lbl = document.createElement("b");
      lbl.textContent = eventLabel(ev.type);
      var tm = document.createElement("span");
      tm.className = "mono muted";
      tm.textContent = fmtTime(ev.at);
      head.appendChild(lbl);
      head.appendChild(tm);
      var msg = document.createElement("div");
      msg.className = "ev-msg";
      msg.textContent = ev.message || "";
      main.appendChild(head);
      if (ev.message) main.appendChild(msg);
      row.appendChild(dot);
      row.appendChild(main);
      frag.appendChild(row);
    }
    eventsListEl.innerHTML = "";
    eventsListEl.appendChild(frag);
  }
  var drawerReturnFocus = null;
  var drawerCloseTimer = null;
  function openEventsDrawer() {
    if (!drawerEl) return;
    clearTimeout(drawerCloseTimer);
    drawerReturnFocus = document.activeElement;
    drawerEl.hidden = false;
    if (backdropEl) backdropEl.hidden = false;
    requestAnimationFrame(function () {
      drawerEl.classList.add("open");
      if (backdropEl) backdropEl.classList.add("open");
    });
    if (eventItems.length) lastSeenEventKey = eventKey(eventItems[eventItems.length - 1]);
    eventDots.forEach(function (d) { d.hidden = true; });
    document.querySelectorAll("[data-events-toggle]").forEach(function (b) { b.setAttribute("aria-expanded", "true"); });
    var closeBtn = drawerEl.querySelector("[data-events-close]");
    if (closeBtn) closeBtn.focus();
  }
  function closeEventsDrawer() {
    if (!drawerEl || drawerEl.hidden) return;
    drawerEl.classList.remove("open");
    if (backdropEl) backdropEl.classList.remove("open");
    document.querySelectorAll("[data-events-toggle]").forEach(function (b) { b.setAttribute("aria-expanded", "false"); });
    drawerCloseTimer = setTimeout(function () {
      drawerEl.hidden = true;
      if (backdropEl) backdropEl.hidden = true;
      if (drawerReturnFocus && typeof drawerReturnFocus.focus === "function") {
        drawerReturnFocus.focus();
      }
    }, reduce() ? 0 : 155);
  }
  function toggleEventsDrawer() {
    if (!drawerEl) return;
    if (drawerEl.hidden) openEventsDrawer();
    else closeEventsDrawer();
  }

  // Focusable error alert on failed form submissions (WCAG UX guideline)
  var focusErr = document.querySelector("[data-focus-err]");
  if (focusErr) {
    try { focusErr.focus({ preventScroll: false }); } catch (e) {}
  }

  // ── join-code countdown ────────────────────────────────────────────
  function fmtLeft(sec) {
    if (sec <= 0) return "已过期，请重新生成";
    var m = Math.floor(sec / 60);
    return "剩余 " + m + " 分钟";
  }
  var expiries = Array.prototype.slice.call(document.querySelectorAll("[data-expires]"));
  if (expiries.length) {
    function tickExpiries() {
      var now = Math.floor(Date.now() / 1000);
      expiries.forEach(function (el) {
        var at = parseInt(el.getAttribute("data-expires"), 10);
        if (isNaN(at)) return;
        el.textContent = fmtLeft(at - now);
      });
    }
    tickExpiries();
    setInterval(tickExpiries, 30000);
  }
})();
