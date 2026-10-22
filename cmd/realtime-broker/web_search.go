package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

func (b *broker) handleWebSearch(ctx context.Context, l *slog.Logger, req Request) Response {
	if !validSurfaces[req.Surface] && req.Surface != "phone" {
		return badRequest("invalid search surface")
	}
	if req.Surface == "phone" && req.Role != store.RoleOwner {
		return Response{Code: 403, Error: "owner_required"}
	}
	var payload struct {
		Query string `json:"query"`
		Days  int    `json:"days"`
	}
	if json.Unmarshal(req.Payload, &payload) != nil || strings.TrimSpace(payload.Query) == "" || utf8.RuneCountInString(payload.Query) > 300 || payload.Days < 0 || payload.Days > 365 {
		return badRequest("a bounded search query is required")
	}
	if b.webSearch == nil || b.gate == nil {
		return Response{Code: 503, Error: "search_unavailable", Message: "Public web search is not configured."}
	}
	if resp, rejected := b.gateFallback(ctx, l, req); rejected {
		return resp
	}
	result, err := b.webSearch(ctx, payload.Query, payload.Days, time.Now())
	if err != nil {
		l.Warn("web search failed", slog.String("error", err.Error()))
		return Response{Code: 502, Error: "search_failed", Message: "Public web search is unavailable; no answer was verified."}
	}
	return Response{Search: result}
}
