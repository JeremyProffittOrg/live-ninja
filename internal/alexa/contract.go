// Package alexa implements the private ASK conversation contract. Provider
// identifiers remain internal; authenticated account/device binding is separate.
package alexa

import (
	"errors"
	"strings"
	"time"
)

const (
	ResponseBudget  = 6500 * time.Millisecond
	WorkerBudget    = 30 * time.Second
	RecoveryWindow  = 15 * time.Minute
	MaxMessages     = 12
	MaxContextBytes = 24 << 10
)

type Event struct {
	Version string `json:"version"`
	Session struct {
		ID          string `json:"sessionId"`
		New         bool   `json:"new"`
		Application struct {
			ID string `json:"applicationId"`
		} `json:"application"`
	} `json:"session"`
	Context struct {
		System struct {
			Application struct {
				ID string `json:"applicationId"`
			} `json:"application"`
			Device struct {
				ID string `json:"deviceId"`
			} `json:"device"`
			User struct {
				AccessToken string `json:"accessToken"`
			} `json:"user"`
		} `json:"System"`
	} `json:"context"`
	Request struct {
		Type        string `json:"type"`
		ID          string `json:"requestId"`
		Timestamp   string `json:"timestamp"`
		Locale      string `json:"locale"`
		DialogState string `json:"dialogState"`
		Intent      struct {
			Name  string `json:"name"`
			Slots map[string]struct {
				Value string `json:"value"`
			} `json:"slots"`
		} `json:"intent"`
	} `json:"request"`
}

type Request struct{ ID, Session, Device, Token, Command, Text string }

var ErrEnvelope = errors.New("invalid Alexa request")

func (e Event) Validate(skillID string, now time.Time) (Request, error) {
	r := Request{ID: e.Request.ID, Session: e.Session.ID, Device: e.Context.System.Device.ID, Token: e.Context.System.User.AccessToken}
	stamp, err := time.Parse(time.RFC3339, e.Request.Timestamp)
	if skillID == "" || e.Version != "1.0" || e.Context.System.Application.ID != skillID ||
		(e.Session.Application.ID != "" && e.Session.Application.ID != skillID) || e.Request.Locale != "en-US" ||
		r.ID == "" || len(r.ID) > 256 || r.Device == "" || len(r.Device) > 1024 || r.Session == "" || len(r.Session) > 1024 ||
		err != nil || stamp.Before(now.Add(-150*time.Second)) || stamp.After(now.Add(150*time.Second)) {
		return Request{}, ErrEnvelope
	}
	switch e.Request.Type {
	case "LaunchRequest":
		r.Command = "launch"
	case "SessionEndedRequest":
		r.Command = "ended"
	case "IntentRequest":
		r.Command = map[string]string{"ChatIntent": "chat", "AMAZON.StopIntent": "stop", "AMAZON.CancelIntent": "stop", "AMAZON.HelpIntent": "help", "AMAZON.RepeatIntent": "repeat", "AMAZON.FallbackIntent": "help", "MoreIntent": "more", "StatusIntent": "status", "ResumeIntent": "resume", "NewConversationIntent": "new"}[e.Request.Intent.Name]
		if r.Command == "" {
			return Request{}, ErrEnvelope
		}
		if r.Command == "chat" {
			r.Text = strings.TrimSpace(e.Request.Intent.Slots["query"].Value)
			if len(r.Text) > 4000 {
				return Request{}, ErrEnvelope
			}
		}
	default:
		return Request{}, ErrEnvelope
	}
	return r, nil
}
