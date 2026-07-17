// Theme boot — runs synchronously in <head> before first paint to avoid a
// flash of the wrong theme. The settings page persists the user's choice to
// localStorage("ln-theme") alongside the settings-document PUT; "system"
// (or no stored value) leaves data-theme unset so prefers-color-scheme
// applies. CSP disallows inline scripts, which is why this is a file.
(function () {
  "use strict";
  try {
    // Only ever ADD the override — never remove one the server rendered
    // (the settings page SSRs data-theme from the settings document; an
    // empty localStorage must not clobber it).
    var t = localStorage.getItem("ln-theme");
    if (t === "light" || t === "dark") {
      document.documentElement.setAttribute("data-theme", t);
    }
  } catch (e) {
    /* storage blocked — fall through to prefers-color-scheme */
  }
})();
