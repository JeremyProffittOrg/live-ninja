// Theme boot — runs synchronously in <head> before first paint to avoid a
// flash of the wrong theme. The settings page persists the user's choices to
// localStorage alongside the settings-document PUT; CSP disallows inline
// scripts, which is why this is a file.
//
// Axes (podcasts.jeremy.ninja-style):
//   ln-theme        "light" | "dark" | "system"
//                   (absent -> LIGHT, the app-zone default; "system" ->
//                   prefers-color-scheme; SSR'd data-theme is respected)
//   ln.appearance   {"appStyle":  "ninja"   | "hal9000" | "minimal" | "terminal",
//                    "liveStyle": "hal9000" | "ninja"   | "minimal" | "terminal",
//                    "accentColor": "#rrggbb" | ""  ("" = style default)}
//                   (a legacy cached {"themeStyle": ...} migrates to liveStyle)
//
// TWO STYLE ZONES: appStyle lands as data-ln-style on <html> and styles
// everything (nav, transcript side, settings, history, memory, downloads,
// landing); liveStyle lands as data-ln-style on #livePanel — the
// conversation page's orb/mic rail, whose template pre-stamps the default
// so there's no flash. app.css scopes the style blocks with bare
// [data-ln-style=...] selectors so the same block works on either element.
// The accent recolors the --ln-teal/--ln-cyan token family inline per zone:
// an explicit accentColor is global (both zones), while "" (auto) resolves
// to each zone's own style default.
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

  function setAccent(el, accent) {
    el.style.setProperty("--ln-teal", accent);
    el.style.setProperty("--ln-cyan", accent);
    el.style.setProperty("--ln-teal-600", shade(accent, 0.72));
    el.style.setProperty("--ln-accent", accent);
    el.style.setProperty(
      "--ln-shadow-teal",
      "0 0 0 1px " + accent + "33, 0 0 28px " + accent + "59"
    );
  }

  function styleOr(value, fallback) {
    return STYLE_ACCENTS.hasOwnProperty(value) ? value : fallback;
  }

  function apply(appearance) {
    var ap = appearance || {};
    var appStyle = styleOr(ap.appStyle, "ninja");
    // Back-compat: the pre-split single themeStyle styled the conversation
    // orb/mic panel, so it migrates to liveStyle.
    var liveStyle = styleOr(ap.liveStyle, styleOr(ap.themeStyle, "hal9000"));

    var accent = ap.accentColor || "";
    if (!/^#[0-9a-fA-F]{6}$/.test(accent)) accent = "";

    var root = document.documentElement;
    root.setAttribute("data-ln-style", appStyle);
    setAccent(root, accent || STYLE_ACCENTS[appStyle]);

    var applyLive = function () {
      var panel = document.getElementById("livePanel");
      if (!panel) return; // page without a live panel (settings, history, …)
      panel.setAttribute("data-ln-style", liveStyle);
      setAccent(panel, accent || STYLE_ACCENTS[liveStyle]);
    };
    if (document.readyState === "loading" && !document.getElementById("livePanel")) {
      // Head boot: the body hasn't parsed yet — stamp the panel as soon as
      // the DOM exists (the SSR'd default attribute prevents any flash).
      document.addEventListener("DOMContentLoaded", applyLive, { once: true });
    } else {
      applyLive();
    }
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
    var root = document.documentElement;
    if (t === "light" || t === "dark") {
      root.setAttribute("data-theme", t);
    } else if (t !== "system" && !root.hasAttribute("data-theme")) {
      // No stored choice and no SSR'd theme: the app default is LIGHT
      // (ninja-light). An explicit "system" keeps prefers-color-scheme.
      root.setAttribute("data-theme", "light");
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
