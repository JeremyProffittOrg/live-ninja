package webapp

// The code-update progress endpoint.
//
// A coding agent running on one of the owner's Windows machines has no way to
// send mail: the node has no SES credential, and giving it one would hand a
// coding agent open send rights on the account. So it reports progress here
// instead, and Live Ninja — which already owns an email pipeline — turns each
// report into one message to the owner's own inbox.
//
// This is the ONE public, pre-auth route in the code-update feature, so its
// discipline matters more than its size:
//
//   - It is mounted OUTSIDE the /api/v1 group. That group's prefix middleware
//     covers every path under it, so a route registered there would inherit
//     RequireAuth and reject the node — which has no session. The public prefix
//     is /v1, matching the Android distribution routes.
//   - The credential is a per-run bearer token, minted by the dispatch worker
//     and embedded in exactly one prompt. Only its SHA-256 is stored, the
//     comparison is constant-time, and the row expires in 24 h.
//   - Every failure is an indistinguishable 401. A wrong secret, an unknown
//     request, an expired row and a malformed header must not be tellable apart,
//     or the endpoint becomes an oracle for which run ids exist.
//   - The recipient is ALWAYS the configured owner address. Nothing in the
//     request body influences where mail goes, so a leaked token buys the
//     ability to email the owner and nothing else.
//   - The post count is claimed with a CONDITIONAL update before any mail is
//     enqueued, so a looping agent cannot flood the inbox and two concurrent
//     posts cannot both slip past the bound.

import (
	"errors"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/codeupdate"
)

// maxProgressSummary bounds one report. Longer summaries are TRUNCATED rather
// than rejected: a verbose agent should not lose its report, and the owner
// still gets the first, most useful paragraph.
const maxProgressSummary = 4000

// progressStatuses is the closed set the prompt teaches. Anything else is a
// 400 — it means the agent improvised, and the subject line is built from this.
var progressStatuses = map[string]struct{}{
	"planning": {},
	"working":  {},
	"blocked":  {},
	"done":     {},
}

type progressRequest struct {
	Status  string `json:"status"`
	Summary string `json:"summary"`
}

// RegisterCodeUpdateRoutes mounts the public progress endpoint. It MUST be
// called before RegisterAPIRoutes' /api/v1 group is used for anything under
// /v1, and it deliberately does not sit under that prefix at all.
func RegisterCodeUpdateRoutes(app *fiber.App, deps *Deps) {
	app.Post("/v1/code-update/progress", handleCodeUpdateProgress(deps))
}

func handleCodeUpdateProgress(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		log := deps.Log
		if log == nil {
			log = slog.Default()
		}

		if deps.CodeUpdate == nil || deps.CodeUpdateDispatcher == nil {
			// Not configured is not the caller's fault, and must not read as a
			// credential failure.
			return c.Status(fiber.StatusServiceUnavailable).
				JSON(fiber.Map{"error": "code update progress is not configured"})
		}

		// (1) Credential shape, before any store is touched. This is what keeps a
		//     session cookie or a bearer JWT from ever reaching this validator.
		token := runBearerToken(c)
		requestID, secret, err := codeupdate.ParseToken(token)
		if err != nil {
			return progressUnauthorized(c)
		}

		// (2) The token row. Absent (or TTL-expired) is the same 401 as wrong.
		row, err := deps.CodeUpdate.GetToken(c.UserContext(), requestID)
		if err != nil {
			if !errors.Is(err, codeupdate.ErrNotFound) {
				log.Error("code update progress: token lookup failed", "error", err.Error())
			}
			return progressUnauthorized(c)
		}
		if err := codeupdate.VerifySecret(secret, row.TokenHash); err != nil {
			log.Warn("code update progress: token mismatch", "request_id", requestID)
			return progressUnauthorized(c)
		}

		// (3) Body. Parsed only after the credential is good, so an unauthenticated
		//     caller learns nothing from a validation message.
		var body progressRequest
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
		}
		status := strings.ToLower(strings.TrimSpace(body.Status))
		if _, ok := progressStatuses[status]; !ok {
			return c.Status(fiber.StatusBadRequest).
				JSON(fiber.Map{"error": "status must be one of: planning, working, blocked, done"})
		}
		summary := strings.TrimSpace(body.Summary)
		if summary == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "summary is required"})
		}
		summary = truncateRunes(summary, maxProgressSummary)

		// (4) Claim a post BEFORE sending. A conditional increment is what makes
		//     the bound hold under concurrency; a read-then-write would let two
		//     simultaneous posts both observe "7 so far".
		count, err := deps.CodeUpdate.ClaimProgressPost(c.UserContext(), requestID)
		if err != nil {
			if errors.Is(err, codeupdate.ErrPostLimit) {
				return c.Status(fiber.StatusTooManyRequests).
					JSON(fiber.Map{"error": "this run has sent its maximum number of progress reports"})
			}
			log.Error("code update progress: claim failed", "error", err.Error())
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		}

		remaining := codeupdate.MaxProgressPosts - count
		if remaining < 0 {
			remaining = 0
		}
		deps.CodeUpdateDispatcher.EmailProgress(c.UserContext(), row.Repo, status, summary, remaining, log)

		log.Info("code update progress accepted",
			"request_id", requestID, "repo", row.Repo, "status", status, "post_count", count)

		return c.JSON(fiber.Map{"accepted": true, "remaining": remaining})
	}
}

// progressUnauthorized is the single failure response. Every credential-class
// failure returns exactly this, with no detail.
func progressUnauthorized(c *fiber.Ctx) error {
	return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
}

// runBearerToken extracts the run token from the Authorization header. It is
// deliberately separate from middleware.go's bearerToken, which requires the
// "Bearer " prefix: a coding agent transcribing a curl from a prompt may drop
// it, and a bare, correctly-shaped cu_ token is unambiguous either way. Shape
// is still enforced by ParseToken, so this looseness widens nothing.
func runBearerToken(c *fiber.Ctx) string {
	h := strings.TrimSpace(c.Get(fiber.HeaderAuthorization))
	if h == "" {
		return ""
	}
	const prefix = "bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return h
}

// truncateRunes bounds s by code points, never splitting one.
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}
