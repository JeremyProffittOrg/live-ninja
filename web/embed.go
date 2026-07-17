// Package web embeds the SSR shell's templates and static assets for the
// Fiber web function (cmd/web). Everything under templates/ and static/ is
// compiled into the binary; internal/webapp consumes this FS to build the
// fingerprinted asset table (assets.go) and the page renderer
// (pages_routes.go).
//
// Layout (ownership: WS-D SSR-shell):
//
//	templates/layouts/    base.html — the single HTML skeleton
//	templates/partials/   nav.html, audio_viz.html
//	templates/pages/      landing.html, conversation.html, settings.html, error.html
//	static/css/           app.css — full design system (dark-first + light, WCAG AA)
//	static/js/            theme.js + landing.mjs (shell-owned); conversation.mjs,
//	                      settings.mjs and the realtime/transcript/visualizer/
//	                      wakeword modules are owned by the client-JS workstream
//	                      and are referenced by the page templates via asset().
package web

import "embed"

// Files holds the embedded templates/ and static/ trees plus sw.js (the
// service worker lives at the web/ root so it can be served at /sw.js with
// scope "/" — PWA workstream).
//
//go:embed all:templates all:static sw.js
var Files embed.FS
