package realtime

import (
	"strings"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/voiceengine"
)

// azureRealtimeVoiceIDs is the azure-realtime-native id list, in the order
// quoted from Microsoft Learn "How to use the Voice Live API", section
// "azure-realtime model" / "Supported voices", opened 2026-09-22.
// https://learn.microsoft.com/en-us/azure/ai-services/speech-service/voice-live-how-to
// That table and the quoted task list are these 34 names. Older notes say
// "35"; that count is this same list counted one high.
var azureRealtimeVoiceIDs = []string{
	"aarti",
	"alvaro",
	"andrew",
	"antonio",
	"ava",
	"clara",
	"dalia",
	"denise",
	"diego",
	"diya",
	"elsa",
	"emma",
	"florian",
	"francisca",
	"hyunsu",
	"jorge",
	"keita",
	"liam",
	"meera",
	"nanami",
	"natasha",
	"niwat",
	"premwadee",
	"rayn",
	"remy",
	"seraphina",
	"sonia",
	"sunhi",
	"sylvie",
	"thierry",
	"william",
	"xiaoxiao",
	"ximena",
	"yunxi",
}

func TestAzureRealtimeVoiceCatalog(t *testing.T) {
	if len(SupportedAzureRealtimeVoices) != len(azureRealtimeVoiceIDs) {
		t.Fatalf("SupportedAzureRealtimeVoices = %d entries, want %d",
			len(SupportedAzureRealtimeVoices), len(azureRealtimeVoiceIDs))
	}
	if len(SupportedAzureRealtimeVoices) != 34 {
		t.Fatalf("SupportedAzureRealtimeVoices = %d entries, want 34 quoted ids",
			len(SupportedAzureRealtimeVoices))
	}

	defaults := 0
	avaSeen := false
	for i, v := range SupportedAzureRealtimeVoices {
		if v.ID != azureRealtimeVoiceIDs[i] {
			t.Errorf("SupportedAzureRealtimeVoices[%d].ID = %q, want %q", i, v.ID, azureRealtimeVoiceIDs[i])
		}
		if v.ID == "cedar" {
			t.Errorf("cedar must not be in SupportedAzureRealtimeVoices")
		}
		if v.ID == "ava" {
			avaSeen = true
		}
		if v.Default {
			defaults++
			if v.ID != "ava" {
				t.Errorf("default azure-realtime-native voice = %q, want ava", v.ID)
			}
			if !strings.HasSuffix(v.Description, " — default") {
				t.Errorf("ava description = %q, want the quoted detail plus \" — default\"", v.Description)
			}
		}
		switch v.Gender {
		case "female", "male", "neutral":
		default:
			t.Errorf("voice %q gender = %q, want female|male|neutral", v.ID, v.Gender)
		}
		if v.ID == "ava" || v.ID == "clara" {
			if v.Gender != "neutral" {
				t.Errorf("voice %q gender = %q, want neutral", v.ID, v.Gender)
			}
		}
	}
	if defaults != 1 {
		t.Errorf("azure-realtime-native defaults = %d, want exactly 1 (ava)", defaults)
	}
	if !avaSeen {
		t.Error("ava is not a member of SupportedAzureRealtimeVoices")
	}
	if voiceInCatalog(SupportedAzureRealtimeVoices, "cedar") {
		t.Error("cedar is in SupportedAzureRealtimeVoices")
	}
}

// TestEveryEngineDefaultVoiceIsInItsOwnCatalog fails when a new engine is
// added to voiceengine.All without a catalog mapping. A missing engine is a
// failure, not a skip.
//
// azure-voice-live and azure-voice-live-lite still resolve the OpenAI voice
// chain, which floors at cedar (DefaultVoice). That voice is in
// SupportedVoices. phi4-mm-realtime is not the azure-realtime-native set.
// The native catalog's own default, ava, is checked separately and is not
// forced into a broker session.
func TestEveryEngineDefaultVoiceIsInItsOwnCatalog(t *testing.T) {
	openaiDefault := ResolveVoiceChain("", "", "", "")
	if DefaultVoice != "cedar" {
		t.Fatalf("DefaultVoice = %q, want cedar", DefaultVoice)
	}
	if openaiDefault != DefaultVoice {
		t.Fatalf("OpenAI voice chain floor = %q, want %q", openaiDefault, DefaultVoice)
	}
	if !voiceInCatalog(SupportedVoices, openaiDefault) {
		t.Fatalf("cedar is not in SupportedVoices")
	}

	geminiDefault := ResolveGeminiVoiceChain("", "")
	if DefaultGeminiVoice != "Kore" {
		t.Fatalf("DefaultGeminiVoice = %q, want Kore", DefaultGeminiVoice)
	}
	if geminiDefault != DefaultGeminiVoice {
		t.Fatalf("Gemini voice chain floor = %q, want %q", geminiDefault, DefaultGeminiVoice)
	}
	if !voiceInCatalog(SupportedGeminiVoices, geminiDefault) {
		t.Fatalf("Kore is not in SupportedGeminiVoices")
	}

	if len(voiceengine.All) == 0 {
		t.Fatal("voiceengine.All is empty")
	}
	for _, engine := range voiceengine.All {
		var catalog []VoiceInfo
		var voice string
		switch engine {
		case voiceengine.EngineOpenAIRealtime,
			voiceengine.EngineOpenAIRealtimeMini,
			voiceengine.EngineNovaSonic,
			voiceengine.EngineGPTLiveAzure,
			voiceengine.EngineGPTLiveAzureMini,
			voiceengine.EngineAzureVoiceLive,
			voiceengine.EngineAzureVoiceLiveLite:
			voice, catalog = openaiDefault, SupportedVoices
		case voiceengine.EngineGeminiFlashLive:
			voice, catalog = geminiDefault, SupportedGeminiVoices
		default:
			t.Errorf("engine %q has no default-voice catalog mapping", engine)
			continue
		}
		if !voiceInCatalog(catalog, voice) {
			t.Errorf("engine %s default voice %q is not in its catalog", engine, voice)
		}
	}

	nativeDefault := ""
	nativeDefaults := 0
	for _, v := range SupportedAzureRealtimeVoices {
		if v.Default {
			nativeDefaults++
			nativeDefault = v.ID
		}
	}
	if nativeDefaults != 1 || nativeDefault != "ava" {
		t.Errorf("azure-realtime-native catalog default = %q (%d defaults), want ava", nativeDefault, nativeDefaults)
	}
	if !voiceInCatalog(SupportedAzureRealtimeVoices, "ava") {
		t.Error("ava is not in SupportedAzureRealtimeVoices")
	}
}

func voiceInCatalog(catalog []VoiceInfo, id string) bool {
	for _, v := range catalog {
		if v.ID == id {
			return true
		}
	}
	return false
}
