package tools

// memory_search / memory_write / entity_get / plan_upsert / forget — the
// M10 Memory Layer tool surface (FR-MEM-01/02/04/05). Thin adapters over a
// MemoryService (implemented by internal/memory: ENT#<type>#<entityId> +
// EMB#<entityId> items in the caller's own partition, Bedrock
// titan-embed-text-v2 query embeddings, in-partition cosine ranking —
// Query/GetItem only, never Scan). This file maps the tool-call schema onto
// the service, parses the flat "key=value" / "relType:targetId" argument
// encodings (realtime tool args are scalars and string arrays — objects are
// not part of the enforced ParamSpec vocabulary), and maps service outcomes
// onto client-safe ToolError codes. Mirrors the DeliverableService seam:
// deps.Memory == nil → not_configured, tests inject a fake.

import (
	"context"
	"strings"
	"unicode/utf8"
)

// MemoryEntityTypes is the closed entity-type set (locked M10 decision:
// ENT#<type>#<entityId> with exactly these six types).
var MemoryEntityTypes = []string{"person", "place", "info", "project", "task", "plan"}

// Bounds on the flat attr/relation encodings accepted by memory_write.
const (
	maxMemoryAttrs     = 20
	maxMemoryRelations = 20
	maxAttrKeyLen      = 50
	maxAttrValueLen    = 500
	maxRelationTypeLen = 50
	maxEntityIDLen     = 100
	maxPlanSteps       = 30
	maxPlanStepLen     = 500
	defaultSearchLimit = 5
	maxSearchLimit     = 20
)

// EntityRelation is one typed edge from an entity to another entity in the
// same user's graph.
type EntityRelation struct {
	Type     string `json:"type"`
	TargetID string `json:"targetId"`
}

// MemoryEntity is the tool-facing view of one ENT# item.
type MemoryEntity struct {
	EntityID  string
	Type      string // one of MemoryEntityTypes
	Name      string
	Attrs     map[string]any
	Relations []EntityRelation
	UpdatedAt string // RFC3339
	// IndexPending marks a partial success: the entity is saved but its
	// embedding write failed, so it is not semantically searchable yet
	// (memory.ErrEmbedFailed — a later write re-embeds it).
	IndexPending bool
}

// MemoryHit is one semantic-search result: an entity plus its cosine
// similarity score against the query embedding (higher = closer).
type MemoryHit struct {
	Entity MemoryEntity
	Score  float64
}

// MemoryWriteInput is the upsert payload for MemoryService.Write.
type MemoryWriteInput struct {
	EntityID  string // "" = create a new entity
	Type      string
	Name      string
	Attrs     map[string]string
	Relations []EntityRelation
}

// MemoryService is the seam between the tool router and the memory layer
// (implemented by internal/memory; the web function wires the real one,
// tool tests inject a fake). Every method operates strictly inside the
// given user's partition.
type MemoryService interface {
	// Search embeds query (Bedrock titan-embed-text-v2) and ranks the
	// user's embedded entities by cosine similarity. entityType "" means
	// all types; limit is >= 1.
	Search(ctx context.Context, userID, query, entityType string, limit int) ([]MemoryHit, error)
	// Write upserts an entity (ENT# item) and refreshes its embedding
	// (EMB# item, embedded=true on success). A non-empty EntityID updates
	// that entity; an unknown EntityID returns (nil, nil).
	Write(ctx context.Context, userID string, in MemoryWriteInput) (*MemoryEntity, error)
	// Get returns the entity, or (nil, nil) when it does not exist.
	Get(ctx context.Context, userID, entityID string) (*MemoryEntity, error)
	// UpsertPlan creates or updates an ENT#plan entity with ordered steps.
	// planID "" creates a new plan; an unknown planID returns (nil, nil).
	UpsertPlan(ctx context.Context, userID, planID, title string, steps []string) (*MemoryEntity, error)
	// Forget deletes both the ENT# and EMB# items (FR-MEM-05 propagation);
	// returns false when no such entity exists.
	Forget(ctx context.Context, userID, entityID string) (bool, error)
}

func memorySearchDefinition() *Definition {
	return &Definition{
		Name: "memory_search",
		Description: "Search the user's long-term memory (people, places, information, projects, " +
			"tasks, and plans) by meaning, not exact words. ALWAYS call this before answering any " +
			"question about the user's personal facts — their home or work address, names, " +
			"birthdays, preferences, plans, or anything they may have told you in a past " +
			"conversation — and before saying you don't know such a fact.",
		Params: []ParamSpec{
			{Name: "query", Type: "string", Required: true, MinLen: 1, MaxLen: 300,
				Description: "What to look for, phrased naturally, e.g. 'sister's birthday' or 'the kitchen remodel project'."},
			{Name: "type", Type: "string", Enum: MemoryEntityTypes,
				Description: "Optionally restrict results to one entity type."},
			{Name: "limit", Type: "integer", Min: floatPtr(1), Max: floatPtr(maxSearchLimit),
				Description: "Maximum results to return (default 5)."},
		},
		Handler: handleMemorySearch,
	}
}

func memoryWriteDefinition() *Definition {
	return &Definition{
		Name: "memory_write",
		Description: "Save or update a long-term memory entity about the user's life: a person, " +
			"place, piece of information, project, task, or plan. Use when the user shares a " +
			"lasting fact worth remembering across conversations (not for throwaway details).",
		SideEffecting: true,
		Params: []ParamSpec{
			{Name: "type", Type: "string", Required: true, Enum: MemoryEntityTypes,
				Description: "The kind of entity being remembered."},
			{Name: "name", Type: "string", Required: true, MinLen: 1, MaxLen: 200,
				Description: "Short display name for the entity, e.g. 'Sarah (sister)' or 'Kitchen remodel'."},
			{Name: "attrs", Type: "string_array",
				Description: "Facts about the entity as \"key=value\" entries, e.g. [\"birthday=March 3\", " +
					"\"city=Austin\"]. At most 20 entries; each key up to 50 characters and each value up " +
					"to 500 characters — exceeding either limit fails with invalid_args."},
			{Name: "relations", Type: "string_array",
				Description: "Edges to other entities as \"relationType:targetEntityId\" entries, e.g. " +
					"[\"sibling:ent-01\"]. Target IDs come from earlier memory_search/memory_write results. " +
					"At most 20 entries; each relationType up to 50 characters and each targetEntityId up " +
					"to 100 characters — exceeding either limit fails with invalid_args."},
			{Name: "entityId", Type: "string", MaxLen: maxEntityIDLen,
				Description: "Existing entity ID to update. Omit to create a new entity."},
		},
		Handler: handleMemoryWrite,
	}
}

func entityGetDefinition() *Definition {
	return &Definition{
		Name: "entity_get",
		Description: "Fetch one memory entity by its ID, including all stored facts and its " +
			"relationships to other entities.",
		Params: []ParamSpec{
			{Name: "entityId", Type: "string", Required: true, MinLen: 1, MaxLen: maxEntityIDLen,
				Description: "The entity's ID from a memory_search or memory_write result."},
		},
		Handler: handleEntityGet,
	}
}

func planUpsertDefinition() *Definition {
	return &Definition{
		Name: "plan_upsert",
		Description: "Create or update a multi-step plan in the user's long-term memory (a 'plan' " +
			"entity with ordered steps). Use for anything the user wants tracked over time, like " +
			"a trip itinerary or a project checklist. The steps list replaces any previous steps — " +
			"it is not appended to.",
		SideEffecting: true,
		Params: []ParamSpec{
			{Name: "planId", Type: "string", MaxLen: maxEntityIDLen,
				Description: "Existing plan's entity ID to update. Omit to create a new plan."},
			{Name: "title", Type: "string", Required: true, MinLen: 1, MaxLen: 200,
				Description: "The plan's title, e.g. 'Spring garden overhaul'."},
			{Name: "steps", Type: "string_array", Required: true,
				Description: "The full ordered list of steps (this replaces any previous steps). At most " +
					"30 steps, each up to 500 characters — exceeding either limit fails with invalid_args."},
		},
		Handler: handlePlanUpsert,
	}
}

func forgetDefinition() *Definition {
	return &Definition{
		Name: "forget",
		Description: "Permanently delete one memory entity (and its search index entry) at the " +
			"user's request. Only call this when the user explicitly asks you to forget something.",
		SideEffecting: true,
		Params: []ParamSpec{
			{Name: "entityId", Type: "string", Required: true, MinLen: 1, MaxLen: maxEntityIDLen,
				Description: "The ID of the entity to forget."},
		},
		Handler: handleForget,
	}
}

func handleMemorySearch(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if deps.Memory == nil {
		return nil, toolErrf(CodeNotConfigured, "the memory layer is not configured")
	}
	query := args["query"].(string)
	entityType, _ := args["type"].(string)
	limit := defaultSearchLimit
	if l, ok := args["limit"].(int); ok {
		limit = l
	}

	hits, err := deps.Memory.Search(ctx, inv.UserID, query, entityType, limit)
	if err != nil {
		deps.Log.Error("tools: memory_search failed", "error", err.Error())
		return nil, toolErrf(CodeUpstreamError, "the memory search failed")
	}

	results := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		r := entityOutput(&h.Entity)
		r["score"] = h.Score
		results = append(results, r)
	}
	return map[string]any{"results": results, "count": len(results)}, nil
}

func handleMemoryWrite(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if deps.Memory == nil {
		return nil, toolErrf(CodeNotConfigured, "the memory layer is not configured")
	}

	rawAttrs, _ := args["attrs"].([]string)
	attrs, terr := parseAttrPairs(rawAttrs)
	if terr != nil {
		return nil, terr
	}
	rawRels, _ := args["relations"].([]string)
	relations, terr := parseRelationPairs(rawRels)
	if terr != nil {
		return nil, terr
	}
	entityID, _ := args["entityId"].(string)

	ent, err := deps.Memory.Write(ctx, inv.UserID, MemoryWriteInput{
		EntityID:  strings.TrimSpace(entityID),
		Type:      args["type"].(string),
		Name:      strings.TrimSpace(args["name"].(string)),
		Attrs:     attrs,
		Relations: relations,
	})
	if err != nil {
		deps.Log.Error("tools: memory_write failed", "error", err.Error())
		return nil, toolErrf(CodeUpstreamError, "failed to save the memory")
	}
	if ent == nil {
		return nil, toolErrf(CodeNotFound, "no such entity to update (or it belongs to another user)")
	}

	out := entityOutput(ent)
	out["status"] = "saved"
	return out, nil
}

func handleEntityGet(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if deps.Memory == nil {
		return nil, toolErrf(CodeNotConfigured, "the memory layer is not configured")
	}
	ent, err := deps.Memory.Get(ctx, inv.UserID, args["entityId"].(string))
	if err != nil {
		deps.Log.Error("tools: entity_get failed", "error", err.Error())
		return nil, toolErrf(CodeUpstreamError, "failed to load the entity")
	}
	if ent == nil {
		return nil, toolErrf(CodeNotFound, "no such entity (or it belongs to another user)")
	}
	return entityOutput(ent), nil
}

func handlePlanUpsert(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if deps.Memory == nil {
		return nil, toolErrf(CodeNotConfigured, "the memory layer is not configured")
	}

	rawSteps, _ := args["steps"].([]string)
	steps := make([]string, 0, len(rawSteps))
	for _, s := range rawSteps {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		// Runes, not bytes — the advertised description says "characters",
		// and the router's own MinLen/MaxLen path measures runes (A5).
		if utf8.RuneCountInString(s) > maxPlanStepLen {
			return nil, toolErrf(CodeInvalidArgs, "each step must be at most %d characters", maxPlanStepLen)
		}
		steps = append(steps, s)
	}
	if len(steps) == 0 {
		return nil, toolErrf(CodeInvalidArgs, "steps must contain at least one non-empty step")
	}
	if len(steps) > maxPlanSteps {
		return nil, toolErrf(CodeInvalidArgs, "steps must contain at most %d steps", maxPlanSteps)
	}

	planID, _ := args["planId"].(string)
	ent, err := deps.Memory.UpsertPlan(ctx, inv.UserID, strings.TrimSpace(planID),
		strings.TrimSpace(args["title"].(string)), steps)
	if err != nil {
		deps.Log.Error("tools: plan_upsert failed", "error", err.Error())
		return nil, toolErrf(CodeUpstreamError, "failed to save the plan")
	}
	if ent == nil {
		return nil, toolErrf(CodeNotFound, "no such plan to update (or it belongs to another user)")
	}

	out := entityOutput(ent)
	out["status"] = "saved"
	out["planId"] = ent.EntityID
	out["stepCount"] = len(steps)
	return out, nil
}

func handleForget(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if deps.Memory == nil {
		return nil, toolErrf(CodeNotConfigured, "the memory layer is not configured")
	}
	entityID := args["entityId"].(string)
	deleted, err := deps.Memory.Forget(ctx, inv.UserID, entityID)
	if err != nil {
		deps.Log.Error("tools: forget failed", "error", err.Error())
		return nil, toolErrf(CodeUpstreamError, "failed to forget the entity")
	}
	if !deleted {
		return nil, toolErrf(CodeNotFound, "no such entity (or it belongs to another user)")
	}
	return map[string]any{"status": "forgotten", "entityId": entityID}, nil
}

// entityOutput renders a MemoryEntity as the client/model-safe result map.
func entityOutput(e *MemoryEntity) map[string]any {
	rels := make([]map[string]any, 0, len(e.Relations))
	for _, r := range e.Relations {
		rels = append(rels, map[string]any{"type": r.Type, "targetId": r.TargetID})
	}
	out := map[string]any{
		"entityId": e.EntityID,
		"type":     e.Type,
		"name":     e.Name,
	}
	if len(e.Attrs) > 0 {
		out["attrs"] = e.Attrs
	}
	if len(rels) > 0 {
		out["relations"] = rels
	}
	if e.UpdatedAt != "" {
		out["updatedAt"] = e.UpdatedAt
	}
	if e.IndexPending {
		out["indexing"] = "pending"
	}
	return out
}

// parseAttrPairs decodes the flat "key=value" attr encoding.
func parseAttrPairs(pairs []string) (map[string]string, *ToolError) {
	if len(pairs) == 0 {
		return nil, nil
	}
	if len(pairs) > maxMemoryAttrs {
		return nil, toolErrf(CodeInvalidArgs, "attrs must contain at most %d entries", maxMemoryAttrs)
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			return nil, toolErrf(CodeInvalidArgs, "each attrs entry must be \"key=value\", got %q", p)
		}
		// Runes, not bytes — the advertised description says "characters",
		// and the router's own MinLen/MaxLen path measures runes (A5).
		if utf8.RuneCountInString(k) > maxAttrKeyLen {
			return nil, toolErrf(CodeInvalidArgs, "attr key %q exceeds %d characters", k, maxAttrKeyLen)
		}
		if utf8.RuneCountInString(v) > maxAttrValueLen {
			return nil, toolErrf(CodeInvalidArgs, "attr value for %q exceeds %d characters", k, maxAttrValueLen)
		}
		out[k] = v
	}
	return out, nil
}

// parseRelationPairs decodes the flat "relationType:targetEntityId" edge
// encoding.
func parseRelationPairs(pairs []string) ([]EntityRelation, *ToolError) {
	if len(pairs) == 0 {
		return nil, nil
	}
	if len(pairs) > maxMemoryRelations {
		return nil, toolErrf(CodeInvalidArgs, "relations must contain at most %d entries", maxMemoryRelations)
	}
	out := make([]EntityRelation, 0, len(pairs))
	for _, p := range pairs {
		t, target, ok := strings.Cut(p, ":")
		t, target = strings.TrimSpace(t), strings.TrimSpace(target)
		if !ok || t == "" || target == "" {
			return nil, toolErrf(CodeInvalidArgs, "each relations entry must be \"relationType:targetEntityId\", got %q", p)
		}
		// Runes, not bytes — see parseAttrPairs.
		if utf8.RuneCountInString(t) > maxRelationTypeLen {
			return nil, toolErrf(CodeInvalidArgs, "relation type %q exceeds %d characters", t, maxRelationTypeLen)
		}
		if utf8.RuneCountInString(target) > maxEntityIDLen {
			return nil, toolErrf(CodeInvalidArgs, "relation target ID exceeds %d characters", maxEntityIDLen)
		}
		out = append(out, EntityRelation{Type: t, TargetID: target})
	}
	return out, nil
}
