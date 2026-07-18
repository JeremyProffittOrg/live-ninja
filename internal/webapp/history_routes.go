// Conversation history + topic taxonomy routes (M11, FR-TOP-02/03/04/05),
// the API behind the /history page (web/static/js/history.mjs) and the
// Android History screen (HistoryDtos.kt):
//
//   - GET   /api/v1/conversations       — filterable history list
//     (?topic=&device=&from=&to=&cursor=&limit=), newest first.
//   - GET   /api/v1/conversations/:id   — one conversation + its LOG#
//     transcript turns.
//   - GET   /api/v1/topics              — the caller's topic taxonomy
//     (populates the filter chips / pickers — never a blind text box).
//   - POST  /api/v1/topics              — create a topic.
//   - PATCH  /api/v1/topics/:id         — rename / recolor / archive /
//     merge (mergedInto). topicId is stable, so rename/recolor/archive
//     never re-tag a conversation (FR-TOP-02); merge rewrites the TREF
//     refs + CONV topicIds via store.MergeTopics.
//   - DELETE /api/v1/topics/:id         — remove a topic and every TREF
//     ref under it (store.DeleteTopic). Conversations are left untouched;
//     see DeleteTopic's doc comment for the "filtered on read, not
//     rewritten" rationale.
//
// Thin HTTP layer over internal/store (topics.go): the CONV#/TREF#/TOPIC#
// item shapes, the Query-only filter mapping (topic → TREF sort range,
// date → sk BETWEEN bounds, device → FilterExpression; deliberately NO new
// GSIs), and the opaque cursor all live there — one canonical
// implementation shared with cmd/topics-extract.
//
// Conversation ids on the wire are store.Conversation.ConvID()
// ("<ts>#<sessionId>", the sk minus its CONV# prefix). Both clients
// URL-encode path ids, so the '#' is safe; the detail route additionally
// accepts a bare sessionId (bounded newest-first resolve) for callers that
// only kept the realtime sessionId.
package webapp

import (
	"context"
	"errors"
	"hash/fnv"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// convResolveMaxPages bounds the bare-sessionId fallback resolve
// (newest-first pages of 100 → the most recent ~2000 conversations).
const convResolveMaxPages = 20

// topicColorPalette is the fixed set new topics draw from when the client
// doesn't pick one (and the palette the UI's color picker presents).
var topicColorPalette = []string{
	"#2563eb", "#16a34a", "#d97706", "#dc2626",
	"#7c3aed", "#0891b2", "#be185d", "#4d7c0f",
}

var hexColorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// ---- wire projections ----

func conversationJSON(cv *store.Conversation) fiber.Map {
	topicIDs := cv.TopicIDs
	if topicIDs == nil {
		topicIDs = []string{}
	}
	out := fiber.Map{
		"id":             cv.ConvID(),
		"conversationId": cv.ConvID(), // alias — lenient client normalizers
		"sessionId":      cv.SessionID,
		"ts":             cv.TS,
		"startedAt":      cv.TS, // alias
		"topicIds":       topicIDs,
		"turnCount":      cv.TurnCount,
	}
	for k, v := range map[string]string{
		"deviceId": cv.DeviceID,
		"engine":   cv.Engine,
		"surface":  cv.Surface,
		"title":    cv.Title,
	} {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

func topicJSON(t *store.Topic) fiber.Map {
	out := fiber.Map{
		"id":        t.TopicID,
		"topicId":   t.TopicID, // alias
		"name":      t.Name,
		"color":     t.Color,
		"archived":  t.Archived,
		"convCount": t.ConvCount,
		"createdAt": t.CreatedAt,
	}
	if t.MergedInto != "" {
		out["mergedInto"] = t.MergedInto
	}
	return out
}

// ---- registrar ----

// RegisterHistoryRoutes mounts the M11 conversation-history + topic API.
// Called from cmd/web/main.go behind the app-wide ExtractAuthContext/
// CSRFProtect middleware; every route re-derives identity from the auth
// Locals and is fail-closed via RequireAuth (same posture as
// api_routes.go).
func RegisterHistoryRoutes(app *fiber.App, deps *Deps) {
	api := app.Group("/api/v1", RequireAuth())

	api.Get("/conversations", handleListConversations(deps))
	api.Get("/conversations/:id", handleGetConversation(deps))

	api.Get("/topics", handleListTopics(deps))
	api.Post("/topics", handleCreateTopic(deps))
	api.Patch("/topics/:id", handlePatchTopic(deps))
	api.Delete("/topics/:id", handleDeleteTopic(deps))
}

// ---- GET /api/v1/conversations ----

// parseHistoryTime accepts YYYY-MM-DD or RFC3339 and returns the RFC3339
// UTC string handed to store.ListConversations (which uses it as a
// lexicographic sk bound — RFC3339 UTC order IS chronological order).
// Date-only values expand to the start of that UTC day; endOfDay selects
// the start of the NEXT day so a date-only ?to= is inclusive (the store's
// upper bound already covers everything under an exact-timestamp bound).
func parseHistoryTime(raw string, endOfDay bool) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", true
	}
	if d, err := time.Parse("2006-01-02", raw); err == nil {
		if endOfDay {
			// The store appends its high sentinel to the To bound, so the
			// prefix "…-17T" form keeps every timestamp on that day in
			// range without spilling into the next day.
			return d.UTC().Format("2006-01-02") + "T", true
		}
		return d.UTC().Format(time.RFC3339), true
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC().Format(time.RFC3339), true
	}
	return "", false
}

func handleListConversations(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)

		topicID := c.Query("topic")
		if topicID != "" && !resourceIDPattern.MatchString(topicID) {
			return apiBadRequest(c, "topic must be a valid topic id")
		}
		deviceFilter := c.Query("device")
		if len(deviceFilter) > 128 {
			return apiBadRequest(c, "device is invalid")
		}
		from, okFrom := parseHistoryTime(c.Query("from"), false)
		if !okFrom {
			return apiBadRequest(c, "from must be YYYY-MM-DD or RFC3339")
		}
		to, okTo := parseHistoryTime(c.Query("to"), true)
		if !okTo {
			return apiBadRequest(c, "to must be YYYY-MM-DD or RFC3339")
		}

		convs, next, err := deps.Store.ListConversations(c.Context(), userID, store.ListConversationsOpts{
			TopicID:  topicID,
			DeviceID: deviceFilter,
			From:     from,
			To:       to,
			Limit:    int32(queryLimit(c, 25, 50)),
			Cursor:   c.Query("cursor"),
		})
		if err != nil {
			if strings.Contains(err.Error(), "invalid cursor") {
				return apiBadRequest(c, "cursor is invalid (it must come from a previous page of the same filter)")
			}
			return apiInternalError(c, deps, "list conversations", err)
		}

		items := make([]fiber.Map, 0, len(convs))
		for i := range convs {
			items = append(items, conversationJSON(&convs[i]))
		}
		resp := fiber.Map{"items": items}
		if next != "" {
			resp["nextCursor"] = next
		}
		return c.JSON(resp)
	}
}

// ---- GET /api/v1/conversations/:id ----

// resolveConversation turns the route id into the canonical CONV record.
// Preferred form: the ConvID the list returned ("<ts>#<sessionId>" —
// direct GetItem). Fallback: a bare sessionId, resolved by walking the
// CONV# range newest-first (bounded; Query-only).
func resolveConversation(ctx context.Context, deps *Deps, userID, id string) (*store.Conversation, error) {
	if strings.Contains(id, "#") {
		return deps.Store.GetConversation(ctx, userID, id)
	}

	cursor := ""
	for page := 0; page < convResolveMaxPages; page++ {
		convs, next, err := deps.Store.ListConversations(ctx, userID, store.ListConversationsOpts{
			Limit:  100,
			Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		for i := range convs {
			if convs[i].SessionID == id {
				return &convs[i], nil
			}
		}
		if next == "" {
			return nil, nil
		}
		cursor = next
	}
	return nil, nil
}

func handleGetConversation(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)

		id := c.Params("id")
		if unescaped, err := url.PathUnescape(id); err == nil {
			id = unescaped // clients URL-encode the '#' in ConvID
		}
		if id == "" || len(id) > 256 {
			return apiBadRequest(c, "conversation id is invalid")
		}

		conv, err := resolveConversation(c.Context(), deps, userID, id)
		if err != nil {
			return apiInternalError(c, deps, "resolve conversation", err)
		}
		if conv == nil {
			return apiNotFound(c)
		}

		// Transcript turns from the LOG#<sessionId># range the transcript
		// sink wrote (api_routes.go handleTranscript), seq order. The
		// broker's seq-0 role=system session-start marker is skipped —
		// it's bookkeeping, not conversation.
		raw, err := deps.Store.ListSessionTurns(c.Context(), userID, conv.SessionID)
		if err != nil {
			return apiInternalError(c, deps, "list session turns", err)
		}
		turns := make([]fiber.Map, 0, len(raw))
		for i := range raw {
			if raw[i].Role == "system" {
				continue
			}
			turn := fiber.Map{"role": raw[i].Role, "text": raw[i].Text}
			if raw[i].TS != "" {
				turn["ts"] = raw[i].TS
			}
			if raw[i].Engine != "" {
				turn["engine"] = raw[i].Engine
			}
			if raw[i].Surface != "" {
				turn["surface"] = raw[i].Surface
			}
			turns = append(turns, turn)
		}

		resp := conversationJSON(conv)
		resp["turns"] = turns
		return c.JSON(resp)
	}
}

// ---- GET /api/v1/topics ----

func handleListTopics(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)

		topics, err := deps.Store.ListTopics(c.Context(), userID)
		if err != nil {
			return apiInternalError(c, deps, "list topics", err)
		}

		includeArchived := c.QueryBool("includeArchived", true)
		sort.Slice(topics, func(i, j int) bool {
			return strings.ToLower(topics[i].Name) < strings.ToLower(topics[j].Name)
		})

		items := make([]fiber.Map, 0, len(topics))
		for i := range topics {
			if !includeArchived && topics[i].Archived {
				continue
			}
			items = append(items, topicJSON(&topics[i]))
		}
		return c.JSON(fiber.Map{"items": items, "palette": topicColorPalette})
	}
}

// ---- POST /api/v1/topics ----

func handleCreateTopic(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)

		var body struct {
			Name  string `json:"name"`
			Color string `json:"color"`
		}
		if err := c.BodyParser(&body); err != nil {
			return apiBadRequest(c, "invalid JSON body")
		}
		name := strings.TrimSpace(body.Name)
		if name == "" || len(name) > 64 {
			return apiBadRequest(c, "name must be a non-empty string of at most 64 characters")
		}
		if body.Color != "" && !hexColorPattern.MatchString(body.Color) {
			return apiBadRequest(c, "color must be a #RRGGBB hex value")
		}

		// Reject case-insensitive duplicates among live topics so the
		// taxonomy stays clean (the extractor also maps by name).
		existing, err := deps.Store.ListTopics(c.Context(), userID)
		if err != nil {
			return apiInternalError(c, deps, "list topics for duplicate check", err)
		}
		for i := range existing {
			if strings.EqualFold(existing[i].Name, name) && !existing[i].Archived {
				// Extends the canonical envelope with a sibling `topicId` so
				// the client can offer "use the existing topic" without a
				// second round trip; errorJSON's fixed signature has no room
				// for that extra field, so the envelope + logging are built
				// the same way it does, by hand, here.
				const code, message = "duplicate_topic", "A topic with that name already exists."
				requestLogger(c).Error("request failed",
					slog.Int("status", fiber.StatusConflict),
					slog.String("code", code),
					slog.String("message", message),
					slog.String("method", c.Method()),
					slog.String("path", c.Path()),
				)
				return c.Status(fiber.StatusConflict).JSON(fiber.Map{
					"error":   ErrorBody{Code: code, Message: message, TxID: TxID(c)},
					"topicId": existing[i].TopicID,
				})
			}
		}

		color := body.Color
		if color == "" {
			h := fnv.New32a()
			_, _ = h.Write([]byte(strings.ToLower(name)))
			color = topicColorPalette[int(h.Sum32())%len(topicColorPalette)]
		}

		t := &store.Topic{
			TopicID: uuid.NewString(),
			Name:    name,
			Color:   color,
		}
		if err := deps.Store.CreateTopic(c.Context(), userID, t); err != nil {
			return apiInternalError(c, deps, "create topic", err)
		}
		return c.Status(fiber.StatusCreated).JSON(topicJSON(t))
	}
}

// ---- PATCH /api/v1/topics/:id ----

func handlePatchTopic(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)
		topicID := c.Params("id")
		if !resourceIDPattern.MatchString(topicID) {
			return apiBadRequest(c, "topic id is invalid")
		}

		var body struct {
			Name       *string `json:"name"`
			Color      *string `json:"color"`
			Archived   *bool   `json:"archived"`
			MergedInto *string `json:"mergedInto"`
		}
		if err := c.BodyParser(&body); err != nil {
			return apiBadRequest(c, "invalid JSON body")
		}
		if body.Name == nil && body.Color == nil && body.Archived == nil && body.MergedInto == nil {
			return apiBadRequest(c, "provide at least one of name, color, archived, mergedInto")
		}
		if body.Name != nil {
			trimmed := strings.TrimSpace(*body.Name)
			if trimmed == "" || len(trimmed) > 64 {
				return apiBadRequest(c, "name must be a non-empty string of at most 64 characters")
			}
			body.Name = &trimmed
		}
		if body.Color != nil && !hexColorPattern.MatchString(*body.Color) {
			return apiBadRequest(c, "color must be a #RRGGBB hex value")
		}

		// Merge first (FR-TOP-02): store.MergeTopics rewrites the TREF refs
		// and CONV topicIds to the destination, marks the source archived +
		// mergedInto, and keeps every id stable. Rename/recolor updates can
		// ride along in the same PATCH.
		if body.MergedInto != nil && strings.TrimSpace(*body.MergedInto) != "" {
			dst := strings.TrimSpace(*body.MergedInto)
			if !resourceIDPattern.MatchString(dst) {
				return apiBadRequest(c, "mergedInto must be a valid topic id")
			}
			if dst == topicID {
				return apiBadRequest(c, "a topic cannot be merged into itself")
			}
			if err := deps.Store.MergeTopics(c.Context(), userID, topicID, dst); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return apiNotFound(c)
				}
				return apiInternalError(c, deps, "merge topics", err)
			}
		}

		if body.Name != nil || body.Color != nil || body.Archived != nil {
			err := deps.Store.UpdateTopic(c.Context(), userID, topicID, store.TopicUpdate{
				Name:     body.Name,
				Color:    body.Color,
				Archived: body.Archived,
			})
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return apiNotFound(c)
				}
				return apiInternalError(c, deps, "update topic", err)
			}
		}

		t, err := deps.Store.GetTopic(c.Context(), userID, topicID)
		if err != nil {
			return apiInternalError(c, deps, "get topic after patch", err)
		}
		if t == nil {
			return apiNotFound(c)
		}
		return c.JSON(topicJSON(t))
	}
}

// ---- DELETE /api/v1/topics/:id ----

func handleDeleteTopic(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)
		topicID := c.Params("id")
		if !resourceIDPattern.MatchString(topicID) {
			return apiBadRequest(c, "topic id is invalid")
		}

		if err := deps.Store.DeleteTopic(c.Context(), userID, topicID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return apiNotFound(c)
			}
			return apiInternalError(c, deps, "delete topic", err)
		}
		return c.JSON(fiber.Map{"ok": true})
	}
}
