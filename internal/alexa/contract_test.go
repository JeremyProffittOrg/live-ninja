package alexa

import (
	"encoding/json"
	"testing"
	"time"
)

func TestContract(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	base := `{"version":"1.0","session":{"sessionId":"session-1"},"context":{"System":{"application":{"applicationId":"skill-1"},"device":{"deviceId":"echo-1"},"user":{"accessToken":"test-linked-token"}}},"request":{"type":"IntentRequest","requestId":"request-1","timestamp":"2026-10-22T12:00:00Z","locale":"en-US","intent":{"name":"ChatIntent","slots":{"query":{"value":"What about its population?"}}}}}`
	var ev Event
	if err := json.Unmarshal([]byte(base), &ev); err != nil {
		t.Fatal(err)
	}
	request, err := ev.Validate("skill-1", now)
	if err != nil || request.Command != "chat" || request.Text != "What about its population?" || request.Device != "echo-1" {
		t.Fatalf("valid follow-up: %+v, %v", request, err)
	}
	for _, name := range []string{"AMAZON.StopIntent", "AMAZON.CancelIntent", "StatusIntent", "ResumeIntent", "NewConversationIntent", "AMAZON.RepeatIntent", "MoreIntent", "AMAZON.HelpIntent", "AMAZON.FallbackIntent"} {
		t.Run(name, func(t *testing.T) {
			e := ev
			e.Request.Intent.Name = name
			if _, err := e.Validate("skill-1", now); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, kind := range []string{"LaunchRequest", "SessionEndedRequest"} {
		e := ev
		e.Request.Type = kind
		if _, err := e.Validate("skill-1", now); err != nil {
			t.Fatal(err)
		}
	}
	for _, mutation := range []struct {
		name   string
		change func(*Event)
	}{
		{"wrong-skill", func(e *Event) { e.Context.System.Application.ID = "another" }},
		{"missing-device", func(e *Event) { e.Context.System.Device.ID = "" }},
		{"wrong-locale", func(e *Event) { e.Request.Locale = "fr-FR" }},
		{"old-request", func(e *Event) { e.Request.Timestamp = "2026-10-21T12:00:00Z" }},
		{"unknown-intent", func(e *Event) { e.Request.Intent.Name = "BuyThings" }},
		{"missing-request-id", func(e *Event) { e.Request.ID = "" }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			e := ev
			mutation.change(&e)
			if _, err := e.Validate("skill-1", now); err == nil {
				t.Fatal("accepted invalid envelope")
			}
		})
	}
}
