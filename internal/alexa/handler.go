package alexa

import (
	"context"
	"errors"
	"strings"
	"time"
)

type Speech struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type Card struct {
	Type string `json:"type"`
}
type Response struct {
	Version  string `json:"version"`
	Response struct {
		OutputSpeech     *Speech          `json:"outputSpeech,omitempty"`
		Card             *Card            `json:"card,omitempty"`
		ShouldEndSession bool             `json:"shouldEndSession"`
		Directives       []map[string]any `json:"directives,omitempty"`
	} `json:"response"`
}

type Handler struct {
	SkillID    string
	Service    *Service
	Resolve    func(context.Context, string, string) (Identity, error)
	Now        func() time.Time
	WaitBudget time.Duration
}

func speak(text string, end bool) Response {
	r := Response{Version: "1.0"}
	r.Response.OutputSpeech = &Speech{Type: "PlainText", Text: safeSpeech(text)}
	r.Response.ShouldEndSession = end
	if !end {
		r.Response.Directives = []map[string]any{{"type": "Dialog.ElicitSlot", "slotToElicit": "query", "updatedIntent": map[string]any{"name": "ChatIntent", "confirmationStatus": "NONE", "slots": map[string]any{"query": map[string]any{"name": "query", "confirmationStatus": "NONE"}}}}}
	}
	return r
}

func commandText(text string) string {
	switch strings.Trim(strings.ToLower(text), " .!?") {
	case "stop", "cancel":
		return "stop"
	case "status", "repeat", "more", "resume", "help":
		return strings.Trim(strings.ToLower(text), " .!?")
	case "new conversation", "start over":
		return "new"
	}
	return "chat"
}

func (h *Handler) Handle(ctx context.Context, event Event) (Response, error) {
	budget := h.WaitBudget
	if budget <= 0 || budget > ResponseBudget {
		budget = ResponseBudget
	}
	waitCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	request, err := event.Validate(h.SkillID, now)
	if err != nil {
		return Response{}, err
	}
	if request.Command == "ended" {
		r := Response{Version: "1.0"}
		r.Response.ShouldEndSession = true
		return r, nil
	}
	if h.Resolve == nil || h.Service == nil {
		return speak("Live Ninja Alexa is not enabled yet.", true), nil
	}
	id, err := h.Resolve(waitCtx, request.Token, request.Device)
	if err != nil {
		if !errors.Is(err, ErrUnauthorized) {
			return h.failure(err)
		}
		r := speak("Link your Live Ninja owner account in the Alexa app, then try again.", true)
		r.Response.Card = &Card{Type: "LinkAccount"}
		return r, nil
	}
	if request.Command == "chat" {
		request.Command = commandText(request.Text)
	}
	switch request.Command {
	case "stop":
		if err = h.Service.Reset(waitCtx, id, "", false); err != nil {
			return h.failure(err)
		}
		return speak("Stopped. This conversation's recovery context is cleared. An action already completed is not undone.", true), nil
	case "launch", "new":
		if err = h.Service.Reset(waitCtx, id, request.Session, true); err != nil {
			return h.failure(err)
		}
		return speak("A new Live Ninja conversation is ready. What would you like to ask?", false), nil
	case "help":
		return speak("Ask a question, then use a follow-up. Say status for a pending answer, more for the next part, or repeat. Say stop to clear this conversation. After listening ends, say Alexa, ask live ninja to resume within fifteen minutes.", false), nil
	case "resume":
		if _, err = h.Service.Resume(waitCtx, id, request.Session); err != nil {
			return h.failure(err)
		}
		return speak("Your Live Ninja conversation is resumed. What would you like to ask?", false), nil
	case "status", "repeat", "more":
		view, e := h.Service.Speech(waitCtx, id, request.Command)
		if e != nil {
			return h.failure(e)
		}
		return viewResponse(view), nil
	case "chat":
		if request.Text == "" {
			return speak("What would you like to ask?", false), nil
		}
		view, e := h.Service.Submit(waitCtx, id, request.ID, request.Session, request.Text)
		if e != nil {
			return h.failure(e)
		}
		for view.State == "pending" || view.State == "running" {
			select {
			case <-waitCtx.Done():
				return speak("Your request is still working. Say status to hear its result without starting it again.", false), nil
			case <-time.After(150 * time.Millisecond):
			}
			view, e = h.Service.Receipt(waitCtx, id, view.Receipt)
			if e != nil {
				if waitCtx.Err() != nil {
					return speak("Your request is still working. Say status to check its result.", false), nil
				}
				return h.failure(e)
			}
		}
		view, e = h.Service.speakView(waitCtx, id, view, "status")
		if e != nil {
			return h.failure(e)
		}
		return viewResponse(view), nil
	}
	return speak("Please ask a question, or say help.", false), nil
}

func viewResponse(view View) Response {
	switch view.State {
	case "pending", "running":
		return speak("Your request is still working. Say status to check the same request.", false)
	case "ready":
		return speak("No question is waiting. What would you like to ask?", false)
	case "expired":
		return speak("That answer's recovery window expired. Please start a new question.", false)
	}
	text := view.Text
	if text == "" {
		text = "That request did not complete. Check Live Ninja before repeating an action."
	}
	if view.More {
		text += " Say more for the next part."
	}
	return speak(text, false)
}
func (h *Handler) failure(err error) (Response, error) {
	switch {
	case errors.Is(err, ErrBusy):
		return speak("Another Alexa request is still working. Say status on that Echo before starting another.", false), nil
	case errors.Is(err, ErrExpired):
		return speak("There is no unexpired conversation to resume. What would you like to ask?", false), nil
	case errors.Is(err, ErrUnauthorized):
		return speak("This Echo or account is no longer authorized. Check Live Ninja device access and re-link in the Alexa app.", true), nil
	default:
		return speak("I could not verify that request. Say status before repeating an action.", false), nil
	}
}
