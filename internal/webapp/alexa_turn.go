package webapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/agentmemory"
	"github.com/JeremyProffittOrg/live-ninja/internal/alexa"
	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
)

func NewAlexaRunner(deps *Deps) (func(context.Context, alexa.Work) (alexa.RunResult, error), error) {
	registry := buildAPIToolsRegistry(deps)
	if registry == nil {
		return nil, errors.New("Alexa tool registry unavailable")
	}
	return func(ctx context.Context, work alexa.Work) (alexa.RunResult, error) {
		return runAlexaTurn(ctx, deps, registry, work)
	}, nil
}

func runAlexaTurn(ctx context.Context, deps *Deps, registry *tools.Registry, work alexa.Work) (alexa.RunResult, error) {
	started := time.Now()
	if work.Check == nil || registry == nil || len(work.Messages) == 0 {
		return alexa.RunResult{}, errors.New("Alexa worker context unavailable")
	}
	if err := work.Check(ctx); err != nil {
		alexa.LogDiagnostic(ctx, deps.Log, "runner_authority", err, slog.String("receiptRef", alexa.DiagnosticRef(work.Receipt)))
		return alexa.RunResult{}, err
	}
	deps.Log.Info("Alexa turn started", slog.String("receipt", work.Receipt), slog.Int64("queueWaitMs", time.Since(work.Deadline.Add(-alexa.WorkerBudget)).Milliseconds()))
	binding, _ := json.Marshal(struct {
		Identity alexa.Identity
		Session  string
		Version  int64
	}{work.Identity, work.SessionID, work.Settings.Version})
	bindingHash := sha256.Sum256(binding)
	confirmationContext := hex.EncodeToString(bindingHash[:])
	confirmedByUser := false
	switch strings.Trim(strings.ToLower(work.Messages[len(work.Messages)-1].Content), " .!?\t\r\n") {
	case "yes", "yes please", "yes do it", "yes go ahead", "confirm", "confirm that", "go ahead", "do it", "proceed":
		confirmedByUser = true
	}
	ctx = realtime.WithClientCapabilities(ctx, store.SurfaceAlexa, nil)
	var manifest []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(realtime.ToolManifestJSONForClient(ctx, store.SurfaceAlexa, true), &manifest); err != nil {
		return alexa.RunResult{}, err
	}
	allowed := make(map[string]bool, len(manifest))
	for _, tool := range manifest {
		allowed[tool.Name] = true
	}
	alexa.LogDiagnostic(ctx, deps.Log, "runner_configured", nil, slog.String("receipt", work.Receipt), slog.Int("toolCount", len(allowed)), slog.Bool("trialMode", os.Getenv("ALEXA_TRIAL_MODE") == "true"), slog.Bool("keepTranscript", work.Settings.KeepTranscript), slog.Int64("settingsVersion", work.Settings.Version))
	round, tokens, cost := 0, 0, 0.0
	result, err := runTextTurn(ctx, work.Messages, func(ctx context.Context, messages []brokerChatMessage) (*brokerResponse, error) {
		if err := work.Check(ctx); err != nil {
			return nil, err
		}
		bounded, err := alexa.BoundMessages(messages)
		if err != nil {
			return nil, err
		}
		payload, err := json.Marshal(map[string]any{"messages": bounded})
		if err != nil {
			return nil, err
		}
		modelStarted := time.Now()
		alexa.LogDiagnostic(ctx, deps.Log, "broker_invocation_started", nil, slog.String("receipt", work.Receipt), slog.Int("round", round), slog.Int("messageCount", len(bounded)), slog.Int("payloadBytes", len(payload)))
		response, err := invokeRealtimeBroker(ctx, deps, brokerRequest{Mode: "alexa-turn", TxID: work.Receipt, UserID: work.Identity.Owner, Surface: store.SurfaceAlexa, AlexaTurn: work.Receipt, AlexaRound: round, Payload: payload}, func(o *lambda.Options) { o.RetryMaxAttempts = 1 })
		alexa.LogDiagnostic(ctx, deps.Log, "broker_invocation_completed", err, slog.String("receipt", work.Receipt), slog.Int("round", round), slog.Int64("elapsedMs", time.Since(modelStarted).Milliseconds()), slog.Bool("hasResult", response != nil))
		round++
		if response != nil && response.Usage != nil {
			if estimate, e := realtime.AlexaUsageCost(*response.Usage); e == nil {
				cost += estimate
				tokens += int(response.Usage.InputTokens + response.Usage.OutputTokens)
			}
			deps.Log.Info("Alexa model completed", slog.String("receipt", work.Receipt), slog.Int64("modelMs", response.ModelMs), slog.Int("round", round))
		}
		return response, err
	}, func(ctx context.Context, call brokerChatToolCall, iter, index int) *tools.Result {
		toolStarted := time.Now()
		toolName := call.Name
		if !allowed[toolName] {
			toolName = "unrecognized"
		}
		alexa.LogDiagnostic(ctx, deps.Log, "tool_started", nil, slog.String("receipt", work.Receipt), slog.String("tool", toolName), slog.Int("round", iter), slog.Int("argumentBytes", len(call.Arguments)))
		// One identifier-only line per tool call: the tool name and error code,
		// never its arguments or output. Alexa writes no tool transcript rows, so
		// without this a failed lookup (2026-10-24 weather) cannot be diagnosed.
		logTool := func(res *tools.Result) *tools.Result {
			code := ""
			if res != nil && res.Error != nil {
				code = res.Error.Code
			}
			attrs := []any{slog.String("receipt", work.Receipt), slog.String("tool", toolName), slog.Bool("ok", code == ""), slog.String("errorCode", code), slog.Int64("elapsedMs", time.Since(toolStarted).Milliseconds()), slog.Int("round", iter)}
			if code == "" {
				deps.Log.Info("Alexa tool completed", attrs...)
			} else {
				deps.Log.Warn("Alexa tool failed", attrs...)
			}
			return res
		}
		failure := func(code, message string) *tools.Result {
			return logTool(&tools.Result{Tool: call.Name, CallID: call.ID, TxID: work.Receipt, Error: &tools.ToolError{Code: code, Message: message, TxID: work.Receipt}})
		}
		if work.Check(ctx) != nil {
			return failure(tools.CodeForbidden, "Alexa access or settings changed. No new action was started.")
		}
		if os.Getenv("ALEXA_TRIAL_MODE") == "true" {
			return failure(tools.CodeForbidden, "The setup trial supports conversation only. Complete the Echo test before requesting tool actions.")
		}
		if !allowed[call.Name] {
			return failure(tools.CodeForbidden, "Use the Live Ninja website for this action. It is not available through Alexa.")
		}
		var args map[string]any
		if strings.TrimSpace(call.Arguments) != "" && json.Unmarshal([]byte(call.Arguments), &args) != nil {
			return failure(tools.CodeInvalidArgs, "The function arguments were not valid JSON.")
		}
		return logTool(registry.Invoke(ctx, tools.Invocation{Tool: call.Name, Args: args, IdempotencyKey: fmt.Sprintf("%s#alexa%d-%d#%s", work.Receipt, iter, index, call.ID), CallID: call.ID, TxID: work.Receipt,
			UserID: work.Identity.Owner, DeviceID: work.Identity.Device, SessionID: work.SessionID, Surface: store.SurfaceAlexa, Role: store.RoleOwner, Scope: "alexa:chat", OmitTranscript: !work.Settings.KeepTranscript, TranscriptExpiresAt: time.Now().Add(time.Duration(work.Settings.RetentionDays) * 24 * time.Hour), ConfirmationContext: confirmationContext, ConfirmationTurn: work.Receipt, ConfirmedByUser: confirmedByUser}))
	})
	alexa.LogDiagnostic(ctx, deps.Log, "runner_completed", err, slog.String("receipt", work.Receipt), slog.Int("rounds", round), slog.Int("tokens", tokens), slog.Float64("costUSD", cost), slog.Int64("elapsedMs", time.Since(started).Milliseconds()))
	if err != nil {
		return alexa.RunResult{}, err
	}
	if result.Failure != nil {
		if result.Failure.Error == "quota_exceeded" {
			// Keep the limit kind and reset time so Alexa can say why nothing
			// ran; the broker's 402 body is otherwise lost here.
			resetAt, _ := time.Parse(time.RFC3339, result.Failure.ResetAt)
			return alexa.RunResult{}, &alexa.QuotaError{Kind: result.Failure.Kind, ResetAt: resetAt, ToolRan: len(result.Calls) > 0}
		}
		return alexa.RunResult{}, errors.New("Alexa broker request failed")
	}
	if err = work.Check(ctx); err != nil {
		return alexa.RunResult{}, err
	}
	if work.Settings.KeepTranscript && !result.Limited && strings.TrimSpace(result.Text) != "" {
		persistStarted := time.Now()
		input := work.Messages[len(work.Messages)-1].Content
		err = deps.Store.SaveAlexaExchange(ctx, work.Identity.Owner, store.AlexaExchange{Session: work.SessionID, Device: work.Identity.Device, Generation: work.Identity.Generation, Input: input, Output: result.Text,
			StartedAt: work.StartedAt, At: time.Now(), Sequence: work.Sequence, RetentionDays: work.Settings.RetentionDays, SettingsVersion: work.Settings.Version, CostUSD: cost, Tokens: tokens})
		alexa.LogDiagnostic(ctx, deps.Log, "transcript_persisted", err, slog.String("receipt", work.Receipt), slog.Int64("elapsedMs", time.Since(persistStarted).Milliseconds()))
		if err != nil {
			return alexa.RunResult{}, err
		}
		if deps.AgentMemory.Admits(store.RoleOwner) {
			memoryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, memoryErr := deps.AgentMemory.RecordExchanges(memoryCtx, work.Identity.Owner, work.SessionID, []agentmemory.Turn{{Seq: work.Sequence, Role: "user", Text: input}, {Seq: work.Sequence + 1, Role: "assistant", Text: result.Text}})
			cancel()
			alexa.LogDiagnostic(ctx, deps.Log, "memory_recorded", memoryErr, slog.String("receipt", work.Receipt))
			if memoryErr != nil {
				deps.Log.Warn("Alexa automatic memory write failed")
			}
		}
	}
	return alexa.RunResult{Text: result.Text, Messages: result.Messages, Limited: result.Limited}, nil
}
