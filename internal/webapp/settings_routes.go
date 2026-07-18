// Settings surface (M3 subset of the M6 FR-S01 contract), owned by the
// WS-D settings workstream per docs/web-ui-spec.md §3/§4:
//
//   - GET  /settings                  — SSR settings page (pages/settings
//     template, current document inlined server-side per spec §3.2 — no
//     client fetch on first paint).
//   - GET  /api/v1/settings           — canonical JSON document (defaults
//     synthesized when absent; voice default cedar).
//   - PUT  /api/v1/settings           — {settings, version} optimistic-
//     concurrency write; stale version → 409 version_conflict.
//   - GET  /api/v1/realtime/voices    — static realtime.SupportedVoices
//     catalog (populates the voice pickers — never a blind text box).
//   - GET  /api/v1/realtime/personas  — static persona catalog (IDs and
//     display copy only; instruction text never leaves the server).
//
// RegisterSettingsRoutes is called from cmd/web/main.go alongside
// RegisterAuthRoutes/RegisterAPIRoutes, behind the same global
// ExtractAuthContext + CSRFProtect middleware (the PUT is CSRF-checked
// there, not here). The /api/v1 routes are additionally fail-closed via
// RequireAuth, mirroring api_routes.go's defense-in-depth posture.
package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	lnsync "github.com/JeremyProffittOrg/live-ninja/internal/sync"
)

// RegisterSettingsRoutes mounts the settings page and the settings/
// catalog JSON API.
func RegisterSettingsRoutes(app *fiber.App, deps *Deps) {
	app.Get("/settings", handleSettingsPage(deps))

	api := app.Group("/api/v1", RequireAuth())
	api.Get("/settings", handleGetSettings(deps))
	api.Put("/settings", handlePutSettings(deps))
	api.Get("/realtime/voices", handleListVoices())
	api.Get("/realtime/personas", handleListPersonas())
}

// ---- GET /settings (SSR page) ----

// settingsPageView is the template bind for pages/settings. SettingsJSON
// is the full canonical document (json.Marshal HTML-escapes <,>,& so it
// is safe to emit raw inside the <script type="application/json"> data
// island); the typed fields drive the server-rendered initial control
// states so the page needs no client fetch (and shows no skeleton) on
// first paint — spec §3.2.
type settingsPageView struct {
	SettingsJSON template.JS
	CatalogsJSON template.JS // {voices:[...], personas:[...]} for JS re-renders

	// ThemeAttr is read by layouts/base.html's themeAttr func (reflection
	// over the bind): "light"/"dark" SSRs data-theme so an explicit theme
	// choice paints right even before theme.js/settings.mjs run; ""
	// (system) leaves the attribute off → prefers-color-scheme rules.
	ThemeAttr string

	Personas []settingsPersonaRow

	WakeWord        string
	WakeEngine      string
	SensitivityPct  int
	PersonaPreset   string
	IsCustomPersona bool
	Instructions    string
	InstructionsLen int
	TurnDetection   string
	MicEagerness    string
	AppStyle        string // style zone: everything outside the live panel
	LiveStyle       string // style zone: the conversation page's live panel
	AccentColor     string
	Theme           string
	MicDeviceID     string
	StoreAudio      bool
	StoreTranscript bool
	RetentionDays   int
}

type settingsPersonaRow struct {
	realtime.PersonaInfo
	Selected bool
}

func handleSettingsPage(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)
		if userID == "" {
			// A plain browser navigation carries only the HttpOnly refresh
			// cookie (no Bearer header), so resolve the session from it —
			// read-only hash check, no rotation (rotation stays the
			// exclusive job of POST /auth/refresh).
			userID = resolveWebSessionUser(c, deps)
		}
		if userID == "" {
			return c.Redirect("/", fiber.StatusFound)
		}

		doc, err := deps.Store.GetSettings(c.Context(), userID)
		if err != nil {
			return apiInternalError(c, deps, "get settings for page", err)
		}

		view, err := buildSettingsPageView(doc)
		if err != nil {
			return apiInternalError(c, deps, "build settings view", err)
		}

		// HTML documents are never cached (spec §0); only fingerprinted
		// /static/* assets are immutable.
		c.Set(fiber.HeaderCacheControl, "no-cache")
		// Renders through the shared Views engine (pages_routes.go
		// Renderer: layouts/base + partials/nav + this page's file).
		return c.Render("pages/settings", view)
	}
}

// resolveWebSessionUser validates the web refresh cookie against its
// session row (same hash discipline as authRoutes.logout: the presented
// secret must match the current or immediately-previous hash) and
// returns the user id when the session and account are still live.
// Returns "" for anything less than a fully-valid session — the caller
// treats that as unauthenticated.
func resolveWebSessionUser(c *fiber.Ctx, deps *Deps) string {
	sid, secret, ok := splitWireRefresh(c.Cookies(RefreshCookieName))
	if !ok {
		return ""
	}
	sess, err := deps.Store.GetSessionByID(c.Context(), sid)
	if err != nil || sess == nil {
		return ""
	}
	now := time.Now().Unix()
	if sess.ExpiresAt > 0 && sess.ExpiresAt < now {
		return ""
	}
	h := auth.HashRefreshToken(secret)
	if !subtleEquals(h, sess.RefreshHash) && !subtleEquals(h, sess.PrevHash) {
		return ""
	}
	user, err := deps.Store.GetUser(c.Context(), sess.UserID)
	if err != nil || user == nil || user.Status != store.UserStatusActive {
		return ""
	}
	if user.TokensValidAfter > 0 && sess.CreatedAt > 0 && sess.CreatedAt < user.TokensValidAfter {
		return ""
	}
	return sess.UserID
}

func buildSettingsPageView(doc map[string]any) (*settingsPageView, error) {
	docJSON, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal settings doc: %w", err)
	}
	catalogs, err := json.Marshal(fiber.Map{
		"voices":   realtime.SupportedVoices,
		"accents":  realtime.SupportedAccents,
		"personas": realtime.ListPersonas(),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal catalogs: %w", err)
	}

	v := &settingsPageView{
		SettingsJSON:    template.JS(docJSON),
		CatalogsJSON:    template.JS(catalogs),
		WakeWord:        docString(doc, "wakeWord", "hey-live-ninja"),
		WakeEngine:      docString(doc, "wakeEngine", "openwakeword"),
		SensitivityPct:  int(math.Round(docFloat(doc, "sensitivity", 0.5) * 100)),
		TurnDetection:   docString(doc, "turnDetection", "semantic_vad"),
		MicEagerness:    docString(doc, "micEagerness", "auto"),
		AppStyle:        docNestedString(doc, "appearance", "appStyle", "ninja"),
		LiveStyle:       docNestedString(doc, "appearance", "liveStyle", "hal9000"),
		AccentColor:     docNestedString(doc, "appearance", "accentColor", ""),
		Theme:           docString(doc, "theme", "system"),
		MicDeviceID:     docString(doc, "micDeviceId", ""),
		StoreAudio:      docBool(doc, "privacy", "storeAudio", false),
		StoreTranscript: docBool(doc, "privacy", "storeTranscripts", true),
		RetentionDays:   int(docNestedFloat(doc, "privacy", "retentionDays", 30)),
	}
	if v.Theme == "light" || v.Theme == "dark" {
		v.ThemeAttr = v.Theme
	}

	if persona, ok := doc["persona"].(map[string]any); ok {
		v.PersonaPreset = strOr(persona["presetId"], "default")
		v.Instructions = strOr(persona["systemInstructions"], "")
	} else {
		v.PersonaPreset = "default"
	}
	v.IsCustomPersona = v.PersonaPreset == "custom"
	v.InstructionsLen = len([]rune(v.Instructions))

	// Voice + accent no longer render on this page: personas are the unit
	// of voice identity (personaPrefs, edited in the conversation page's
	// persona editor). The stored top-level voice/voiceAccent remain in the
	// SettingsJSON island purely so write-backs preserve the fallback
	// default; CatalogsJSON keeps shipping the voice/accent catalogs for
	// client-side consumers.
	for _, p := range realtime.ListPersonas() {
		v.Personas = append(v.Personas, settingsPersonaRow{PersonaInfo: p, Selected: p.ID == v.PersonaPreset})
	}
	return v, nil
}

// ---- GET /api/v1/settings[?since=<v>] ----

// Without `since` the response is the bare canonical document
// (unchanged M3 behavior). With `?since=<v>` this is the M6
// reconciliation fetch (contracts/api.md): the poll-based fan-out for
// web (30s + visibilitychange/focus) and Android (foreground +
// wake-service 15-min tick) — the locked M6 decision replacing the
// WebSocket/FCM push sketch (no Firebase account; no WebSocket API).
// `{changed:false, version}` is the cheap steady-state answer;
// `{changed:true, version, settings}` delivers the newer document.
func handleGetSettings(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		doc, err := deps.Store.GetSettings(c.Context(), UserID(c))
		if err != nil {
			return apiInternalError(c, deps, "get settings", err)
		}

		if sinceRaw := c.Query("since"); sinceRaw != "" {
			since, perr := strconv.ParseInt(sinceRaw, 10, 64)
			if perr != nil || since < 0 {
				return apiBadRequest(c, "since must be a non-negative integer (the version you last saw)")
			}
			cur := lnsync.DocVersion(doc)
			if cur <= since {
				// Fast path: nothing newer than what the caller holds.
				// (cur < since can only mean the caller is from the
				// future/confused — it still gets changed:false plus the
				// authoritative version so it can re-sync with a plain GET.)
				return c.JSON(fiber.Map{"changed": false, "version": cur})
			}
			return c.JSON(fiber.Map{"changed": true, "version": cur, "settings": doc})
		}
		return c.JSON(doc)
	}
}

// ---- PUT /api/v1/settings ----

func handlePutSettings(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)

		var body struct {
			Settings map[string]any `json:"settings"`
			Version  int64          `json:"version"`
		}
		if err := c.BodyParser(&body); err != nil {
			return apiBadRequest(c, "invalid JSON body")
		}
		if body.Settings == nil {
			return apiBadRequest(c, "settings object is required")
		}
		if body.Version < 1 {
			return apiBadRequest(c, "version must be a positive integer (the version you last read)")
		}

		if msg := validateAndNormalizeSettings(body.Settings); msg != "" {
			return apiBadRequest(c, msg)
		}

		newVersion, err := deps.Store.PutSettings(c.Context(), userID, body.Settings, body.Version)
		if err != nil {
			if errors.Is(err, store.ErrVersionConflict) {
				return errorJSON(c, fiber.StatusConflict, "version_conflict",
					"Your settings were changed from another device. Re-read and re-apply.")
			}
			return apiInternalError(c, deps, "put settings", err)
		}

		body.Settings["version"] = newVersion

		// M6 fan-out: push the committed document to the user's M5Stack
		// devices as IoT shadow desired state (the only real-push surface
		// — web/Android reconcile via ?since polling, per the locked M6
		// decisions documented in internal/sync). Best-effort by design:
		// the write is already committed, and a fan-out failure must
		// never turn a successful PUT into an error — offline devices
		// converge through the shadow's persistence / their next poll.
		publishSettingsShadow(c.Context(), deps, userID, body.Settings, newVersion)

		return c.JSON(fiber.Map{"settings": body.Settings, "version": newVersion})
	}
}

// publishSettingsShadow publishes the freshly-written settings document
// as the `config` shadow desired state for every ACTIVE IoT-provisioned
// device of the user (contracts/shadow.md; internal/sync). Declared as a
// package var so tests can intercept the fan-out without IoT clients.
// hexColorRe validates appearance.accentColor ("#rrggbb").
var hexColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

var publishSettingsShadow = func(ctx context.Context, deps *Deps, userID string, doc map[string]any, version int64) {
	pub, err := lnsync.SharedPublisher(ctx, deps.Log)
	if err != nil {
		deps.Log.Warn("settings: shadow publisher unavailable; skipping device fan-out",
			"error", err.Error())
		return
	}
	if err := pub.PublishDesired(ctx, deps.Store, userID, doc, version); err != nil {
		deps.Log.Warn("settings: shadow desired fan-out failed",
			"userId", userID, "error", err.Error())
	}
}

// validateAndNormalizeSettings checks the known schema fields' types and
// bounds in place, returning "" when valid or a human-readable error.
// Unknown fields pass through untouched (additionalProperties:true /
// forward-compat preservation, contracts/README.md rule 2). Closed
// platform-behavior enums (turnDetection, theme, wakeEngine,
// retentionDays, voiceEngine values) are enforced; `voice` (and every
// personaPrefs voice/accent) is deliberately lenient beyond
// non-empty-string (new voices append to the enum in later milestones and
// an unknown value must be preserved, per the schema — the broker's
// ResolveSessionVoice chain already falls through safely).
func validateAndNormalizeSettings(doc map[string]any) string {
	delete(doc, "version") // server-owned; PutSettings sets it

	if s, ok := doc["wakeWord"].(string); !ok || strings.TrimSpace(s) == "" || len(s) > 128 {
		return "wakeWord must be a non-empty catalog id"
	}
	if s, ok := doc["wakeEngine"].(string); !ok || !oneOf(s, "openwakeword", "porcupine", "wakenet") {
		return "wakeEngine must be one of openwakeword, porcupine, wakenet"
	}
	sens, ok := numberVal(doc["sensitivity"])
	if !ok || sens < 0 || sens > 1 {
		return "sensitivity must be a number between 0 and 1"
	}

	persona, ok := doc["persona"].(map[string]any)
	if !ok {
		return "persona must be an object with presetId"
	}
	presetID, ok := persona["presetId"].(string)
	if !ok || strings.TrimSpace(presetID) == "" || len(presetID) > 64 {
		return "persona.presetId must be a non-empty string"
	}
	switch si := persona["systemInstructions"].(type) {
	case nil:
		// fine (null / absent)
	case string:
		if len([]rune(si)) > 4000 {
			return "persona.systemInstructions must be at most 4000 characters"
		}
	default:
		return "persona.systemInstructions must be a string or null"
	}
	if presetID != "custom" {
		// Instructions are only meaningful for the custom persona
		// (schema); normalize to null so a preset switch can't smuggle
		// stale instruction text along.
		persona["systemInstructions"] = nil
	}

	if s, ok := doc["voice"].(string); !ok || strings.TrimSpace(s) == "" || len(s) > 64 {
		return "voice must be a non-empty voice id"
	}
	// voiceAccent: speech-accent directive id from the accents catalog.
	// Optional for older clients — absent/null normalizes to "" (none), and
	// the catalog's "none" id normalizes to its stored form "". Like
	// `voice`, unknown ids are preserved rather than rejected (additive
	// accent-catalog growth; the broker's AccentDirective already mints
	// unknown values without an accent).
	switch a := doc["voiceAccent"].(type) {
	case nil:
		doc["voiceAccent"] = ""
	case string:
		if a == "none" {
			doc["voiceAccent"] = ""
		} else if len(a) > 64 {
			return "voiceAccent must be an accent id of at most 64 characters"
		}
	default:
		return "voiceAccent must be a string"
	}
	// personaPrefs: per-persona voice identity map {personaId: {voice,
	// accent, updatedAt}} — personas are the unit of voice identity; the
	// top-level voice/voiceAccent above are only the account-wide fallback.
	// Validation mirrors the voice/voiceAccent posture (lenient — unknown
	// ids preserved; the broker's chain falls through safely), entries keep
	// their unknown fields (rule 2), and the map is capped at
	// maxPersonaPrefs with the oldest-updated entries pruned first.
	switch pp := doc["personaPrefs"].(type) {
	case nil:
		doc["personaPrefs"] = map[string]any{}
	case map[string]any:
		for id, raw := range pp {
			if strings.TrimSpace(id) == "" || len(id) > 128 {
				return "personaPrefs keys must be non-empty persona ids of at most 128 characters"
			}
			entry, ok := raw.(map[string]any)
			if !ok {
				return "personaPrefs[" + id + "] must be an object"
			}
			if v, present := entry["voice"]; present {
				if s, ok := v.(string); !ok || len(s) > 64 {
					return "personaPrefs[" + id + "].voice must be a voice id of at most 64 characters"
				}
			}
			if a, present := entry["accent"]; present {
				s, ok := a.(string)
				if !ok || len(s) > 64 {
					return "personaPrefs[" + id + "].accent must be an accent id of at most 64 characters"
				}
				if s == "none" {
					// Same normalization as voiceAccent: the catalog's "none"
					// id stores as "" (explicitly no accent).
					entry["accent"] = ""
				}
			}
			if u, present := entry["updatedAt"]; present {
				if s, ok := u.(string); !ok || len(s) > 64 {
					return "personaPrefs[" + id + "].updatedAt must be an RFC3339 timestamp string"
				}
			}
		}
		prunePersonaPrefs(pp, maxPersonaPrefs)
	default:
		return "personaPrefs must be an object"
	}
	if s, ok := doc["turnDetection"].(string); !ok || !oneOf(s, "semantic_vad", "server_vad") {
		return "turnDetection must be semantic_vad or server_vad"
	}
	// micEagerness: how quickly semantic VAD decides the user finished a
	// turn. Optional for older clients — absent normalizes to auto.
	switch e := doc["micEagerness"].(type) {
	case nil:
		doc["micEagerness"] = "auto"
	case string:
		if !oneOf(e, "low", "medium", "high", "auto") {
			return "micEagerness must be one of low, medium, high, auto"
		}
	default:
		return "micEagerness must be a string"
	}
	// appearance: two style zones (appStyle = everything outside the live
	// panel, liveStyle = the conversation page's orb/mic panel) + a global
	// accent color ("" = each zone's style default). Optional for older
	// clients — absent normalizes to the defaults; a legacy single
	// themeStyle (pre-split clients / cached bundles) migrates to liveStyle.
	switch ap := doc["appearance"].(type) {
	case nil:
		doc["appearance"] = map[string]any{"appStyle": "ninja", "liveStyle": "hal9000", "accentColor": ""}
	case map[string]any:
		if ts, ok := ap["themeStyle"].(string); ok && ts != "" {
			if _, has := ap["liveStyle"]; !has {
				ap["liveStyle"] = ts
			}
		}
		delete(ap, "themeStyle")
		ls, _ := ap["liveStyle"].(string)
		if ls == "" {
			ap["liveStyle"] = "hal9000"
		} else if !oneOf(ls, "hal9000", "ninja", "minimal", "terminal") {
			return "appearance.liveStyle must be one of hal9000, ninja, minimal, terminal"
		}
		as, _ := ap["appStyle"].(string)
		if as == "" {
			ap["appStyle"] = "ninja"
		} else if !oneOf(as, "hal9000", "ninja", "minimal", "terminal") {
			return "appearance.appStyle must be one of hal9000, ninja, minimal, terminal"
		}
		switch ac := ap["accentColor"].(type) {
		case nil:
			ap["accentColor"] = ""
		case string:
			if ac != "" && !hexColorRe.MatchString(ac) {
				return "appearance.accentColor must be a #rrggbb hex color (or empty for the style default)"
			}
		default:
			return "appearance.accentColor must be a string"
		}
	default:
		return "appearance must be an object"
	}
	if s, ok := doc["theme"].(string); !ok || !oneOf(s, "light", "dark", "system") {
		return "theme must be light, dark, or system"
	}
	switch mic := doc["micDeviceId"].(type) {
	case nil:
	case string:
		if len(mic) > 256 {
			return "micDeviceId is too long"
		}
	default:
		return "micDeviceId must be a string or null"
	}

	ve, ok := doc["voiceEngine"].(map[string]any)
	if !ok {
		return "voiceEngine must be an object"
	}
	if s, ok := ve["default"].(string); !ok || !oneOf(s, "openai-realtime", "openai-realtime-mini", "nova-sonic") {
		return "voiceEngine.default must be one of openai-realtime, openai-realtime-mini, nova-sonic"
	}
	devices, ok := ve["devices"].(map[string]any)
	if !ok {
		return "voiceEngine.devices must be an object"
	}
	for id, pin := range devices {
		if s, ok := pin.(string); !ok || !oneOf(s, "openai-realtime", "openai-realtime-mini", "nova-sonic") {
			return "voiceEngine.devices[" + id + "] must be one of openai-realtime, openai-realtime-mini, nova-sonic"
		}
	}

	if privacy, present := doc["privacy"]; present {
		p, ok := privacy.(map[string]any)
		if !ok {
			return "privacy must be an object"
		}
		if v, present := p["storeAudio"]; present {
			if _, ok := v.(bool); !ok {
				return "privacy.storeAudio must be a boolean"
			}
		}
		if v, present := p["storeTranscripts"]; present {
			if _, ok := v.(bool); !ok {
				return "privacy.storeTranscripts must be a boolean"
			}
		}
		if v, present := p["retentionDays"]; present {
			n, ok := numberVal(v)
			if !ok || (n != 0 && n != 7 && n != 30 && n != 90) {
				return "privacy.retentionDays must be one of 0, 7, 30, 90"
			}
		}
	}
	return ""
}

// maxPersonaPrefs caps the personaPrefs map (contracts/settings.schema.json)
// so the settings item can never grow unbounded — a user cycling through
// hundreds of shared personas keeps only the ~200 most recently edited
// voice identities.
const maxPersonaPrefs = 200

// prunePersonaPrefs drops the oldest-updated entries until at most max
// remain. Entries without an updatedAt (or with a non-string one) count as
// oldest; RFC3339 UTC timestamps order correctly under plain string
// comparison, and ties break on the persona id for determinism.
func prunePersonaPrefs(pp map[string]any, max int) {
	if len(pp) <= max {
		return
	}
	type rec struct{ id, updatedAt string }
	recs := make([]rec, 0, len(pp))
	for id, raw := range pp {
		u := ""
		if entry, ok := raw.(map[string]any); ok {
			if s, ok := entry["updatedAt"].(string); ok {
				u = s
			}
		}
		recs = append(recs, rec{id: id, updatedAt: u})
	}
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].updatedAt != recs[j].updatedAt {
			return recs[i].updatedAt < recs[j].updatedAt
		}
		return recs[i].id < recs[j].id
	})
	for _, r := range recs[:len(recs)-max] {
		delete(pp, r.id)
	}
}

// ---- GET /api/v1/realtime/{voices,personas} (static catalogs) ----

func handleListVoices() fiber.Handler {
	return func(c *fiber.Ctx) error {
		// `accents` rides along in the same response (additive — existing
		// clients reading only `voices` are unaffected): the enumerated
		// accent-directive catalog backing the settings Accent picker.
		return c.JSON(fiber.Map{
			"voices":  realtime.SupportedVoices,
			"accents": realtime.SupportedAccents,
		})
	}
}

func handleListPersonas() fiber.Handler {
	return func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"personas": realtime.ListPersonas()})
	}
}

// ---- small doc accessors (map[string]any → typed with default) ----

func docString(doc map[string]any, key, def string) string {
	if s, ok := doc[key].(string); ok {
		return s
	}
	return def
}

func docFloat(doc map[string]any, key string, def float64) float64 {
	if n, ok := numberVal(doc[key]); ok {
		return n
	}
	return def
}

func docBool(doc map[string]any, objKey, key string, def bool) bool {
	if m, ok := doc[objKey].(map[string]any); ok {
		if b, ok := m[key].(bool); ok {
			return b
		}
	}
	return def
}

func docNestedString(doc map[string]any, objKey, key, def string) string {
	if m, ok := doc[objKey].(map[string]any); ok {
		if s, ok := m[key].(string); ok {
			return s
		}
	}
	return def
}

func docNestedFloat(doc map[string]any, objKey, key string, def float64) float64 {
	if m, ok := doc[objKey].(map[string]any); ok {
		if n, ok := numberVal(m[key]); ok {
			return n
		}
	}
	return def
}

func strOr(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

// numberVal normalizes the numeric shapes that reach a map[string]any
// from encoding/json (float64) and attributevalue (float64/int64).
func numberVal(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func oneOf(s string, opts ...string) bool {
	for _, o := range opts {
		if s == o {
			return true
		}
	}
	return false
}
