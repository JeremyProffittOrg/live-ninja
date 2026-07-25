// Package docs embeds the repo-versioned operator documents that server code
// must ship inside a binary. Today that is exactly one file: system-map.md,
// the "how this system works" cheat sheet the M17 RCA analyzer puts in front
// of Claude Opus so it reasons about *this* system rather than a generic one.
//
// It lives here, as a Go package over the real doc, because //go:embed paths
// cannot traverse ".." — internal/rca could not reach docs/system-map.md.
// Copying the markdown into internal/rca would create two files that drift;
// this keeps one reviewable source of truth. Same pattern as web/embed.go.
package docs

import _ "embed"

// SystemMap is docs/system-map.md verbatim. Reviewed like code: it is part of
// a production prompt, and its headings are load-bearing (internal/rca/prompt.go
// uses single-'#' lines as section delimiters, so this file must not contain
// any — TestSystemMapHasNoTopLevelHeadings enforces it).
//
//go:embed system-map.md
var SystemMap string
