// Relay site behaviour. No dependencies. Everything degrades gracefully if it fails.
(function () {
  'use strict';
  var root = document.documentElement;

  // ---- install command: always point at THIS site's own install.sh ----
  var installUrl = new URL('install.sh', document.baseURI).href;
  var installCmd = 'curl -fsSL ' + installUrl + ' | bash';
  document.querySelectorAll('[data-install-cmd]').forEach(function (el) { el.textContent = installCmd; });

  // ---- theme toggle ----
  var themeBtn = document.getElementById('theme-btn');
  function effectiveTheme() {
    var t = root.getAttribute('data-theme');
    if (t === 'light' || t === 'dark') return t;
    return window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
  }
  function labelTheme() {
    if (themeBtn) themeBtn.setAttribute('aria-label', effectiveTheme() === 'dark' ? 'Switch to light theme' : 'Switch to dark theme');
  }
  if (themeBtn) {
    labelTheme();
    themeBtn.addEventListener('click', function () {
      var next = effectiveTheme() === 'dark' ? 'light' : 'dark';
      root.setAttribute('data-theme', next);
      try { localStorage.setItem('relay-theme', next); } catch (e) { /* ignore */ }
      labelTheme();
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
    function select(i, focus) {
      tabs.forEach(function (t, n) {
        var on = n === i;
        t.setAttribute('aria-selected', String(on));
        t.tabIndex = on ? 0 : -1;
        panels[n].hidden = !on;
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
    select(0, false);
  });

  // ---- demo video: plays on a loop, forever ----
  var video = document.getElementById('demo-video');
  var overlay = document.getElementById('play-overlay');
  if (video) {
    var reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    var userPaused = reduceMotion;   // reduced motion: wait for the visitor to press play
    var autoPaused = false;          // true while WE paused it (scrolled out of view)

    var showOverlay = function (on) { if (overlay) overlay.hidden = !on; };
    var tryPlay = function () {
      var p = video.play();
      if (p && typeof p.then === 'function') {
        p.then(function () { showOverlay(false); }, function () { showOverlay(true); }); // autoplay blocked: offer a button
      }
    };

    video.muted = true; // required for autoplay; the recording has no sound anyway
    if (reduceMotion) { video.pause(); showOverlay(true); }

    video.addEventListener('pause', function () {
      if (!autoPaused && !video.ended) userPaused = true;
      autoPaused = false;
    });
    video.addEventListener('play', function () { userPaused = false; showOverlay(false); });
    // `loop` handles this; the listener is a safety net for browsers that ignore it.
    video.addEventListener('ended', function () { video.currentTime = 0; tryPlay(); });
    if (overlay) overlay.addEventListener('click', function () { userPaused = false; tryPlay(); });

    // Save battery: pause while off screen, resume when it comes back (unless the visitor paused it).
    if ('IntersectionObserver' in window) {
      new IntersectionObserver(function (entries) {
        entries.forEach(function (en) {
          if (en.isIntersecting) {
            if (!userPaused && video.paused) tryPlay();
          } else if (!video.paused) {
            autoPaused = true;
            video.pause();
          }
        });
      }, { threshold: 0.25 }).observe(video);
    }
  }
})();
