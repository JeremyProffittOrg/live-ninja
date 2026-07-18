// Versioning/compatibility surface owned by this workstream
// (contracts/headers.md, plan.md M7 "Versioning/compat"): the
// X-LN-Server response header + X-LN-Client parsing middleware, the
// below-min 426 gate, the ClientVersions EMF metric, and the public
// GET /v1/compat capability-negotiation route.
package webapp

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
)

// BuildVersion is the backend's own deployed release version, stamped at
// compile time via:
//
//	-ldflags "-X github.com/JeremyProffittOrg/live-ninja/internal/webapp.BuildVersion=<deployedSemver>+<gitSha>"
//
// (Makefile's `build` target composes this from the BUILD_VERSION env var
// deploy.yml sets before invoking `make build`, falling back to a local
// VERSION file + `git rev-parse --short HEAD` for a bare `go build`/`go
// run` outside CI.) Sent verbatim as the X-LN-Server response header on
// every request (contracts/headers.md). Left at this default for any
// Lambda the linker doesn't actually pull internal/webapp into (the -X
// flag is a harmless no-op there) and for local dev without the ldflags.
var BuildVersion = "0.0.0+dev"

// clientHeaderPattern is contracts/headers.md's X-LN-Client grammar,
// verbatim: "<surface>/<semver>+<build>".
var clientHeaderPattern = regexp.MustCompile(`^(web|android|m5stack)/(\d+)\.(\d+)\.(\d+)\+([A-Za-z0-9._-]+)$`)

// clientVersion is a successfully parsed X-LN-Client header.
type clientVersion struct {
	surface             string
	major, minor, patch int
	build               string
}

func (v clientVersion) semver() string {
	return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
}

// parseClientVersion parses raw against clientHeaderPattern. A
// missing/malformed header (including an unrecognized surface token)
// returns ok=false — callers must degrade gracefully (never 5xx) per
// headers.md, treating an unparseable header as "unknown", not as a
// hostile input to reject outright.
func parseClientVersion(raw string) (cv clientVersion, ok bool) {
	m := clientHeaderPattern.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return clientVersion{}, false
	}
	major, err1 := strconv.Atoi(m[2])
	minor, err2 := strconv.Atoi(m[3])
	patch, err3 := strconv.Atoi(m[4])
	if err1 != nil || err2 != nil || err3 != nil {
		return clientVersion{}, false
	}
	return clientVersion{surface: m[1], major: major, minor: minor, patch: patch, build: m[5]}, true
}

// parseSemver parses a bare "MAJOR.MINOR.PATCH" string (the config-side
// min/recommended version values below — never client-supplied). Falls
// back to 0.0.0 on a malformed config value rather than panicking; that
// value is only ever written by env vars this deploy controls.
func parseSemver(s string) (major, minor, patch int) {
	parts := strings.SplitN(strings.TrimSpace(s), ".", 3)
	if len(parts) != 3 {
		return 0, 0, 0
	}
	major, _ = strconv.Atoi(parts[0])
	minor, _ = strconv.Atoi(parts[1])
	patch, _ = strconv.Atoi(parts[2])
	return major, minor, patch
}

// semverLess reports whether a < b, comparing MAJOR.MINOR.PATCH in order.
func semverLess(aMajor, aMinor, aPatch, bMajor, bMinor, bPatch int) bool {
	if aMajor != bMajor {
		return aMajor < bMajor
	}
	if aMinor != bMinor {
		return aMinor < bMinor
	}
	return aPatch < bPatch
}

// compatVersionSet is the pair of per-surface version maps GET
// /v1/compat always returns in full (contracts/headers.md), read from
// env vars set by template.yaml with defaults matching that contract's
// own worked example — a deploy that hasn't wired the env vars yet still
// serves sane, documented defaults rather than empty/zero versions.
type compatVersionSet struct {
	min         map[string]string
	recommended map[string]string
}

var compatSurfaces = []string{"web", "android", "m5stack"}

func loadCompatVersions() compatVersionSet {
	defaultMin := map[string]string{"web": "0.5.0", "android": "1.0.0", "m5stack": "1.0.0"}
	defaultRecommended := map[string]string{"web": "0.9.0", "android": "2.1.0", "m5stack": "1.4.2"}

	set := compatVersionSet{min: map[string]string{}, recommended: map[string]string{}}
	for _, surface := range compatSurfaces {
		upper := strings.ToUpper(surface)
		set.min[surface] = envDefault("MIN_SUPPORTED_CLIENT_VERSION_"+upper, defaultMin[surface])
		set.recommended[surface] = envDefault("RECOMMENDED_CLIENT_VERSION_"+upper, defaultRecommended[surface])
	}
	return set
}

func envDefault(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// compatStatus computes contracts/headers.md's status/message pair for
// one client, given the config's version maps. surface/semver come from
// an already-parsed X-LN-Client (see callers) — pass ok=false (or an
// unrecognized surface) to get the documented "surface can't be
// inferred" -> unsupported fallback.
func compatStatus(versions compatVersionSet, cv clientVersion, ok bool) (status, message string) {
	if !ok || cv.surface == "" {
		return "unsupported", "Unable to determine client surface/version from the X-LN-Client header; please update to a client that sends it."
	}
	minVer, hasMin := versions.min[cv.surface]
	if !hasMin {
		return "unsupported", "Unrecognized client surface; please update."
	}
	minMajor, minMinor, minPatch := parseSemver(minVer)
	if semverLess(cv.major, cv.minor, cv.patch, minMajor, minMinor, minPatch) {
		return "unsupported", fmt.Sprintf(
			"This %s client version (%s) is below the minimum supported version (%s). Please update.",
			cv.surface, cv.semver(), minVer)
	}

	recVer := versions.recommended[cv.surface]
	recMajor, _, _ := parseSemver(recVer)
	if recMajor-cv.major > 1 {
		return "deprecated", fmt.Sprintf(
			"This %s client version (%s) is several major versions behind the recommended version (%s). An update is available.",
			cv.surface, cv.semver(), recVer)
	}

	return "supported", ""
}

// RegisterCompatRoute mounts the public GET /v1/compat route (already
// present in cmd/authorizer's public allowlist — see that file's
// publicExact map). No auth required per headers.md: it must be reachable
// by a device that cannot yet authenticate (onboarding, expired/rotating
// credentials).
func RegisterCompatRoute(app *fiber.App, deps *Deps) {
	app.Get("/v1/compat", handleCompat())
}

func handleCompat() fiber.Handler {
	versions := loadCompatVersions()
	return func(c *fiber.Ctx) error {
		cv, ok := parseClientVersion(c.Get("X-LN-Client"))
		status, message := compatStatus(versions, cv, ok)

		body := fiber.Map{
			"apiVersion":                "v1",
			"minSupportedClientVersion": versions.min,
			"recommendedClientVersion":  versions.recommended,
			"status":                    status,
			"serverTime":                time.Now().UTC().Format(time.RFC3339),
		}
		if message != "" {
			body["message"] = message
		} else {
			body["message"] = nil
		}
		return c.JSON(body)
	}
}

// versionGateExemptPrefixes are paths the below-min 426 hard gate must
// never apply to, even for a parseable below-min X-LN-Client: the public
// bootstrap/auth/pairing/asset surface a device needs to reach in order
// to update itself, pair for the first time, or rotate credentials in
// the first place (mirrors cmd/authorizer's own public allowlist reasons
// — see contracts/headers.md's "who calls /compat and when"). /v1/compat
// itself is exempt for the same reason: it exists specifically to tell a
// stuck device *why* it's unsupported, so it must stay reachable.
var versionGateExemptPrefixes = []string{"/auth/", "/.well-known/", "/static/"}

func versionGateExempt(path string) bool {
	if path == "/v1/compat" || path == "/healthz" {
		return true
	}
	for _, p := range versionGateExemptPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// VersionMiddleware sets X-LN-Server on every response, parses
// X-LN-Client when present (emitting the ClientVersions EMF metric), and
// — for android/m5stack callers on a parseable header below
// minSupportedClientVersion, outside the bootstrap-path exemptions above
// — short-circuits with 426 Upgrade Required and an update message.
// **Never for web** (plan.md M7 locked decision): FR-W03's guaranteed
// click-to-talk fallback must never be blocked by a version check; the
// web surface only ever gets the soft update-nudge driven client-side by
// GET /v1/compat. A missing/malformed X-LN-Client never triggers the
// gate either — this middleware only ever acts on a header it could
// actually parse, per headers.md's "degrade gracefully, never error the
// whole request on a bad header" rule.
func VersionMiddleware(deps *Deps) fiber.Handler {
	versions := loadCompatVersions()
	return func(c *fiber.Ctx) error {
		c.Set("X-LN-Server", BuildVersion)

		cv, ok := parseClientVersion(c.Get("X-LN-Client"))
		if !ok {
			return c.Next()
		}

		observ.EmitMetric("LiveNinja/Versioning", "ClientVersions", 1, "Count", map[string]string{
			"surface": cv.surface,
			"semver":  cv.semver(),
		})

		if cv.surface == "web" || versionGateExempt(c.Path()) {
			return c.Next()
		}

		minVer := versions.min[cv.surface]
		minMajor, minMinor, minPatch := parseSemver(minVer)
		if semverLess(cv.major, cv.minor, cv.patch, minMajor, minMinor, minPatch) {
			deps.Log.Warn("version gate: client below minimum supported version",
				"surface", cv.surface, "clientVersion", cv.semver(), "minSupportedVersion", minVer, "path", c.Path())
			return c.Status(fiber.StatusUpgradeRequired).JSON(fiber.Map{
				"error": "client_unsupported",
				"message": fmt.Sprintf(
					"This %s client version (%s) is no longer supported (minimum %s). Please update to continue.",
					cv.surface, cv.semver(), minVer),
				"minSupportedClientVersion": minVer,
			})
		}
		return c.Next()
	}
}
