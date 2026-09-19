package tools

import "context"

// Device-local session controls (owner request 2026-07-25: "close the app when
// asked — stop listening", and "ask the application to start a new conversation").
//
// These are the first tools whose work CANNOT happen on the server. Stopping the
// microphone or recycling a realtime session is an action on the device holding
// that microphone; the backend has no reach into it. They are declared here anyway
// because the manifest is single-sourced from this registry — it is what tells the
// model the capability exists at all — but the client is expected to intercept them
// before they ever reach `POST /api/v1/tools/invoke` (Android: ToolCallRouter).
//
// [Definition.DeviceLocal] marks that contract. Reaching the handler below means a
// surface that cannot perform the action called it anyway, which is a real
// misconfiguration worth reporting back to the model plainly rather than pretending
// success — a tool that claims it stopped the microphone when nothing stopped is
// exactly the kind of lie the wake-phrase work spent this week removing.

func stopListeningDefinition() *Definition {
	return &Definition{
		Name: "stop_listening",
		Description: "End the live conversation and go back to waiting for the wake word. " +
			"Always-on wake-word listening stays armed. Use when the user says stop listening, " +
			"Jarvis stop listening, that's all, or they are done talking for now. Tell them " +
			"briefly to say the wake word when they want to talk again — do not turn always-listening off.",
		DeviceLocal: true,
		Surfaces:    []string{"web", "android", "device"},
		Handler:     handleDeviceLocalOnly,
	}
}

func startNewConversationDefinition() *Definition {
	return &Definition{
		Name: "start_new_conversation",
		Description: "End the current conversation and immediately start a fresh one, so the " +
			"new conversation gets its own transcript and history entry. Use when the user " +
			"asks to start over, start a new conversation, clear the conversation, or change " +
			"to an unrelated subject and wants a clean slate. This does not delete anything.",
		DeviceLocal: true,
		Surfaces:    []string{"web", "android"},
		Handler:     handleDeviceLocalOnly,
	}
}

// handleDeviceLocalOnly is the honest fallback for a [Definition.DeviceLocal] tool
// that reached the server. It is not an internal error — the request was well
// formed — so it returns CodeNotConfigured, which the RCA pipeline deliberately
// does not treat as a defect worth an Opus analysis.
func handleDeviceLocalOnly(
	_ context.Context, _ *Deps, inv Invocation, _ map[string]any,
) (map[string]any, *ToolError) {
	return nil, toolErrf(CodeNotConfigured,
		"%s runs on the user's device and the %s surface cannot perform it",
		inv.Tool, surfaceOrUnknown(inv.Surface))
}

func surfaceOrUnknown(surface string) string {
	if surface == "" {
		return "current"
	}
	return surface
}
