// pkgmirror console — vanilla JS for two global behaviors:
//   1. Dark-mode toggle persisted in a cookie
//   2. Dismissible flash messages
//
// Per-page interactivity ships as additional .js files referenced via
// BaseData.ScriptURLs (populated by the page handler before c.Render).
// No inline scripts — the CSP forbids them.

(function () {
  'use strict';

  // ---- Theme toggle ----
  function getCookie(name) {
    const match = document.cookie.match(new RegExp('(?:^| )' + name + '=([^;]+)'));
    return match ? decodeURIComponent(match[1]) : null;
  }

  function setCookie(name, value, days) {
    const exp = new Date();
    exp.setTime(exp.getTime() + days * 24 * 60 * 60 * 1000);
    document.cookie = name + '=' + encodeURIComponent(value) + ';expires=' + exp.toUTCString() + ';path=/console;samesite=lax';
  }

  function applyTheme(value) {
    const html = document.documentElement;
    if (value === 'dark') {
      html.classList.add('dark');
    } else {
      html.classList.remove('dark');
    }
  }

  // Hydrate immediately if a cookie is set (overrides whatever the
  // server rendered into the class attribute).
  const persisted = getCookie('pkgmirror_theme');
  if (persisted === 'dark' || persisted === 'light') {
    applyTheme(persisted);
  }

  document.addEventListener('click', function (e) {
    const toggle = e.target.closest('[data-theme-toggle]');
    if (!toggle) return;
    e.preventDefault();
    const isDark = document.documentElement.classList.contains('dark');
    const next = isDark ? 'light' : 'dark';
    applyTheme(next);
    setCookie('pkgmirror_theme', next, 365);
  });

  // ---- Flash dismiss ----
  document.addEventListener('click', function (e) {
    const btn = e.target.closest('[data-flash-dismiss]');
    if (!btn) return;
    const flash = btn.closest('.flash');
    if (flash && flash.parentNode) {
      flash.parentNode.removeChild(flash);
    }
  });
})();
