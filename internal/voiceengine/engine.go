package voiceengine

// Engine identifies a voice engine. Values are byte-identical to
// contracts/settings.schema.json#/properties/voiceEngine's enum, so a settings
// pin string can be compared to these constants directly and the realtime
// broker's bootstrap routing (internal/realtime.ResolveEngine, FR-VE-03) can
// return one of these typed values.
type Engine string

const (
	// EngineOpenAIRealtime is the default engine: client-direct WebRTC with a
	// short-lived OpenAI ephemeral token. AWS is never in the media path.
	EngineOpenAIRealtime Engine = "openai-realtime"
	// EngineOpenAIRealtimeMini is the cheaper client-direct OpenAI option;
	// same transport as EngineOpenAIRealtime, a different model.
	EngineOpenAIRealtimeMini Engine = "openai-realtime-mini"
	// EngineNovaSonic is Amazon Nova Sonic on Bedrock, reached through the
	// backend media bridge (cmd/nova-bridge). It is the ONLY engine that puts
	// AWS in the audio media path (PRD N-6 exception, FR-VE-02).
	EngineNovaSonic Engine = "nova-sonic"
	// EngineGeminiFlashLive is Google's Gemini Live API with native audio
	// (M13): client-direct WSS to generativelanguage.googleapis.com with a
	// broker-minted ephemeral token — like OpenAI, AWS is never in the media
	// path and there is no bridge or standing infrastructure.
	EngineGeminiFlashLive Engine = "gemini-flash-live"
	// EngineGPTLiveAzure is Azure OpenAI Realtime: the same client-direct
	// WebRTC transport and the same config-bound 60s ephemeral secret as
	// EngineOpenAIRealtime, minted from an Azure resource instead of the
	// OpenAI platform. Choosing it is a provider and data-residency
	// decision, not a cost saving.
	EngineGPTLiveAzure Engine = "gpt-live-azure"
	// EngineGPTLiveAzureMini is the cheaper Azure OpenAI Realtime option;
	// same transport, a smaller model.
	EngineGPTLiveAzureMini Engine = "gpt-live-azure-mini"
	// EngineAzureVoiceLive is Azure AI Voice Live (public preview, no SLA).
	// Unlike every other engine its client credential is a resource-scoped
	// Entra bearer token, not a session-bound ephemeral secret, and the
	// session config is authored by the client rather than enforced by the
	// broker — see azure-voice-plan.md "The token problem, stated honestly".
	EngineAzureVoiceLive Engine = "azure-voice-live"
	// EngineAzureVoiceLiveLite is the Lite-tier Voice Live pin
	// (phi4-mm-realtime). Same credential shape and same caveats as
	// EngineAzureVoiceLive.
	EngineAzureVoiceLiveLite Engine = "azure-voice-live-lite"
)

// All is the single source of truth for the shipped engine set. Every gate
// that accepts an engine string derives its allowlist from this slice, so the
// settings write path, the mint resolver and the JSON contract cannot drift
// apart the way they could when each kept its own switch (azure-voice-plan.md
// WS-B M2, gap register D2).
//
// Order is the order engines are offered in the settings picker; keep the
// platform default first.
var All = []Engine{
	EngineOpenAIRealtime,
	EngineOpenAIRealtimeMini,
	EngineNovaSonic,
	EngineGeminiFlashLive,
	EngineGPTLiveAzure,
	EngineGPTLiveAzureMini,
	EngineAzureVoiceLive,
	EngineAzureVoiceLiveLite,
}

// AllStrings returns All as plain strings, for the JSON-schema and settings
// validators that compare against untyped input.
func AllStrings() []string {
	out := make([]string, len(All))
	for i, e := range All {
		out[i] = string(e)
	}
	return out
}

// IsAzure reports whether the engine is served by an Azure resource. Both the
// Azure OpenAI pins and the Voice Live pins return true; it is the switch the
// broker uses to pick the Azure credential path and the client-version gate
// uses to decide whether a session may be handed an Azure credential at all.
func (e Engine) IsAzure() bool {
	switch e {
	case EngineGPTLiveAzure, EngineGPTLiveAzureMini, EngineAzureVoiceLive, EngineAzureVoiceLiveLite:
		return true
	default:
		return false
	}
}

// IsVoiceLive reports whether the engine is an Azure AI Voice Live pin, which
// is the subset that carries a resource-scoped Entra bearer token instead of a
// session-bound ephemeral secret.
func (e Engine) IsVoiceLive() bool {
	return e == EngineAzureVoiceLive || e == EngineAzureVoiceLiveLite
}

// IsClientDirect reports whether the engine uses the client-direct transport
// (WebRTC/WSS straight to the provider, no backend bridge). Only nova-sonic
// is bridged, so this is the switch the session broker uses to decide between
// an ephemeral-token bootstrap and a bridge-URL bootstrap.
func (e Engine) IsClientDirect() bool { return e != EngineNovaSonic }

// Valid reports whether e is one of the known engines. It is derived from All
// so a new constant cannot be added without every gate seeing it.
func (e Engine) Valid() bool {
	for _, known := range All {
		if e == known {
			return true
		}
	}
	return false
}
