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
)

// IsClientDirect reports whether the engine uses the client-direct transport
// (WebRTC/WSS straight to OpenAI, no backend bridge). Only nova-sonic is
// bridged, so this is the switch the session broker uses to decide between an
// ephemeral-token bootstrap and a bridge-URL bootstrap.
func (e Engine) IsClientDirect() bool { return e != EngineNovaSonic }

// Valid reports whether e is one of the three known engines.
func (e Engine) Valid() bool {
	switch e {
	case EngineOpenAIRealtime, EngineOpenAIRealtimeMini, EngineNovaSonic:
		return true
	default:
		return false
	}
}
