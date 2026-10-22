package alexa

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode"

	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
)

var speechCode = regexp.MustCompile("(?s)```.*?```")
var speechMarkup = regexp.MustCompile(`<[^>]*>`)
var speechURL = regexp.MustCompile(`https?://[^\s)]+`)
var speechCredential = regexp.MustCompile(`(?i)(Bearer\s+[A-Za-z0-9._~+/-]{8,}|sk-[A-Za-z0-9_-]{12,}|ln_mcp_[ar]_[A-Za-z0-9_-]+|amzn1\.oa2\.[A-Za-z0-9._-]+)`)

func safeSpeech(text string) string {
	if strings.Contains(text, "PRIVATE KEY-----") {
		return "That answer contains sensitive key material. View it privately in Live Ninja."
	}
	text = speechCode.ReplaceAllString(text, " Code is available in Live Ninja. ")
	text = speechMarkup.ReplaceAllString(text, "")
	text = speechURL.ReplaceAllString(text, "link in Live Ninja")
	text = speechCredential.ReplaceAllString(text, "sensitive value omitted")
	return strings.Join(strings.Fields(strings.NewReplacer("<", "", ">", "", "`", "").Replace(text)), " ")
}
func speechChunk(text string, start int) (string, int, bool) {
	runes := []rune(text)
	if start >= len(runes) {
		return "That is the full answer.", len(runes), false
	}
	// Reserve space for the spoken "say more" instruction in the 600-character response.
	end := start + 570
	if end > len(runes) {
		end = len(runes)
	} else {
		for cut := end; cut > start+300; cut-- {
			if unicode.IsSpace(runes[cut-1]) {
				end = cut
				break
			}
		}
	}
	return strings.TrimSpace(string(runes[start:end])), end, end < len(runes)
}
func chatAnswer(text string) realtime.ChatMessage {
	return realtime.ChatMessage{Role: "assistant", Content: text}
}

// BoundMessages drops complete older user exchanges, preserving paired tool
// calls/results. Oversized current work fails explicitly rather than truncating
// a tool result into a fabricated successful response.
func BoundMessages(messages []realtime.ChatMessage) ([]realtime.ChatMessage, error) {
	for len(messages) > 0 {
		raw, err := json.Marshal(messages)
		if err != nil {
			return nil, err
		}
		if len(messages) <= MaxMessages && len(raw) <= MaxContextBytes {
			return messages, nil
		}
		next := -1
		for i := 1; i < len(messages); i++ {
			if messages[i].Role == "user" {
				next = i
				break
			}
		}
		if next < 0 {
			return nil, errors.New("Alexa current turn exceeds its context limit")
		}
		messages = messages[next:]
	}
	return nil, ErrEnvelope
}

func (s *Service) Receipt(ctx context.Context, id Identity, receipt string) (View, error) {
	if err := s.authorized(ctx, id); err != nil {
		return View{}, err
	}
	if len(receipt) != 64 {
		return View{}, ErrEnvelope
	}
	var current turn
	row, err := s.read(ctx, id.Owner, turnKey(receipt), &current)
	if err != nil {
		return View{}, err
	}
	if row == nil {
		return View{}, ErrExpired
	}
	return s.viewTurn(ctx, id, current, row)
}

func (s *Service) Speech(ctx context.Context, id Identity, mode string) (View, error) {
	view, err := s.Status(ctx, id)
	if err != nil {
		return View{}, err
	}
	return s.speakView(ctx, id, view, mode)
}
func (s *Service) speakView(ctx context.Context, id Identity, view View, mode string) (View, error) {
	if view.State != "complete" {
		return view, nil
	}
	var state session
	row, err := s.read(ctx, id.Owner, sessionKey(id), &state)
	if err != nil {
		return View{}, err
	}
	if !s.validSession(row, state, id) {
		return View{}, ErrExpired
	}
	text := safeSpeech(view.Text)
	if state.LastTurn != view.Receipt {
		view.Text, _, view.More = speechChunk(text, 0)
		return view, nil
	}
	start := 0
	if mode == "more" {
		start = state.SpeechEnd
	} else if mode == "repeat" {
		start = state.SpeechStart
	}
	view.Text, state.SpeechEnd, view.More = speechChunk(text, start)
	state.SpeechStart = start
	err = s.Repo.Commit(ctx, []Change{changed(id.Owner, sessionKey(id), state, s.now().Add(RecoveryWindow).Unix(), row)})
	return view, err
}
