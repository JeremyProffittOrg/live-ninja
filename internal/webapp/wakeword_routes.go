// Wake-word surface (M6 FR-K01..06), owned by the wake-word backend
// workstream. Routes (all Session-JWT authed — the public catalog
// snapshot named in contracts/api.md is a separate static S3 asset, not
// a live route here):
//
//   - POST   /api/v1/wakeword            — create a custom wake phrase
//     {phrase, engine} → 202 pending item; validation 400, collision
//     409, ≤3/day + queue-full 429, non-trainable engine 400.
//   - GET    /api/v1/wakeword            — catalog: builtins + the
//     caller's customs, engine trainability flags, esp32CustomSupported.
//   - GET    /api/v1/wakeword/:id        — one entry's status (lazily
//     finalizes in-flight training via S3 manifests + Batch
//     DescribeJobs — the locked no-poller design).
//   - GET    /api/v1/wakeword/:id/model?platform={web|android|esp32} —
//     contracts/wakeword-manifest.md manifest (presigned 15-min URL +
//     sha256); 409 {"status":...} while not ready (the contract's
//     "implementers pick one" choice, fixed here as 409).
//   - DELETE /api/v1/wakeword/:id        — cancel training (best
//     effort), purge S3 artifacts, remove the item.
//
// contracts/api.md spells the create/status routes plural
// (/v1/wakewords, /v1/wakewords/{id}) and the manifest route singular
// (/v1/wakeword/{id}/model); both spellings are registered under both
// /api/v1 (this app's canonical API prefix) and bare /v1 (the
// contract-literal prefix) so every documented path resolves.
package webapp

import (
	"context"
	"errors"
	"log/slog"
	"os"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/wakeword"
)

// defaultWakewordsBucket matches template.yaml's WakewordsBucket
// (BucketName live-ninja-wakewords-759775734231) as a fallback when the
// WAKEWORDS_BUCKET env var is unset.
const defaultWakewordsBucket = "live-ninja-wakewords-759775734231"

// RegisterWakewordRoutes mounts the wake-word API. Called once from
// cmd/web/main.go after Deps is built, behind the app-wide
// ExtractAuthContext/CSRFProtect middleware. The Service is built here
// at cold start (same pattern as buildAPIToolsRegistry): AWS client
// construction failure degrades to 503 service_unavailable on these
// routes only, never a startup crash.
func RegisterWakewordRoutes(app *fiber.App, deps *Deps) {
	svc := buildWakewordService(deps)

	register := func(g fiber.Router) {
		// Create + catalog, singular (role spec) and plural (api.md).
		g.Post("/wakeword", handleWakewordCreate(deps, svc))
		g.Post("/wakewords", handleWakewordCreate(deps, svc))
		g.Get("/wakeword", handleWakewordCatalog(deps, svc))
		g.Get("/wakewords", handleWakewordCatalog(deps, svc))
		// Status + delete.
		g.Get("/wakeword/:id", handleWakewordGet(deps, svc))
		g.Get("/wakewords/:id", handleWakewordGet(deps, svc))
		g.Delete("/wakeword/:id", handleWakewordDelete(deps, svc))
		g.Delete("/wakewords/:id", handleWakewordDelete(deps, svc))
		// Model manifest (contracts/wakeword-manifest.md — singular only).
		g.Get("/wakeword/:id/model", handleWakewordModel(deps, svc))
	}
	register(app.Group("/api/v1", RequireAuth()))
	register(app.Group("/v1", RequireAuth()))
}

// buildWakewordService constructs the wakeword.Service once at cold
// start. Env contract with template.yaml (WebFunction Environment):
// WAKEWORDS_BUCKET, BATCH_JOB_QUEUE, BATCH_JOB_DEFINITION — the latter
// two name the AWS Batch Fargate-ARM64 queue/definition for
// containers/wakeword-train (conc≤2 enforced by the compute env).
func buildWakewordService(deps *Deps) *wakeword.Service {
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background())
	if err != nil {
		deps.Log.Error("wakeword: load aws config failed", slog.String("error", err.Error()))
		return nil
	}

	bucket := os.Getenv("WAKEWORDS_BUCKET")
	if bucket == "" {
		bucket = defaultWakewordsBucket
	}
	cfg := wakeword.Config{
		Bucket:        bucket,
		JobQueue:      envFirst("BATCH_JOB_QUEUE", "WAKEWORD_JOB_QUEUE"),
		JobDefinition: envFirst("BATCH_JOB_DEFINITION", "WAKEWORD_JOB_DEFINITION"),
	}

	userEmail := func(ctx context.Context, userID string) (string, error) {
		u, err := deps.Store.GetUser(ctx, userID)
		if err != nil {
			return "", err
		}
		if u == nil {
			return "", errors.New("user not found")
		}
		return u.Email, nil
	}

	return wakeword.NewFromAWS(awsCfg, cfg, deps.Store, deps.Log, userEmail, deps.EnqueueEmail)
}

// envFirst returns the first non-empty environment variable among names
// (template.yaml sets BATCH_JOB_QUEUE/BATCH_JOB_DEFINITION; the
// WAKEWORD_-prefixed spellings are accepted as a fallback so a template
// rename can't silently strand the training path).
func envFirst(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// wakewordUnavailable is the degraded response when cold-start service
// construction failed (no stub behavior — an explicit, truthful 503).
func wakewordUnavailable(c *fiber.Ctx) error {
	return errorJSON(c, fiber.StatusServiceUnavailable, "service_unavailable", "wake-word service is not available right now")
}

// ---- POST /api/v1/wakeword ----

func handleWakewordCreate(deps *Deps, svc *wakeword.Service) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if svc == nil {
			return wakewordUnavailable(c)
		}
		var body struct {
			Phrase string `json:"phrase"`
			Engine string `json:"engine"`
		}
		if err := c.BodyParser(&body); err != nil {
			return apiBadRequest(c, "invalid JSON body")
		}
		if body.Phrase == "" {
			return apiBadRequest(c, "phrase is required")
		}

		item, err := svc.Create(c.Context(), UserID(c), body.Phrase, body.Engine)
		if err != nil {
			return wakewordError(c, deps, "create wakeword", err)
		}
		// 202: creation accepted, training runs async (poll GET :id).
		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
			"id":        item.ID,
			"phrase":    item.Phrase,
			"engine":    item.Engine,
			"status":    item.Status,
			"createdAt": item.CreatedAt,
		})
	}
}

// ---- GET /api/v1/wakeword (catalog) ----

func handleWakewordCatalog(deps *Deps, svc *wakeword.Service) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if svc == nil {
			return wakewordUnavailable(c)
		}
		cat, err := svc.Catalog(c.Context(), UserID(c))
		if err != nil {
			return apiInternalError(c, deps, "wakeword catalog", err)
		}
		return c.JSON(cat)
	}
}

// ---- GET /api/v1/wakeword/:id (status) ----

func handleWakewordGet(deps *Deps, svc *wakeword.Service) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if svc == nil {
			return wakewordUnavailable(c)
		}
		entry, err := svc.Get(c.Context(), UserID(c), c.Params("id"))
		if err != nil {
			return wakewordError(c, deps, "get wakeword", err)
		}
		return c.JSON(entry)
	}
}

// ---- GET /api/v1/wakeword/:id/model ----

func handleWakewordModel(deps *Deps, svc *wakeword.Service) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if svc == nil {
			return wakewordUnavailable(c)
		}
		platform := c.Query("platform")
		if platform == "" {
			return apiBadRequest(c, "platform query parameter is required (web, android, or esp32)")
		}
		man, err := svc.Model(c.Context(), UserID(c), c.Params("id"), platform)
		if err != nil {
			return wakewordError(c, deps, "wakeword model manifest", err)
		}
		return c.JSON(man)
	}
}

// ---- DELETE /api/v1/wakeword/:id ----

func handleWakewordDelete(deps *Deps, svc *wakeword.Service) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if svc == nil {
			return wakewordUnavailable(c)
		}
		if err := svc.Delete(c.Context(), UserID(c), c.Params("id")); err != nil {
			if errors.Is(err, wakeword.ErrBuiltinModel) {
				return errorJSON(c, fiber.StatusBadRequest, "builtin_model", "built-in wake words cannot be deleted")
			}
			return wakewordError(c, deps, "delete wakeword", err)
		}
		return c.SendStatus(fiber.StatusNoContent)
	}
}

// wakewordError maps the service's typed errors onto the HTTP surface.
func wakewordError(c *fiber.Ctx, deps *Deps, op string, err error) error {
	var vErr *wakeword.ValidationError
	if errors.As(err, &vErr) {
		return apiBadRequest(c, vErr.Msg)
	}
	var nrErr *wakeword.NotReadyError
	if errors.As(err, &nrErr) {
		// Contract choice (wakeword-manifest.md): not-ready → 409 with a
		// status body, consistently.
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"status": nrErr.Status})
	}
	switch {
	case errors.Is(err, wakeword.ErrNotFound):
		return errorJSON(c, fiber.StatusNotFound, "not_found", "Wake word not found.")
	case errors.Is(err, wakeword.ErrBuiltinModel):
		return errorJSON(c, fiber.StatusNotFound, "builtin_model", "built-in models ship with the client and are not downloadable")
	case errors.Is(err, wakeword.ErrPlatformUnsupported):
		return errorJSON(c, fiber.StatusNotFound, "unsupported_platform",
			"custom wake words are not available on esp32 yet — the device selects among built-in WakeNet models")
	case errors.Is(err, wakeword.ErrEngineUnavailable):
		return errorJSON(c, fiber.StatusBadRequest, "engine_unavailable", "only the openwakeword engine supports custom training on this server")
	case errors.Is(err, wakeword.ErrCollision):
		return errorJSON(c, fiber.StatusConflict, "phrase_conflict", "that phrase already exists in your catalog — pick a different one")
	case errors.Is(err, wakeword.ErrDailyLimit):
		c.Set(fiber.HeaderRetryAfter, "86400")
		return errorJSON(c, fiber.StatusTooManyRequests, "daily_limit", "custom wake-word training is limited to 3 per day — try again tomorrow")
	case errors.Is(err, wakeword.ErrQueueFull):
		c.Set(fiber.HeaderRetryAfter, "600")
		return errorJSON(c, fiber.StatusTooManyRequests, "queue_full", "the training queue is busy — try again in a few minutes")
	}
	return apiInternalError(c, deps, op, err)
}
