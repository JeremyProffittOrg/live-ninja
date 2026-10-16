package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// rehearsalHotelTurns is the staff side of owner rehearsal take 1
// (CAdebbeeb654690d3519827db4b7babbad, Actions run 38901224896, 2026-10-16),
// as whisper transcribed it. The deployed bridge answered every turn with an
// instruction-less response.create, so the model re-asked the room-number /
// verification question five times until the owner hung up.
var rehearsalHotelTurns = []string{
	"Thank you.",
	"Select the network and just click OK.",
	"Yeah, they clicked on a password and that's it.",
	"in Minecraft.",
	"No.",
	"the Wi-Fi password.",
}

type dialogHarness struct {
	t       *testing.T
	events  chan wireEvent
	azPeer  *websocket.Conn
	done    chan string
	creates chan string
	n       int
}

func startDialog(t *testing.T, waits ...time.Duration) *dialogHarness {
	t.Helper()
	s, c := fixture()
	if len(waits) == 2 {
		s.silenceWait, s.transcriptWait = waits[0], waits[1]
	}
	c.ModelStarted = true
	tw, _ := wsPair(t)
	az, azPeer := wsPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	h := &dialogHarness{t: t, events: make(chan wireEvent, 16), azPeer: azPeer, done: make(chan string, 1), creates: make(chan string, 16)}
	go func() { h.done <- s.bridgeLoop(ctx, c, tw, az, "MZtest", h.events) }()
	// A gorilla read timeout corrupts the connection, so read continuously and
	// let nextResponse time out on the channel instead.
	go func() {
		for {
			var v map[string]any
			if azPeer.ReadJSON(&v) != nil {
				return
			}
			if v["type"] == "response.create" {
				instructions, _ := objectAt(v, "response")["instructions"].(string)
				h.creates <- instructions
			}
		}
	}()
	h.events <- wireEvent{Source: "azure", Data: map[string]any{"type": "session.updated"}}
	return h
}

// nextResponse returns the instructions of the next response.create, skipping
// audio/truncate traffic, or ok=false when none arrives within wait.
func (h *dialogHarness) nextResponse(wait time.Duration) (string, bool) {
	h.t.Helper()
	select {
	case instructions := <-h.creates:
		h.n++
		h.events <- wireEvent{Source: "azure", Data: map[string]any{"type": "response.done", "response": map[string]any{"id": "resp" + string(rune('a'+h.n)), "status": "completed", "usage": map[string]any{"input_tokens": float64(100), "output_tokens": float64(50)}}}}
		return instructions, true
	case <-time.After(wait):
		return "", false
	}
}
func (h *dialogHarness) hotelSays(item, text string) {
	h.events <- wireEvent{Source: "azure", Data: map[string]any{"type": "input_audio_buffer.speech_started", "item_id": item}}
	h.events <- wireEvent{Source: "azure", Data: map[string]any{"type": "input_audio_buffer.committed", "item_id": item}}
	h.events <- wireEvent{Source: "azure", Data: map[string]any{"type": "conversation.item.input_audio_transcription.completed", "item_id": item, "transcript": text}}
}

// askedQuestion reports which research question an instruction tells the model to ask.
func askedQuestion(t *testing.T, instructions string) int {
	t.Helper()
	found := -1
	for i, q := range researchQuestions {
		if strings.Contains(instructions, "Ask: "+q.ask) || strings.Contains(instructions, "Ask again in different, simpler words: "+q.ask) {
			if found >= 0 {
				t.Fatalf("one response asks two questions: %q", instructions)
			}
			found = i
		}
	}
	return found
}

func TestRehearsalDoesNotRepeatQuestions(t *testing.T) {
	h := startDialog(t)
	first, ok := h.nextResponse(time.Second)
	if !ok {
		t.Fatal("no opening question")
	}
	asks := map[int]int{askedQuestion(t, first): 1}
	closed := false
	for i, text := range rehearsalHotelTurns {
		if closed {
			break
		}
		h.hotelSays("hotel"+string(rune('0'+i)), text)
		got, ok := h.nextResponse(time.Second)
		if !ok {
			t.Fatalf("no reply to %q", text)
		}
		if got == "" {
			t.Fatalf("reply to %q carries no turn instructions; the model is left to re-derive the whole questionnaire", text)
		}
		if !strings.Contains(got, text) {
			t.Fatalf("staff answer %q not fed back into the next turn: %q", text, got)
		}
		if strings.Contains(step(got), "call end_call") {
			closed = true
			continue
		}
		q := askedQuestion(t, got)
		if q < 0 {
			t.Fatalf("reply to %q neither asks a question nor closes: %q", text, got)
		}
		asks[q]++
		if asks[q] > 2 {
			t.Fatalf("question %d asked %d times (reply to %q)", q, asks[q], text)
		}
	}
	if !closed {
		t.Fatalf("call never closed politely; asks=%v", asks)
	}
	// "Thank you." is not an answer: the first question is re-asked exactly once.
	if asks[0] != 2 {
		t.Fatalf("first question asks=%d, want ask + one re-ask", asks[0])
	}
	for q := 1; q < len(researchQuestions); q++ {
		if asks[q] != 1 {
			t.Fatalf("question %d asked %d times; want exactly once: %v", q, asks[q], asks)
		}
	}
}

func TestSecondNonAnswerClosesInsteadOfLooping(t *testing.T) {
	h := startDialog(t)
	h.nextResponse(time.Second)
	h.hotelSays("a", "Thank you.")
	if got, _ := h.nextResponse(time.Second); askedQuestion(t, got) != 0 || !strings.Contains(step(got), "Ask again") {
		t.Fatalf("first non-answer must re-ask once: %q", got)
	}
	h.hotelSays("b", "Okay.")
	if got, _ := h.nextResponse(time.Second); !strings.Contains(step(got), "call end_call") || askedQuestion(t, got) >= 0 {
		t.Fatalf("second non-answer must close politely: %q", got)
	}
}

func TestSilenceReasksOnceThenCloses(t *testing.T) {
	h := startDialog(t, 50*time.Millisecond, 0)
	h.nextResponse(time.Second)
	if got, ok := h.nextResponse(time.Second); !ok || askedQuestion(t, got) != 0 || !strings.Contains(step(got), "Ask again") {
		t.Fatalf("silence must re-ask once: %q", got)
	}
	if got, ok := h.nextResponse(time.Second); !ok || !strings.Contains(step(got), "call end_call") {
		t.Fatalf("second silence must close: %q", got)
	}
	if got, ok := h.nextResponse(200 * time.Millisecond); ok {
		t.Fatalf("closing must not be followed by more prompts: %q", got)
	}
}

func TestHoldRequestWaitsWithoutCountingAsAnswer(t *testing.T) {
	h := startDialog(t)
	h.nextResponse(time.Second)
	h.hotelSays("a", "Hold on one moment.")
	if got, ok := h.nextResponse(300 * time.Millisecond); ok {
		t.Fatalf("hold must not trigger a reply: %q", got)
	}
	h.hotelSays("b", "Okay, it's just the Wi-Fi password.")
	if got, _ := h.nextResponse(time.Second); askedQuestion(t, got) != 1 {
		t.Fatalf("answer after hold must advance to the next question: %q", got)
	}
}

func TestWithdrawalEndsWithConsentWithdrawn(t *testing.T) {
	h := startDialog(t)
	h.nextResponse(time.Second)
	h.hotelSays("a", "Please stop recording.")
	got, _ := h.nextResponse(time.Second)
	if !strings.Contains(step(got), "consent_withdrawn") || askedQuestion(t, got) >= 0 {
		t.Fatalf("withdrawal must end the call: %q", got)
	}
}

func TestMissingTranscriptAdvancesInsteadOfRepeating(t *testing.T) {
	h := startDialog(t, 0, 50*time.Millisecond)
	h.nextResponse(time.Second)
	h.events <- wireEvent{Source: "azure", Data: map[string]any{"type": "input_audio_buffer.committed", "item_id": "a"}}
	if got, _ := h.nextResponse(time.Second); askedQuestion(t, got) != 1 {
		t.Fatalf("an untranscribed reply must not repeat the question: %q", got)
	}
}

func TestClassifyReply(t *testing.T) {
	for text, want := range map[string]replyKind{
		"Thank you.": replyNone, "": replyNone, "Sorry, what?": replyNone, "Can you repeat that?": replyNone,
		"No.": replyAnswer, "Yes.": replyAnswer, "the Wi-Fi password.": replyAnswer, "in Minecraft.": replyAnswer,
		"Hold on.": replyHold, "One moment please": replyHold,
		"Don't record this.": replyWithdraw, "Stop recording please": replyWithdraw, "I don't want to be recorded": replyWithdraw,
	} {
		if got := classifyReply(text); got != want {
			t.Errorf("%q => %v want %v", text, got, want)
		}
	}
}

// step returns only this turn's directive, excluding the standing rules that
// mention end_call for every turn.
func step(instructions string) string {
	if i := strings.Index(instructions, "This turn:"); i >= 0 {
		return instructions[i:]
	}
	return ""
}
