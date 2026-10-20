package main

import (
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

// Owner decision 2026-10-19: "cap per call to $10 in cost and a 10 minute
// limit." The bridge prices every response from its usage details and ends
// the call, after a spoken goodbye, before the admitted per-call cap.
const (
	ownerCallMaxSeconds  = 600
	ownerCallMaxCostUSD  = 10.0
	ownerWrapCostRatio   = 0.9
	ownerWrapBeforeLimit = 30 // seconds before the call deadline
	ownerReplyMaxTokens  = 1024
	ownerGoodbyeTokens   = 160
	ownerContextTokens   = 6000 // truncation.token_limits.post_instructions in ownerSession
	// ownerWrapHardLimit bounds the goodbye itself: once the goodbye has been
	// requested the call ends with the wrap reason even if the model never
	// completes it, so a cap can never be outlived by a stalled response.
	ownerWrapHardLimit = 25 * time.Second
)

// gpt-realtime-2.1-mini GlobalStandard, Azure retail, USD per 1M tokens.
//
// Source check 2026-10-19: https://azure.microsoft.com/en-us/pricing/details/cognitive-services/openai-service/
// lists "GPT-Realtime 2.1 mini Global" with text, cached, audio, cached audio
// and output columns, but the page renders every price client-side ("$-") so
// the numbers could not be read from Microsoft directly. The Azure Retail
// Prices API returns no realtime meters. The six rates below match the
// Microsoft Foundry global-standard rows mirrored at
// https://modelavailability.com/models/openai/gpt-realtime-mini (updated
// 2026-10-19). Image input ($0.80 / $0.08 cached) is not priced separately:
// phone calls carry no images, and an unreported token kind is priced at the
// most expensive applicable rate below, which is never lower.
var ownerTokenPriceUSDPerMillion = struct {
	TextIn, TextInCached, AudioIn, AudioInCached, TextOut, AudioOut float64
}{TextIn: 0.60, TextInCached: 0.06, AudioIn: 10.00, AudioInCached: 0.30, TextOut: 2.40, AudioOut: 20.00}

// ownerCostCap reads the per-call model cap the recorder computed at
// admission: min($10, remaining daily allowance, remaining monthly cap),
// less the reserved telephone estimate. A missing or invalid cap rejects the
// bootstrap: the bridge never guesses a budget.
func ownerCostCap(b ownerBootstrap) (float64, error) {
	v, err := strconv.ParseFloat(b.CostCapUSD, 64)
	if err != nil || !(v > 0) || v > ownerCallMaxCostUSD {
		return 0, errors.New("invalid call cost cap")
	}
	return v, nil
}

func usageCount(m map[string]any, key string) (float64, bool) {
	n, ok := m[key].(float64)
	return n, ok && n >= 0 && n == float64(int64(n))
}

// ownerReserveUSD bounds the cost of the next reply and of the goodbye for
// this session, so the loop can stop admitting replies while both still fit
// under the cap. Every token is priced at the most expensive applicable rate:
// the post-instruction context cap (6000 tokens, ownerSession), the session
// instructions and the tool manifest (three characters per token, a low
// estimate for English and JSON) all at the audio input rate, plus the
// response's output allowance at the audio output rate. Real replies cost a
// fraction of this bound; it exists so one in-flight reply plus the goodbye
// can never carry the call past its cap.
func ownerReserveUSD(session map[string]any) (reply, goodbye float64) {
	p := ownerTokenPriceUSDPerMillion
	chars := len(stringAt(session, "instructions"))
	if tools, err := json.Marshal(session["tools"]); err == nil {
		chars += len(tools)
	}
	context := float64(ownerContextTokens + (chars+2)/3)
	reply = (context*p.AudioIn + ownerReplyMaxTokens*p.AudioOut) / 1e6
	goodbye = (context*p.AudioIn + ownerGoodbyeTokens*p.AudioOut) / 1e6
	return reply, goodbye
}

// ownerUsageCostUSD prices one response.done usage object and returns the
// number of cached input tokens the price actually discounted, so the
// reported cache count and the priced cost can never disagree. Any token
// whose kind is not reported is priced at the most expensive applicable rate,
// so a missing detail can only over-count:
//   - an input token of unknown kind costs the audio input rate;
//   - a cached input token of unknown kind costs the cached audio rate;
//   - cached tokens of a known total but unknown kind are taken from text
//     first, because discounting text saves the least;
//   - a cached count the details cannot support is not discounted at all;
//   - an output token of unknown kind costs the audio output rate.
func ownerUsageCostUSD(usage map[string]any, in, out float64) (cost float64, cachedDiscounted int64) {
	p := ownerTokenPriceUSDPerMillion
	inDetails := objectAt(usage, "input_token_details")
	text, okText := usageCount(inDetails, "text_tokens")
	audio, okAudio := usageCount(inDetails, "audio_tokens")
	cachedTotal, okCachedTotal := usageCount(inDetails, "cached_tokens")
	cached := objectAt(inDetails, "cached_tokens_details")
	cachedText, okCachedText := usageCount(cached, "text_tokens")
	cachedAudio, okCachedAudio := usageCount(cached, "audio_tokens")
	switch {
	case okText && okAudio && text+audio <= in && okCachedText && okCachedAudio && cachedText <= text && cachedAudio <= audio:
		cost += cachedText*p.TextInCached + (text-cachedText)*p.TextIn +
			cachedAudio*p.AudioInCached + (audio-cachedAudio)*p.AudioIn + (in-text-audio)*p.AudioIn
		cachedDiscounted = int64(cachedText + cachedAudio)
	case okText && okAudio && text+audio <= in && okCachedTotal && cachedTotal <= text+audio:
		cachedText = min(cachedTotal, text)
		cachedAudio = cachedTotal - cachedText
		cost += cachedText*p.TextInCached + (text-cachedText)*p.TextIn +
			cachedAudio*p.AudioInCached + (audio-cachedAudio)*p.AudioIn + (in-text-audio)*p.AudioIn
		cachedDiscounted = int64(cachedTotal)
	case okText && okAudio && text+audio <= in:
		cost += text*p.TextIn + audio*p.AudioIn + (in-text-audio)*p.AudioIn
	case okCachedTotal && cachedTotal <= in:
		cost += cachedTotal*p.AudioInCached + (in-cachedTotal)*p.AudioIn
		cachedDiscounted = int64(cachedTotal)
	default:
		cost += in * p.AudioIn
	}
	outDetails := objectAt(usage, "output_token_details")
	textOut, okTextOut := usageCount(outDetails, "text_tokens")
	audioOut, okAudioOut := usageCount(outDetails, "audio_tokens")
	if okTextOut && okAudioOut && textOut+audioOut <= out {
		cost += textOut*p.TextOut + audioOut*p.AudioOut + (out-textOut-audioOut)*p.AudioOut
	} else {
		cost += out * p.AudioOut
	}
	return cost / 1e6, cachedDiscounted
}
