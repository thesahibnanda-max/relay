// Relay site behaviour. No dependencies. Everything degrades gracefully if it fails.
(function () {
  'use strict';
  var root = document.documentElement;
  var reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)');

  // ---- install command: always point at THIS site's own install script ----
  var unixInstallUrl = new URL('install.sh', document.baseURI).href;
  var windowsInstallUrl = new URL('install.ps1', document.baseURI).href;
  var installCmds = {
    unix: 'curl -fsSL ' + unixInstallUrl + ' | bash',
    windows: 'irm ' + windowsInstallUrl + ' | iex',
  };

  // ---- OS detection: best-effort, only ever picks a friendlier starting
  // point (which command/tab is shown first) - every command is always one
  // click away regardless, so a wrong guess costs nothing.
  var ua = navigator.userAgent || '';
  function detectTabId() {
    if (/Windows/.test(ua)) return 'tab-windows';
    if (/Mac OS X|Macintosh/.test(ua)) return 'tab-mac';
    if (/Linux/.test(ua) && !/Android/.test(ua)) return 'tab-linux';
    return null; // unknown: keep the default (macOS)
  }
  var detectedKind = /Windows/.test(ua) ? 'windows' : 'unix';
  document.querySelectorAll('[data-install-cmd]').forEach(function (el) { el.textContent = installCmds[detectedKind]; });
  var osNames = { 'tab-windows': 'Windows (PowerShell)', 'tab-mac': 'macOS', 'tab-linux': 'Linux' };
  var detectedId = detectTabId();
  document.querySelectorAll('[data-os-label]').forEach(function (el) {
    if (detectedId) el.textContent = 'Detected: ' + osNames[detectedId];
  });

  // ---- theme: System / Light / Dark ----
  var themeColors = { dark: '#0a0714', light: '#f7f4ff' };
  var metas = Array.prototype.slice.call(document.querySelectorAll('meta[name="theme-color"]'));
  var metaDefaults = metas.map(function (m) { return m.getAttribute('content'); });
  var themeBtns = Array.prototype.slice.call(document.querySelectorAll('[data-theme-set]'));
  function currentMode() {
    var t = root.getAttribute('data-theme');
    return t === 'light' || t === 'dark' ? t : 'system';
  }
  function applyMode(mode) {
    if (mode === 'light' || mode === 'dark') root.setAttribute('data-theme', mode);
    else root.removeAttribute('data-theme');
    try { localStorage.setItem('relay-theme', mode); } catch (e) { /* ignore */ }
    metas.forEach(function (m, i) { m.setAttribute('content', mode === 'system' ? metaDefaults[i] : themeColors[mode]); });
    themeBtns.forEach(function (b) {
      var on = b.getAttribute('data-theme-set') === mode;
      b.setAttribute('aria-checked', String(on));
      b.tabIndex = on ? 0 : -1;
    });
  }
  if (themeBtns.length) {
    applyMode(currentMode());
    themeBtns.forEach(function (b, i) {
      b.addEventListener('click', function () { applyMode(b.getAttribute('data-theme-set')); });
      b.addEventListener('keydown', function (e) {
        var n = null;
        if (e.key === 'ArrowRight' || e.key === 'ArrowDown') n = (i + 1) % themeBtns.length;
        else if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') n = (i - 1 + themeBtns.length) % themeBtns.length;
        if (n !== null) {
          e.preventDefault();
          applyMode(themeBtns[n].getAttribute('data-theme-set'));
          themeBtns[n].focus();
        }
      });
    });
  }

  // ---- mobile menu ----
  var menuBtn = document.getElementById('menu-btn');
  var nav = document.getElementById('site-nav');
  function setMenu(open) {
    if (!menuBtn || !nav) return;
    nav.classList.toggle('open', open);
    menuBtn.setAttribute('aria-expanded', String(open));
  }
  if (menuBtn && nav) {
    menuBtn.addEventListener('click', function () { setMenu(menuBtn.getAttribute('aria-expanded') !== 'true'); });
    nav.addEventListener('click', function (e) { if (e.target.closest('a')) setMenu(false); });
    document.addEventListener('keydown', function (e) { if (e.key === 'Escape') setMenu(false); });
  }

  // ---- header: condenses once the page scrolls ----
  var header = document.querySelector('.site-header');
  if (header) {
    var ticking = false;
    var onScroll = function () {
      if (ticking) return;
      ticking = true;
      requestAnimationFrame(function () {
        header.classList.toggle('scrolled', window.scrollY > 8);
        ticking = false;
      });
    };
    window.addEventListener('scroll', onScroll, { passive: true });
    onScroll();
  }

  // ---- copy buttons ----
  function fallbackCopy(text) {
    var ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    var ok = false;
    try { ok = document.execCommand('copy'); } catch (e) { ok = false; }
    document.body.removeChild(ta);
    return ok;
  }
  document.querySelectorAll('[data-copy]').forEach(function (btn) {
    var label = btn.querySelector('.copy-label');
    btn.addEventListener('click', function () {
      var target = document.querySelector(btn.getAttribute('data-copy'));
      if (!target) return;
      var text = target.textContent.trim();
      var done = function (ok) {
        btn.classList.toggle('copied', ok);
        if (label) label.textContent = ok ? 'Copied!' : 'Press Ctrl+C';
        setTimeout(function () { btn.classList.remove('copied'); if (label) label.textContent = 'Copy'; }, 2000);
      };
      if (navigator.clipboard && window.isSecureContext) {
        navigator.clipboard.writeText(text).then(function () { done(true); }, function () { done(fallbackCopy(text)); });
      } else {
        done(fallbackCopy(text));
      }
    });
  });

  // ---- install tabs (arrow keys, Home/End) ----
  document.querySelectorAll('[data-tabs]').forEach(function (box) {
    var tabs = Array.prototype.slice.call(box.querySelectorAll('[role="tab"]'));
    var panels = tabs.map(function (t) { return document.getElementById(t.getAttribute('aria-controls')); });
    var cmdEl = box.querySelector('[data-install-cmd]');
    var cardsUnix = document.querySelector('[data-cards="unix"]');
    var cardsWindows = document.querySelector('[data-cards="windows"]');
    function select(i, focus) {
      tabs.forEach(function (t, n) {
        var on = n === i;
        t.setAttribute('aria-selected', String(on));
        t.tabIndex = on ? 0 : -1;
        panels[n].hidden = !on;
      });
      var kind = tabs[i].getAttribute('data-cmd') || 'unix';
      if (cmdEl && installCmds[kind]) cmdEl.textContent = installCmds[kind];
      if (cardsUnix) cardsUnix.hidden = kind !== 'unix';
      if (cardsWindows) cardsWindows.hidden = kind !== 'windows';
      if (focus) tabs[i].focus();
    }
    tabs.forEach(function (t, i) {
      t.addEventListener('click', function () { select(i, false); });
      t.addEventListener('keydown', function (e) {
        var n = null;
        if (e.key === 'ArrowRight') n = (i + 1) % tabs.length;
        else if (e.key === 'ArrowLeft') n = (i - 1 + tabs.length) % tabs.length;
        else if (e.key === 'Home') n = 0;
        else if (e.key === 'End') n = tabs.length - 1;
        if (n !== null) { e.preventDefault(); select(n, true); }
      });
    });
    var initial = detectedId ? tabs.findIndex(function (t) { return t.id === detectedId; }) : -1;
    select(initial >= 0 ? initial : 0, false);
  });

  // ---- demo views (across two laptops / on one machine): plain tabs, nothing else changes ----
  document.querySelectorAll('[data-switch]').forEach(function (box) {
    var tabs = Array.prototype.slice.call(box.querySelectorAll('[role="tab"]'));
    var panels = tabs.map(function (t) { return document.getElementById(t.getAttribute('aria-controls')); });
    function select(i, focus) {
      tabs.forEach(function (t, n) {
        var on = n === i;
        t.setAttribute('aria-selected', String(on));
        t.tabIndex = on ? 0 : -1;
        if (panels[n]) panels[n].hidden = !on;
      });
      if (focus) tabs[i].focus();
    }
    tabs.forEach(function (t, i) {
      t.addEventListener('click', function () { select(i, false); });
      t.addEventListener('keydown', function (e) {
        var n = null;
        if (e.key === 'ArrowRight') n = (i + 1) % tabs.length;
        else if (e.key === 'ArrowLeft') n = (i - 1 + tabs.length) % tabs.length;
        else if (e.key === 'Home') n = 0;
        else if (e.key === 'End') n = tabs.length - 1;
        if (n !== null) { e.preventDefault(); select(n, true); }
      });
    });
  });

  // ---- the solution's four perks: tap one to read what it means ----
  document.querySelectorAll('[data-perks]').forEach(function (wrap) {
    var btns = Array.prototype.slice.call(wrap.querySelectorAll('.perk'));
    var openBtn = null;
    function aim(btn, detail) {
      var w = wrap.getBoundingClientRect(), b = btn.getBoundingClientRect();
      var x = b.left + b.width / 2 - w.left;
      detail.style.setProperty('--ax', Math.max(18, Math.min(w.width - 18, x)) + 'px');
    }
    function close(returnFocus) {
      if (!openBtn) return;
      var detail = document.getElementById(openBtn.getAttribute('aria-controls'));
      openBtn.setAttribute('aria-expanded', 'false');
      if (detail) detail.classList.remove('open');
      if (returnFocus) openBtn.focus();
      openBtn = null;
    }
    function open(btn) {
      if (openBtn === btn) { close(false); return; }
      close(false);
      var detail = document.getElementById(btn.getAttribute('aria-controls'));
      if (!detail) return;
      aim(btn, detail);
      btn.setAttribute('aria-expanded', 'true');
      detail.classList.add('open');
      openBtn = btn;
    }
    btns.forEach(function (b) { b.addEventListener('click', function () { open(b); }); });
    document.addEventListener('keydown', function (e) { if (e.key === 'Escape' && openBtn) close(true); });
    document.addEventListener('click', function (e) { if (openBtn && !wrap.contains(e.target)) close(false); });
    window.addEventListener('resize', function () {
      if (openBtn) aim(openBtn, document.getElementById(openBtn.getAttribute('aria-controls')));
    });
    wrap.addEventListener('perks:close', function () { close(false); });
  });

  // ---- how it works: the problem, then "Add Relay" slides in the solution ----
  // Always opens on the problem (nothing is remembered, and no hash or query picks the
  // slide), so every visit, including a reload, tells the story from the start.
  document.querySelectorAll('[data-carousel]').forEach(function (box) {
    var viewport = box.querySelector('.carousel-viewport');
    var slides = Array.prototype.slice.call(box.querySelectorAll('.slide'));
    var tabs = Array.prototype.slice.call(box.querySelectorAll('[role="tab"]'));
    var solutionSea = box.querySelector('.sea-solution');
    var current = -1;
    function fitHeight() {
      if (current >= 0 && viewport) viewport.style.setProperty('--h', slides[current].offsetHeight + 'px');
    }
    function go(i, opts) {
      opts = opts || {};
      if (i === current) return;
      current = i;
      box.classList.toggle('at-1', i === 1);
      slides.forEach(function (sl, n) {
        var on = n === i;
        sl.toggleAttribute('inert', !on);
        sl.setAttribute('aria-hidden', String(!on));
      });
      tabs.forEach(function (t, n) {
        t.setAttribute('aria-selected', String(n === i));
        t.tabIndex = n === i ? 0 : -1;
      });
      if (solutionSea) {
        solutionSea.classList.remove('lit');
        if (i === 1) {
          void solutionSea.offsetWidth; // restart the arrival and lighting from the beginning
          solutionSea.classList.add('lit');
        }
      }
      box.querySelectorAll('[data-perks]').forEach(function (w) { w.dispatchEvent(new CustomEvent('perks:close')); });
      if (toggle) {
        if (toggleLabel) toggleLabel.textContent = i === 1 ? 'Remove Relay' : 'Add Relay';
        toggle.setAttribute('aria-controls', i === 1 ? 'problem' : 'solution');
      }
      fitHeight();
      if (opts.focusTitle) {
        var title = slides[i].querySelector('.slide-title');
        if (title) title.focus({ preventScroll: true });
      }
      if (opts.scroll) {
        var top = box.getBoundingClientRect().top;
        if (top < 0 || top > window.innerHeight * 0.6) {
          box.scrollIntoView({ behavior: reduceMotion.matches ? 'auto' : 'smooth', block: 'start' });
        }
      }
    }
    // One button right under the tabs: "Add Relay" on the problem, "Remove Relay" on the solution.
    var toggle = box.querySelector('[data-toggle-relay]');
    var toggleLabel = toggle && toggle.querySelector('.btn-add-label');
    if (toggle) {
      toggle.addEventListener('click', function () { go(current === 0 ? 1 : 0, { scroll: true }); });
    }
    tabs.forEach(function (t, i) {
      t.addEventListener('click', function () { go(i, {}); });
      t.addEventListener('keydown', function (e) {
        var n = null;
        if (e.key === 'ArrowRight') n = Math.min(i + 1, tabs.length - 1);
        else if (e.key === 'ArrowLeft') n = Math.max(i - 1, 0);
        else if (e.key === 'Home') n = 0;
        else if (e.key === 'End') n = tabs.length - 1;
        if (n !== null) { e.preventDefault(); go(n, {}); tabs[n].focus(); }
      });
    });

    // Swipe: a mostly-horizontal drag of 50px or more switches slides; vertical scrolling is untouched.
    if (viewport) {
      var sx = 0, sy = 0, tracking = false;
      viewport.addEventListener('pointerdown', function (e) {
        if (e.pointerType === 'mouse' && e.button !== 0) return;
        if (e.target.closest('a, button, code, pre')) return;
        sx = e.clientX; sy = e.clientY; tracking = true;
      });
      viewport.addEventListener('pointerup', function (e) {
        if (!tracking) return;
        tracking = false;
        var dx = e.clientX - sx, dy = e.clientY - sy;
        if (Math.abs(dx) < 50 || Math.abs(dx) < Math.abs(dy) * 1.5) return;
        if (dx < 0 && current < slides.length - 1) go(current + 1, {});
        else if (dx > 0 && current > 0) go(current - 1, {});
      });
      viewport.addEventListener('pointercancel', function () { tracking = false; });
      if ('ResizeObserver' in window) {
        var ro = new ResizeObserver(fitHeight);
        slides.forEach(function (sl) { ro.observe(sl); });
      } else {
        window.addEventListener('resize', fitHeight);
      }
    }
    go(0, {});
  });

  // ---- scroll reveal (and the hero terminal's typing, which starts once it is seen) ----
  var revealables = Array.prototype.slice.call(document.querySelectorAll('[data-reveal], .term'));
  if (reduceMotion.matches || !('IntersectionObserver' in window)) {
    revealables.forEach(function (el) { el.classList.add('in'); });
  } else {
    var io = new IntersectionObserver(function (entries) {
      entries.forEach(function (en) {
        if (en.isIntersecting) { en.target.classList.add('in'); io.unobserve(en.target); }
      });
    }, { rootMargin: '0px 0px -8% 0px', threshold: 0.08 });
    revealables.forEach(function (el) { io.observe(el); });
  }

  // ---- card spotlight: a soft brand-coloured glow that follows the pointer ----
  if (window.matchMedia('(hover: hover)').matches && !reduceMotion.matches) {
    document.querySelectorAll('.card').forEach(function (card) {
      card.addEventListener('pointermove', function (e) {
        var r = card.getBoundingClientRect();
        card.style.setProperty('--mx', (e.clientX - r.left) + 'px');
        card.style.setProperty('--my', (e.clientY - r.top) + 'px');
      });
    });
  }
})();
