package realtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
)

// Fallback cascade (plan.md M2, Voice §12): when the realtime engine is
// unavailable, clients degrade to a chained STT -> LLM -> TTS loop (or
// text-only). All three legs are proxied through the broker because the
// broker is the ONLY holder of the OpenAI key — the web function invokes
// this Lambda with mode "fallback-turn" / "fallback-stt" / "fallback-tts"
// and never sees the key. Each leg retries twice with backoff (2× backoff
// retry per plan.md) on transient failures before reporting hard-down.
const (
	fallbackChatModel = "gpt-4o-mini"
	fallbackSTTModel  = "gpt-4o-transcribe"
	fallbackTTSModel  = "gpt-4o-mini-tts"

	// DefaultTTSVoice is the fallback-TTS default. The realtime default
	// voice (cedar) is a realtime-tuned voice not offered by the
	// /v1/audio/speech endpoint, so the closest standard voice is used.
	DefaultTTSVoice = "sage"

	chatCompletionsURL = "https://api.openai.com/v1/chat/completions"
	transcriptionsURL  = "https://api.openai.com/v1/audio/transcriptions"
	speechURL          = "https://api.openai.com/v1/audio/speech"
)

// fallbackRetryDelays are the sleeps between the initial attempt and each
// of the two retries.
var fallbackRetryDelays = []time.Duration{500 * time.Millisecond, 1 * time.Second}

// FallbackClient calls OpenAI's non-realtime endpoints for the degraded
// cascade. Like Minter, it resolves the API key through the SSM-backed
// loader on every call (5-minute cache).
type FallbackClient struct {
	httpc  *http.Client
	loader *config.Loader
}

// NewFallbackClient builds a FallbackClient.
func NewFallbackClient(loader *config.Loader) *FallbackClient {
	return &FallbackClient{
		httpc:  &http.Client{Timeout: 30 * time.Second},
		loader: loader,
	}
}

// Turn runs one text-only fallback turn: the user's text against
// gpt-4o-mini with the resolved persona's instructions as the system
// prompt. Returns the assistant's reply text.
func (c *FallbackClient) Turn(ctx context.Context, personaID, text string) (string, error) {
	persona := ResolvePersona(personaID)
	body, err := json.Marshal(map[string]any{
		"model": fallbackChatModel,
		"messages": []map[string]string{
			{"role": "system", "content": persona.Instructions},
			{"role": "user", "content": text},
		},
	})
	if err != nil {
		return "", fmt.Errorf("realtime: marshal fallback turn: %w", err)
	}

	respBody, err := c.doJSON(ctx, chatCompletionsURL, "application/json", body)
	if err != nil {
		return "", err
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("realtime: decode fallback turn response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("realtime: fallback turn returned no choices")
	}
	return out.Choices[0].Message.Content, nil
}

// Transcribe runs the STT leg: audio bytes -> gpt-4o-transcribe -> text.
// filename hints the container format to OpenAI (e.g. "audio.webm",
// "audio.wav"); contentType is the part's MIME type.
func (c *FallbackClient) Transcribe(ctx context.Context, audio []byte, filename, contentType string) (string, error) {
	if filename == "" {
		filename = "audio.webm"
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreatePart(map[string][]string{
		"Content-Disposition": {fmt.Sprintf(`form-data; name="file"; filename=%q`, filename)},
		"Content-Type":        {contentType},
	})
	if err != nil {
		return "", fmt.Errorf("realtime: build stt multipart: %w", err)
	}
	if _, err := part.Write(audio); err != nil {
		return "", fmt.Errorf("realtime: write stt audio part: %w", err)
	}
	if err := w.WriteField("model", fallbackSTTModel); err != nil {
		return "", fmt.Errorf("realtime: write stt model field: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("realtime: close stt multipart: %w", err)
	}

	respBody, err := c.doJSON(ctx, transcriptionsURL, w.FormDataContentType(), buf.Bytes())
	if err != nil {
		return "", err
	}

	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("realtime: decode stt response: %w", err)
	}
	return out.Text, nil
}

// Speak runs the TTS leg: text -> gpt-4o-mini-tts -> MP3 bytes
// (audio/mpeg). An empty voice uses DefaultTTSVoice.
func (c *FallbackClient) Speak(ctx context.Context, text, voice string) ([]byte, error) {
	if voice == "" {
		voice = DefaultTTSVoice
	}
	body, err := json.Marshal(map[string]any{
		"model":           fallbackTTSModel,
		"input":           text,
		"voice":           voice,
		"response_format": "mp3",
	})
	if err != nil {
		return nil, fmt.Errorf("realtime: marshal tts request: %w", err)
	}
	return c.doJSON(ctx, speechURL, "application/json", body)
}

// doJSON POSTs body to url with the OpenAI bearer key, retrying transient
// failures (network errors, 429, 5xx) per fallbackRetryDelays. Returns
// the raw response body of the first successful (2xx) attempt.
func (c *FallbackClient) doJSON(ctx context.Context, url, contentType string, body []byte) ([]byte, error) {
	apiKey, err := c.loader.Get(ctx, config.ParamOpenAIAPIKey, config.EnvOverrideOpenAIAPIKey)
	if err != nil {
		return nil, fmt.Errorf("realtime: resolve openai key: %w", err)
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("realtime: build fallback request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", contentType)

		resp, err := c.httpc.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("realtime: fallback request failed: %w", err)
		} else {
			respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 25<<20))
			_ = resp.Body.Close()
			switch {
			case readErr != nil:
				lastErr = fmt.Errorf("realtime: read fallback response: %w", readErr)
			case resp.StatusCode >= 200 && resp.StatusCode < 300:
				return respBody, nil
			case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
				lastErr = fmt.Errorf("realtime: openai returned %d: %s",
					resp.StatusCode, truncate(string(respBody), 300))
			default:
				// 4xx other than 429 will not improve on retry.
				return nil, fmt.Errorf("realtime: openai returned %d: %s",
					resp.StatusCode, truncate(string(respBody), 300))
			}
		}

		if attempt >= len(fallbackRetryDelays) {
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(fallbackRetryDelays[attempt]):
		}
	}
}
