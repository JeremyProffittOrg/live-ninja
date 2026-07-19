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
	// Cost is a client-side list-price estimate persisted on session end;
	// absent on conversations that predate cost reporting (or whose engine
	// surfaces no usage) — zero is "unknown", so the field is omitted.
	if cv.CostUSD > 0 {
		out["costUsd"] = cv.CostUSD
		out["costTextTokens"] = cv.CostTextTokens
		out["costAudioTokens"] = cv.CostAudioTokens
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

// toolAuditPattern parses the audit line internal/tools writeAudit
// persists as a role=tool LOG# row:
//
//	tool=<name> outcome=<ok|error|duplicate> callId=<id> args=<json>[ error=<code>]
//
// (the line may be truncated at the router's maxAuditText cap, in which
// case only the raw text survives — parsing is best-effort).
var toolAuditPattern = regexp.MustCompile(`^tool=(\S+) outcome=(\S+) callId=(\S*) args=(.*)$`)

// toolAuditErrSuffix matches the " error=<code>" tail writeAudit appends
// on failed invocations (ToolError codes are snake_case identifiers).
var toolAuditErrSuffix = regexp.MustCompile(` error=([A-Za-z0-9_.-]+)$`)

// toolTurnJSON projects one role=tool audit row into a history tool-call
// entry: the raw audit text always ships (Android renders it as a plain
// bubble), plus the parsed tool/outcome/callId/args fields and the stored
// output snippet the web detail view renders as a tool card.
func toolTurnJSON(t *store.Turn) fiber.Map {
	entry := fiber.Map{"role": "tool", "text": t.Text}
	if t.TS != "" {
		entry["ts"] = t.TS
	}
	if t.Engine != "" {
		entry["engine"] = t.Engine
	}
	if t.Surface != "" {
		entry["surface"] = t.Surface
	}
	if t.Output != "" {
		entry["output"] = t.Output
	}
	m := toolAuditPattern.FindStringSubmatch(t.Text)
	if m == nil {
		return entry // truncated/legacy line — raw text only
	}
	args := m[4]
	if m[2] != "ok" {
		if em := toolAuditErrSuffix.FindStringSubmatch(args); em != nil {
			entry["error"] = em[1]
			args = strings.TrimSuffix(args, em[0])
		}
	}
	entry["tool"] = m[1]
	entry["outcome"] = m[2]
	if m[3] != "" {
		entry["callId"] = m[3]
	}
	entry["args"] = args
	return entry
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
	api.Get("/costs", handleCostSummary(deps))

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
		// ?turnsOver=8 keeps only conversations with MORE than 8 turns —
		// the "long conversations" facet. 0/absent = no filter.
		turnsOver := c.QueryInt("turnsOver", 0)
		if turnsOver < 0 || turnsOver > 10000 {
			return apiBadRequest(c, "turnsOver must be between 0 and 10000")
		}

		convs, next, err := deps.Store.ListConversations(c.Context(), userID, store.ListConversationsOpts{
			TopicID:   topicID,
			DeviceID:  deviceFilter,
			From:      from,
			To:        to,
			TurnsOver: turnsOver,
			Limit:     int32(queryLimit(c, 25, 50)),
			Cursor:    c.Query("cursor"),
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
		// sink wrote (api_routes.go handleTranscript), plus the tool
		// router's role=tool audit rows (internal/tools writeAudit). The
		// broker's seq-0 role=system session-start marker is skipped —
		// it's bookkeeping, not conversation.
		//
		// The two writers use disjoint seq schemes (the sink counts 0,1,2…
		// while the tool router derives seq from the millisecond clock), so
		// sk order does NOT interleave them chronologically — the merged
		// view is stable-sorted by each row's ts instead. Rows without a
		// parseable ts inherit the previous row's (carry-forward), which
		// preserves their original seq position.
		raw, err := deps.Store.ListSessionTurns(c.Context(), userID, conv.SessionID)
		if err != nil {
			return apiInternalError(c, deps, "list session turns", err)
		}
		type timedEntry struct {
			entry fiber.Map
			at    time.Time
		}
		entries := make([]timedEntry, 0, len(raw))
		var lastTS time.Time
		for i := range raw {
			if raw[i].Role == "system" {
				continue
			}
			if t, perr := time.Parse(time.RFC3339Nano, raw[i].TS); perr == nil {
				lastTS = t
			}
			var entry fiber.Map
			if raw[i].Role == "tool" {
				entry = toolTurnJSON(&raw[i])
			} else {
				entry = fiber.Map{"role": raw[i].Role, "text": raw[i].Text}
				if raw[i].TS != "" {
					entry["ts"] = raw[i].TS
				}
				if raw[i].Engine != "" {
					entry["engine"] = raw[i].Engine
				}
				if raw[i].Surface != "" {
					entry["surface"] = raw[i].Surface
				}
			}
			entries = append(entries, timedEntry{entry: entry, at: lastTS})
		}
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].at.Before(entries[j].at) })

		turns := make([]fiber.Map, 0, len(entries))
		for i := range entries {
			turns = append(turns, entries[i].entry)
		}

		resp := conversationJSON(conv)
		resp["turns"] = turns

		// rawTurns — the stored LOG# rows verbatim (every field, INCLUDING
		// the role=system session-start marker and the tool audit lines,
		// in raw sk order, no ts merge/sort): the history detail view's
		// "Raw transcript" toggle renders these for debugging what the
		// LLM pipeline actually stored. seq is the sk's numeric tail
		// (the sink's counter, or the tool router's millisecond clock).
		// Always included, NOT gated behind a query param: the detail path
		// id carries an encoded '#' (%23), and the prod edge chain
		// (CloudFront/API GW/LWA) drops everything after it — including
		// the query string — before Fiber sees the request, so a ?raw=1
		// flag silently never arrives on ConvID paths. Transcripts are
		// bounded, so shipping both views costs little.
		rawTurns := make([]fiber.Map, 0, len(raw))
		for i := range raw {
			entry := fiber.Map{"sk": raw[i].SK, "role": raw[i].Role, "text": raw[i].Text}
			if idx := strings.LastIndex(raw[i].SK, "#"); idx >= 0 && idx+1 < len(raw[i].SK) {
				entry["seq"] = raw[i].SK[idx+1:]
			}
			for k, v := range map[string]string{
				"ts": raw[i].TS, "engine": raw[i].Engine,
				"surface": raw[i].Surface, "output": raw[i].Output,
			} {
				if v != "" {
					entry[k] = v
				}
			}
			rawTurns = append(rawTurns, entry)
		}
		resp["rawTurns"] = rawTurns
		return c.JSON(resp)
	}
}

// ---- GET /api/v1/costs ----

// handleCostSummary totals the persisted per-session cost estimates for
// the current UTC month (the conversation page's Menu drawer line). One
// bounded single-partition Query over the month's CONV# range.
func handleCostSummary(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)

		now := time.Now().UTC()
		monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
		sum, err := deps.Store.SumConversationCosts(c.Context(), userID, monthStart, "")
		if err != nil {
			return apiInternalError(c, deps, "sum conversation costs", err)
		}
		return c.JSON(fiber.Map{
			"monthStart":    monthStart,
			"totalUsd":      sum.TotalUSD,
			"conversations": sum.Conversations,
			"costed":        sum.Costed,
		})
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
