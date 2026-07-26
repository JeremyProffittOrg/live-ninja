package realtime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
)

type mintRoundTripFunc func(*http.Request) (*http.Response, error)

func (f mintRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestBuildTurnDetection locks the micEagerness -> turn_detection mapping:
// semantic_vad always; eagerness forwarded only for the explicit choices;
// interrupt_response true everywhere EXCEPT low ("Patient"), where the
// server must not auto-truncate on ambient-noise VAD blips (the client
// confirms real speech before cancelling — see realtime.mjs).
func TestBuildTurnDetection(t *testing.T) {
	cases := []struct {
		name              string
		eagerness         string
		wantEagerness     string // "" = key must be absent (API default)
		wantInterruptResp bool
	}{
		{name: "low is patient", eagerness: "low", wantEagerness: "low", wantInterruptResp: false},
		{name: "medium forwarded", eagerness: "medium", wantEagerness: "medium", wantInterruptResp: true},
		{name: "high forwarded", eagerness: "high", wantEagerness: "high", wantInterruptResp: true},
		{name: "auto keeps API default", eagerness: "auto", wantEagerness: "", wantInterruptResp: true},
		{name: "empty keeps API default", eagerness: "", wantEagerness: "", wantInterruptResp: true},
		{name: "unknown keeps API default", eagerness: "bogus", wantEagerness: "", wantInterruptResp: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			td := buildTurnDetection(tc.eagerness)

			if got := td["type"]; got != "semantic_vad" {
				t.Fatalf("type = %v, want semantic_vad", got)
			}
			if got := td["interrupt_response"]; got != tc.wantInterruptResp {
				t.Fatalf("interrupt_response = %v, want %v", got, tc.wantInterruptResp)
			}
			got, present := td["eagerness"]
			if tc.wantEagerness == "" {
				if present {
					t.Fatalf("eagerness = %v, want absent", got)
				}
			} else if got != tc.wantEagerness {
				t.Fatalf("eagerness = %v, want %q", got, tc.wantEagerness)
			}
		})
	}
}

// TestBuildAudioInput verifies the full GA audio.input object: the mapped
// turn_detection, near_field noise reduction (pre-VAD, dampens false
// speech_started from ambient noise), and the input transcription model.
func TestBuildAudioInput(t *testing.T) {
	in := buildAudioInput("low")

	td, ok := in["turn_detection"].(map[string]any)
	if !ok {
		t.Fatalf("turn_detection missing or wrong type: %T", in["turn_detection"])
	}
	if td["type"] != "semantic_vad" || td["eagerness"] != "low" || td["interrupt_response"] != false {
		t.Fatalf("turn_detection = %v, want semantic_vad/low/interrupt_response=false", td)
	}

	nr, ok := in["noise_reduction"].(map[string]any)
	if !ok || nr["type"] != "near_field" {
		t.Fatalf("noise_reduction = %v, want {type: near_field}", in["noise_reduction"])
	}

	tr, ok := in["transcription"].(map[string]any)
	if !ok || tr["model"] != "gpt-4o-mini-transcribe" {
		t.Fatalf("transcription = %v, want {model: gpt-4o-mini-transcribe}", in["transcription"])
	}
}

// TestBuildAudioInputJSONShape round-trips the default-mode object through
// JSON the way Mint ships it, asserting the wire shape the GA API sees.
func TestBuildAudioInputJSONShape(t *testing.T) {
	b, err := json.Marshal(buildAudioInput(""))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		TurnDetection struct {
			Type              string  `json:"type"`
			Eagerness         *string `json:"eagerness"`
			InterruptResponse *bool   `json:"interrupt_response"`
		} `json:"turn_detection"`
		NoiseReduction struct {
			Type string `json:"type"`
		} `json:"noise_reduction"`
	}
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.TurnDetection.Type != "semantic_vad" {
		t.Fatalf("turn_detection.type = %q", decoded.TurnDetection.Type)
	}
	if decoded.TurnDetection.Eagerness != nil {
		t.Fatalf("default mode must omit eagerness, got %q", *decoded.TurnDetection.Eagerness)
	}
	if decoded.TurnDetection.InterruptResponse == nil || !*decoded.TurnDetection.InterruptResponse {
		t.Fatalf("default mode must ship interrupt_response=true")
	}
	if decoded.NoiseReduction.Type != "near_field" {
		t.Fatalf("noise_reduction.type = %q, want near_field", decoded.NoiseReduction.Type)
	}
}

func TestMiniMinterBindsExactModelInRequestAndResult(t *testing.T) {
	t.Setenv(config.EnvOverrideOpenAIAPIKey, "test")
	minter := NewMinter(config.NewLoaderWithClient(nil), MiniRealtimeModel)

	var requestModel string
	minter.httpc = &http.Client{Transport: mintRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload struct {
			Session struct {
				Model string `json:"model"`
			} `json:"session"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requestModel = payload.Session.Model
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewBufferString(`{"value":"ephemeral-test","expires_at":1785096000}`)),
			Header:     make(http.Header),
		}, nil
	})}

	result, err := minter.Mint(context.Background(), "", "cedar", "", "", "web")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if requestModel != MiniRealtimeModel {
		t.Fatalf("request session.model = %q, want %q", requestModel, MiniRealtimeModel)
	}
	if result.Model != MiniRealtimeModel {
		t.Fatalf("result model = %q, want %q", result.Model, MiniRealtimeModel)
	}

	var session struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(result.SessionConfig, &session); err != nil {
		t.Fatalf("decode returned session config: %v", err)
	}
	if session.Model != MiniRealtimeModel {
		t.Fatalf("returned session config model = %q, want %q", session.Model, MiniRealtimeModel)
	}
}
