package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/alexa"
	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

func (b *broker) storedPersona(ctx context.Context, owner string, settings map[string]any) (realtime.Persona, string, error) {
	section, _ := settings["persona"].(map[string]any)
	id, _ := section["presetId"].(string)
	if strings.ContainsAny(id, ":#") {
		return realtime.Persona{}, "", errors.New("invalid stored persona")
	}
	if id != "" && id != "custom" && !realtime.IsBuiltinPersona(id) {
		if p, e := b.phoneStore.GetUserPersona(ctx, owner, id); e == nil && p != nil {
			id = realtime.UserPersonaRef(owner, id)
		} else if p, e := b.phoneStore.GetCatalogPersona(ctx, id); e == nil && p != nil && p.Shared {
			id = realtime.SharedPersonaRef(id)
		}
	}
	persona := realtime.ResolvePersona(id)
	if id == "custom" {
		persona.Style, _ = section["systemInstructions"].(string)
	}
	return persona, id, nil
}

func (b *broker) handleAlexaTurn(ctx context.Context, l *slog.Logger, req Request) Response {
	if req.Surface != store.SurfaceAlexa || req.Persona != "" || req.DeviceID != "" || req.VoiceOverride != "" || len(req.Capabilities) != 0 || req.AlexaTurn == "" || req.AlexaRound < 0 || req.AlexaRound >= 5 {
		return badRequest("An internal Alexa turn receipt is required.")
	}
	if b.alexaWork == nil || b.alexaCall == nil || b.phoneStore == nil || b.gate == nil {
		return Response{Code: 503, Error: "alexa_unavailable"}
	}
	work, err := b.alexaWork(ctx, alexa.Job{Owner: req.UserID, Turn: req.AlexaTurn})
	if err != nil {
		return Response{Code: 403, Error: "alexa_access_changed"}
	}
	ctx, cancel := context.WithDeadline(ctx, work.Deadline.Add(-2*time.Second))
	defer cancel()
	var payload turnPayload
	if json.Unmarshal(req.Payload, &payload) != nil || realtime.ValidateChatMessages(payload.Messages) != nil {
		return badRequest("Invalid Alexa messages.")
	}
	messages, err := alexa.BoundMessages(payload.Messages)
	if err != nil {
		return Response{Code: 413, Error: "alexa_context_limit"}
	}
	if req.AlexaRound == 0 {
		err = b.gate.CheckFallback(ctx, req.UserID)
	} else {
		err = b.gate.CheckTextContinuation(ctx, req.UserID)
	}
	if resp, rejected := gateErrResponse(l, err, "alexa"); rejected {
		return resp
	}
	if err != nil {
		return internalError("Alexa quota unavailable.")
	}
	settings, err := b.phoneStore.GetEffectiveSettings(ctx, req.UserID, work.Identity.Device)
	if err != nil {
		return internalError("Alexa settings unavailable.")
	}
	persona, _, err := b.storedPersona(ctx, req.UserID, settings)
	if err != nil {
		return badRequest("Invalid stored persona.")
	}
	guides, err := realtime.LoadEnabledGuides(ctx, b.ddb, b.table, req.UserID)
	if err != nil {
		return internalError("Alexa guides unavailable.")
	}
	req.Role, req.DeviceID = store.RoleOwner, work.Identity.Device
	directives := realtime.SessionDirectives + realtime.BuildBaseKnowledge(store.LoadProfileForDevice(ctx, b.settings, b.table, req.UserID, req.DeviceID), time.Now()) + realtime.GuideInstructions(guides) + b.rulesBlock(ctx, l, req) + b.rememberedBlock(ctx, l, req)
	if len(directives) > 24000 {
		return Response{Code: 413, Error: "alexa_prompt_limit"}
	}
	if err = work.Check(ctx); err != nil {
		return Response{Code: 403, Error: "alexa_access_changed"}
	}
	ctx = realtime.WithClientCapabilities(ctx, "alexa", nil)
	started := time.Now()
	result, callErr := b.alexaCall(ctx, persona, messages, directives, realtime.AlexaTextModel)
	modelMs := time.Since(started).Milliseconds()
	usage := store.AlexaUsage{Model: realtime.AlexaTextModel}
	if result != nil && result.Usage != nil {
		u := result.Usage
		cost, priceErr := realtime.AlexaUsageCost(*u)
		if priceErr != nil {
			callErr = priceErr
		} else {
			usage = store.AlexaUsage{Model: u.Model, Reported: true, Input: u.InputTokens, Output: u.OutputTokens, Cached: u.CachedTokens, CostUSD: cost}
		}
	}
	meterCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	meterErr := b.phoneStore.RecordAlexaUsage(meterCtx, req.UserID, req.AlexaTurn, req.AlexaRound, started, usage)
	done()
	if meterErr != nil {
		l.Error("Alexa usage write failed")
		return Response{Code: 503, Error: "alexa_usage_unavailable", ModelMs: modelMs}
	}
	if callErr != nil || result == nil {
		l.Warn("Alexa model request failed")
		return Response{Code: 502, Error: "alexa_model_failed", ModelMs: modelMs}
	}
	return Response{Text: result.Text, ToolCalls: result.ToolCalls, Usage: result.Usage, ModelMs: modelMs}
}
