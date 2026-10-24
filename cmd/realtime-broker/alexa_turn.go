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

func (b *broker) handleAlexaTurn(ctx context.Context, l *slog.Logger, req Request) (response Response) {
	startedRequest := time.Now()
	l = l.With("receiptRef", alexa.DiagnosticRef(req.AlexaTurn), "round", req.AlexaRound)
	phase := "validation"
	alexa.LogDiagnostic(ctx, l, "broker_received", nil, slog.Int("payloadBytes", len(req.Payload)))
	defer func() {
		level := slog.LevelInfo
		if response.Code >= 400 {
			level = slog.LevelWarn
		}
		l.LogAttrs(ctx, level, "Alexa broker completed", slog.String("stage", phase), slog.String("errorCode", response.Error), slog.Int("status", response.Code), slog.Int("outputBytes", len(response.Text)), slog.Int("toolCallCount", len(response.ToolCalls)), slog.Int64("elapsedMs", time.Since(startedRequest).Milliseconds()))
	}()
	if req.Surface != store.SurfaceAlexa || req.Persona != "" || req.DeviceID != "" || req.VoiceOverride != "" || len(req.Capabilities) != 0 || req.AlexaTurn == "" || req.AlexaRound < 0 || req.AlexaRound >= 5 {
		return badRequest("An internal Alexa turn receipt is required.")
	}
	if b.alexaWork == nil || b.alexaCall == nil || b.phoneStore == nil || b.gate == nil {
		return Response{Code: 503, Error: "alexa_unavailable"}
	}
	phase = "authority"
	work, err := b.alexaWork(ctx, alexa.Job{Owner: req.UserID, Turn: req.AlexaTurn})
	alexa.LogDiagnostic(ctx, l, "broker_authority", err)
	if err != nil {
		return Response{Code: 403, Error: "alexa_access_changed"}
	}
	ctx, cancel := context.WithDeadline(ctx, work.Deadline.Add(-2*time.Second))
	defer cancel()
	var payload turnPayload
	phase = "messages"
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
	phase = "quota"
	alexa.LogDiagnostic(ctx, l, "broker_quota", err, slog.Int("messageCount", len(messages)))
	if resp, rejected := gateErrResponse(l, err, "alexa"); rejected {
		return resp
	}
	if err != nil {
		return internalError("Alexa quota unavailable.")
	}
	settingsID := store.AlexaSettingsDeviceID(req.UserID)
	phase = "settings"
	settings, err := b.phoneStore.GetEffectiveSettings(ctx, req.UserID, settingsID)
	alexa.LogDiagnostic(ctx, l, "broker_settings", err)
	if err != nil {
		return internalError("Alexa settings unavailable.")
	}
	phase = "persona"
	persona, _, err := b.storedPersona(ctx, req.UserID, settings)
	alexa.LogDiagnostic(ctx, l, "broker_persona", err, slog.Int("styleBytes", len(persona.Style)))
	if err != nil {
		return badRequest("Invalid stored persona.")
	}
	phase = "guides"
	guides, err := realtime.LoadEnabledGuides(ctx, b.ddb, b.table, req.UserID)
	alexa.LogDiagnostic(ctx, l, "broker_guides", err, slog.Int("guideCount", len(guides)))
	if err != nil {
		return internalError("Alexa guides unavailable.")
	}
	req.Role, req.DeviceID = store.RoleOwner, work.Identity.Device
	directives := realtime.SessionDirectives + realtime.BuildBaseKnowledge(store.LoadProfileForDevice(ctx, b.settings, b.table, req.UserID, settingsID), time.Now()) + realtime.GuideInstructions(guides) + b.rulesBlock(ctx, l, req) + b.rememberedBlock(ctx, l, req)
	if len(directives) > 24000 {
		return Response{Code: 413, Error: "alexa_prompt_limit"}
	}
	if err = work.Check(ctx); err != nil {
		return Response{Code: 403, Error: "alexa_access_changed"}
	}
	ctx = realtime.WithClientCapabilities(ctx, "alexa", nil)
	phase = "model"
	started := time.Now()
	alexa.LogDiagnostic(ctx, l, "model_started", nil, slog.String("model", realtime.AlexaTextModel), slog.Int("directiveBytes", len(directives)), slog.Int("messageCount", len(messages)))
	result, callErr := b.alexaCall(ctx, persona, messages, directives, realtime.AlexaTextModel)
	modelMs := time.Since(started).Milliseconds()
	alexa.LogDiagnostic(ctx, l, "model_returned", callErr, slog.Int64("elapsedMs", modelMs), slog.Bool("hasResult", result != nil))
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
	alexa.LogDiagnostic(ctx, l, "usage_recorded", meterErr, slog.Bool("usageReported", usage.Reported), slog.Int64("inputTokens", usage.Input), slog.Int64("outputTokens", usage.Output), slog.Float64("costUSD", usage.CostUSD))
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
