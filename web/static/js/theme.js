// Theme boot — runs synchronously in <head> before first paint to avoid a
// flash of the wrong theme. The settings page persists the user's choices to
// localStorage alongside the settings-document PUT; CSP disallows inline
// scripts, which is why this is a file.
//
// Two independent axes (podcasts.jeremy.ninja-style):
//   ln-theme        "light" | "dark"           (unset -> prefers-color-scheme)
//   ln.appearance   {"themeStyle": "hal9000"|"ninja"|"minimal"|"terminal",
//                    "accentColor": "#rrggbb" | ""  ("" = style default)}
// themeStyle lands as data-ln-style on <html> (app.css restyles tokens + the
// orb per style); the accent recolors the --ln-teal/--ln-cyan token family
// inline so every gradient/glow picks it up without CSS surgery.
(function () {
  "use strict";

  var STYLE_ACCENTS = {
    hal9000: "#e32636",
    ninja: "#22e0d0",
    minimal: "#38d0ff",
    terminal: "#33ff66",
  };

  function shade(hex, factor) {
    // #rrggbb scaled toward black (factor < 1) — the "-600" token variant.
    var n = parseInt(hex.slice(1), 16);
    var r = Math.round(((n >> 16) & 255) * factor);
    var g = Math.round(((n >> 8) & 255) * factor);
    var b = Math.round((n & 255) * factor);
    return "#" + ((1 << 24) | (r << 16) | (g << 8) | b).toString(16).slice(1);
  }

  function apply(appearance) {
    var root = document.documentElement;
    var style = (appearance && appearance.themeStyle) || "hal9000";
    if (!STYLE_ACCENTS.hasOwnProperty(style)) style = "hal9000";
    root.setAttribute("data-ln-style", style);

    var accent = (appearance && appearance.accentColor) || "";
    if (!/^#[0-9a-fA-F]{6}$/.test(accent)) accent = STYLE_ACCENTS[style];
    root.style.setProperty("--ln-teal", accent);
    root.style.setProperty("--ln-cyan", accent);
    root.style.setProperty("--ln-teal-600", shade(accent, 0.72));
    root.style.setProperty("--ln-accent", accent);
    root.style.setProperty(
      "--ln-shadow-teal",
      "0 0 0 1px " + accent + "33, 0 0 28px " + accent + "59"
    );
  }

  // Settings/conversation pages call this after loading or saving the
  // settings document so changes apply live and cache for the next boot.
  window.__lnApplyAppearance = function (appearance, persist) {
    try {
      apply(appearance);
      if (persist !== false) {
        localStorage.setItem("ln.appearance", JSON.stringify(appearance || {}));
      }
    } catch (e) {
      /* non-fatal */
    }
  };

  try {
    var t = localStorage.getItem("ln-theme");
    if (t === "light" || t === "dark") {
      document.documentElement.setAttribute("data-theme", t);
    }
  } catch (e) {
    /* storage blocked — fall through to prefers-color-scheme */
  }

  try {
    var raw = localStorage.getItem("ln.appearance");
    apply(raw ? JSON.parse(raw) : null);
  } catch (e) {
    apply(null);
  }
})();
