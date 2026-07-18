// RegisterAccountRoutes mounts the M7 privacy/account surface
// (FR `[PRIV]`, contracts/api.md "Account & Devices"):
//
//	POST   /api/v1/consent        — record a consent event (CONSENT#<ts>)
//	GET    /api/v1/consent        — list the caller's consent ledger
//	GET    /api/v1/account/export — export the caller's data as a deliverable
//	DELETE /api/v1/account        — right-to-delete: mark deleting + async purge
//
// The delete route follows the locked M7 decision: synchronous purge
// orchestration inside the web function is NOT acceptable (timeout risk),
// so the handler re-authorizes the caller fresh against the store, flips
// the profile to status=deleting, kills every outstanding JWT via the
// tokensValidAfter watermark, and fire-and-forgets the account-purge
// Lambda (ACCOUNT_PURGE_FUNCTION_NAME, direct async invoke) which owns
// the actual partition/S3/IoT teardown (cmd/account-purge).
package webapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// accountPurgeEvent mirrors cmd/account-purge's Event (that package is
// `main` and cannot be imported — wire-contract mirror, same pattern as
// brokerRequest). Identity fields come from the verified auth context +
// the freshly re-read profile, never from the client body.
type accountPurgeEvent struct {
	UserID       string `json:"userId"`
	Email        string `json:"email,omitempty"`
	AmazonUserID string `json:"amazonUserId,omitempty"`
	RequestedAt  string `json:"requestedAt"`
}

// exportPartTarget keeps each export deliverable comfortably under
// deliv.MaxContentBytes (1 MiB) including the JSON envelope around the
// item chunk.
const exportPartTarget = 900 << 10

// RegisterAccountRoutes mounts the consent + account privacy routes.
// Called once from cmd/web/main.go alongside the other registrars, behind
// the app-wide ExtractAuthContext/CSRFProtect middleware.
func RegisterAccountRoutes(app *fiber.App, deps *Deps) {
	api := app.Group("/api/v1", RequireAuth())

	api.Post("/consent", handleRecordConsent(deps))
	api.Get("/consent", handleListConsents(deps))

	api.Get("/account/export", handleAccountExport(deps))
	api.Delete("/account", handleDeleteAccount(deps))
}

// ---- POST /api/v1/consent ----

func handleRecordConsent(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID, surface := UserID(c), Surface(c)

		var body struct {
			// Version is the disclosure/policy version the client showed
			// (required — a consent event without a version is meaningless).
			Version string `json:"version"`
			// TS is the client-side grant timestamp (optional, RFC3339);
			// stored as informational context, never as the record key.
			TS string `json:"ts"`
		}
		if err := c.BodyParser(&body); err != nil {
			return apiBadRequest(c, "invalid JSON body")
		}
		if strings.TrimSpace(body.Version) == "" {
			return apiBadRequest(c, "version is required")
		}
		if surface == "" {
			// Defense in depth: the JWT always carries a surface, but a
			// consent row must never be recorded without one.
			return apiBadRequest(c, "authenticated surface is required")
		}
		clientTS := strings.TrimSpace(body.TS)
		if clientTS != "" {
			if _, err := time.Parse(time.RFC3339, clientTS); err != nil {
				return apiBadRequest(c, "ts must be RFC3339 (e.g. 2026-07-17T12:00:00Z)")
			}
		}

		rec, err := deps.Store.RecordConsent(c.Context(), userID, surface, body.Version, clientTS)
		if err != nil {
			return apiInternalError(c, deps, "record consent", err)
		}
		return c.Status(fiber.StatusCreated).JSON(fiber.Map{
			"ts":      rec.TS,
			"surface": rec.Surface,
			"version": rec.Version,
		})
	}
}

// ---- GET /api/v1/consent ----

func handleListConsents(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)

		consents, err := deps.Store.ListConsents(c.Context(), userID)
		if err != nil {
			return apiInternalError(c, deps, "list consents", err)
		}
		out := make([]fiber.Map, 0, len(consents))
		for _, cs := range consents {
			m := fiber.Map{"ts": cs.TS, "surface": cs.Surface, "version": cs.Version}
			if cs.ClientTS != "" {
				m["clientTs"] = cs.ClientTS
			}
			out = append(out, m)
		}
		return c.JSON(fiber.Map{"consents": out})
	}
}

// ---- GET /api/v1/account/export ----

// exportStripAttrs are attributes never included in an export: table/GSI
// key plumbing (pk repeats the caller's own id on every row; sk is kept
// as the row identity) and credential material (refresh-token hashes are
// secrets even hashed — an export must not become an offline cracking
// target).
var exportStripAttrs = map[string]bool{
	"pk": true, "gsi1pk": true, "gsi1sk": true, "gsi2pk": true, "gsi2sk": true,
	"refreshHash": true, "prevHash": true,
}

func handleAccountExport(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)
		if deps.Deliv == nil {
			return deliverablesUnavailable(c)
		}

		items, err := deps.Store.QueryUserPartition(c.Context(), userID)
		if err != nil {
			return apiInternalError(c, deps, "export: query user partition", err)
		}
		for _, it := range items {
			for k := range exportStripAttrs {
				delete(it, k)
			}
		}

		now := time.Now().UTC()
		parts, err := marshalExportParts(userID, now, items)
		if err != nil {
			return apiInternalError(c, deps, "export: marshal", err)
		}

		base := "live-ninja-export-" + now.Format("20060102-150405")
		created := make([]fiber.Map, 0, len(parts))
		for i, content := range parts {
			name := base + ".json"
			if len(parts) > 1 {
				name = fmt.Sprintf("%s-part%d-of-%d.json", base, i+1, len(parts))
			}
			d, err := deps.Deliv.Create(c.Context(), userID, name, "application/json", content)
			if err != nil {
				return apiInternalError(c, deps, "export: create deliverable", err)
			}
			created = append(created, fiber.Map{
				"deliverableId": d.DeliverableID,
				"name":          d.Name,
				"sizeBytes":     d.SizeBytes,
				"status":        d.Status,
			})
		}

		deps.Log.Info("account: export created",
			slog.String("userId", userID), slog.Int("items", len(items)), slog.Int("parts", len(parts)))
		return c.Status(fiber.StatusCreated).JSON(fiber.Map{
			"deliverables": created,
			"itemCount":    len(items),
			"message":      "Export ready in your Download Center.",
		})
	}
}

// marshalExportParts renders the export as one or more standalone JSON
// documents, each below the deliverables content cap. Every part is a
// complete valid JSON object carrying its own part/of envelope.
func marshalExportParts(userID string, now time.Time, items []map[string]any) ([][]byte, error) {
	type envelope struct {
		ExportedAt string           `json:"exportedAt"`
		UserID     string           `json:"userId"`
		Part       int              `json:"part"`
		Of         int              `json:"of"`
		Items      []map[string]any `json:"items"`
	}

	// Greedy chunking on the marshalled size of each item.
	var chunks [][]map[string]any
	var cur []map[string]any
	curSize := 0
	for _, it := range items {
		b, err := json.Marshal(it)
		if err != nil {
			return nil, fmt.Errorf("marshal export item: %w", err)
		}
		if curSize+len(b) > exportPartTarget && len(cur) > 0 {
			chunks = append(chunks, cur)
			cur, curSize = nil, 0
		}
		cur = append(cur, it)
		curSize += len(b) + 1
	}
	chunks = append(chunks, cur) // final chunk (possibly empty: still a valid export)

	out := make([][]byte, 0, len(chunks))
	for i, ch := range chunks {
		if ch == nil {
			ch = []map[string]any{}
		}
		b, err := json.Marshal(envelope{
			ExportedAt: now.Format(time.RFC3339),
			UserID:     userID,
			Part:       i + 1,
			Of:         len(chunks),
			Items:      ch,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal export part: %w", err)
		}
		out = append(out, b)
	}
	return out, nil
}

// ---- DELETE /api/v1/account ----

func handleDeleteAccount(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)

		var body struct {
			// Confirm must be the literal string "DELETE" — a typed
			// confirmation guard on an irreversible destructive action.
			Confirm string `json:"confirm"`
		}
		// Body is required; a bare DELETE with no confirmation is rejected.
		if err := c.BodyParser(&body); err != nil || body.Confirm != "DELETE" {
			return errorJSON(c, fiber.StatusBadRequest, "confirmation_required",
				`Account deletion is irreversible. Send {"confirm":"DELETE"} to proceed.`)
		}

		// Re-auth check: never act on the (up to 15-minute-old) JWT alone —
		// re-read the profile fresh from the store.
		u, err := deps.Store.GetUser(c.Context(), userID)
		if err != nil {
			return apiInternalError(c, deps, "delete account: get user", err)
		}
		if u == nil {
			// Profile already gone — a retried delete. Idempotent success.
			return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"status": "deleting"})
		}
		if u.Role == store.RoleOwner {
			// The owner account anchors the deployment (CONFIG/OWNER
			// binding, allowlist administration). Purging it would brick
			// the stack — decommission the whole stack instead.
			return errorJSON(c, fiber.StatusConflict, "owner_undeletable",
				"The owner account cannot be deleted; decommission the deployment instead.")
		}
		if u.Status == store.UserStatusDeleting {
			return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"status": "deleting"})
		}
		if u.Status != store.UserStatusActive {
			return errorJSON(c, fiber.StatusForbidden, "forbidden", "Your account is not in a state that allows deletion.")
		}

		fn := os.Getenv("ACCOUNT_PURGE_FUNCTION_NAME")
		if fn == "" || deps.Lambda == nil {
			return errorJSON(c, fiber.StatusServiceUnavailable, "not_configured", "account deletion is not configured")
		}

		// 1) Flip the profile to deleting (blocks new session mints/tool
		// calls via every fresh-status re-check) …
		if err := deps.Store.SetUserStatus(c.Context(), userID, store.UserStatusDeleting); err != nil &&
			!errors.Is(err, store.ErrNotFound) {
			return apiInternalError(c, deps, "delete account: mark deleting", err)
		}
		// 2) … kill every outstanding JWT across every surface (the purge
		// deletes the refresh rows themselves) …
		if err := deps.Store.SetTokensValidAfter(c.Context(), userID, time.Now().Unix()); err != nil &&
			!errors.Is(err, store.ErrNotFound) {
			deps.Log.Error("account: bump tokensValidAfter failed",
				slog.String("error", err.Error()), slog.String("userId", userID))
			// Continue: the purge removes the sessions regardless.
		}

		// 3) … then hand off to the purge Lambda asynchronously.
		payload, err := json.Marshal(accountPurgeEvent{
			UserID:       userID,
			Email:        u.Email,
			AmazonUserID: u.AmazonUserID,
			RequestedAt:  time.Now().UTC().Format(time.RFC3339),
		})
		if err != nil {
			return apiInternalError(c, deps, "delete account: marshal purge event", err)
		}
		if _, err := deps.Lambda.Invoke(c.Context(), &lambda.InvokeInput{
			FunctionName:   aws.String(fn),
			InvocationType: lambdatypes.InvocationTypeEvent,
			Payload:        payload,
		}); err != nil {
			// Status stays "deleting" (fail closed for the account) — the
			// caller can retry the DELETE, which re-invokes idempotently.
			deps.Log.Error("account: purge invoke failed",
				slog.String("error", err.Error()), slog.String("userId", userID))
			return errorJSON(c, fiber.StatusBadGateway, "purge_unavailable",
				"Deletion was recorded but the purge worker could not be reached; retry to re-trigger it.")
		}

		deps.Log.Info("account: deletion requested", slog.String("userId", userID))
		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
			"status":  "deleting",
			"message": "Your account and data are being deleted. A confirmation email will follow.",
		})
	}
}
