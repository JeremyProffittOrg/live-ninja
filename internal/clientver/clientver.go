// Package clientver parses the X-LN-Client capability-negotiation header
// (contracts/headers.md) into a comparable version.
//
// It lives in its own package because two Lambdas need the same grammar from
// opposite sides of an invoke boundary: internal/webapp reads the header off
// the inbound fiber request, and cmd/realtime-broker gates Azure engine
// selection on the value the web function forwards. The broker must not import
// internal/webapp — that would link fiber into the broker's binary — so the
// grammar cannot live there (azure-voice-plan.md WS-D M1, gap register W3).
package clientver

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// headerPattern is contracts/headers.md's X-LN-Client grammar, verbatim:
// "<surface>/<semver>+<build>".
var headerPattern = regexp.MustCompile(`^(web|android|m5stack)/(\d+)\.(\d+)\.(\d+)\+([A-Za-z0-9._-]+)$`)

// Version is a successfully parsed X-LN-Client header.
type Version struct {
	Surface             string
	Major, Minor, Patch int
	Build               string
}

// Semver renders the MAJOR.MINOR.PATCH triple.
func (v Version) Semver() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// AtLeast reports whether v is greater than or equal to the given triple.
func (v Version) AtLeast(major, minor, patch int) bool {
	if v.Major != major {
		return v.Major > major
	}
	if v.Minor != minor {
		return v.Minor > minor
	}
	return v.Patch >= patch
}

// Parse parses raw against headerPattern. A missing or malformed header
// (including an unrecognized surface token, and including a pre-release
// suffix such as "0.2.2-hal", which the grammar does not admit) returns
// ok=false. Callers must degrade gracefully rather than 5xx, per headers.md —
// an unparseable header means "unknown client", never "hostile input".
func Parse(raw string) (v Version, ok bool) {
	m := headerPattern.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return Version{}, false
	}
	major, err1 := strconv.Atoi(m[2])
	minor, err2 := strconv.Atoi(m[3])
	patch, err3 := strconv.Atoi(m[4])
	if err1 != nil || err2 != nil || err3 != nil {
		return Version{}, false
	}
	return Version{Surface: m[1], Major: major, Minor: minor, Patch: patch, Build: m[5]}, true
}
