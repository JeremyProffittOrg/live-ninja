package main

import (
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const navigationLimit = 75 * time.Second
const navigationTurns = 8
const navigationSelections = 4
const menuDigit = "(zero|one|two|three|four|five|six|seven|eight|nine|star|pound|[0-9*#])"

var optionAfter = regexp.MustCompile(`\b(?:for|to reach|to speak to|to speak with)\s+([^,.;!?]{1,90}?),?\s+(?:press|dial|select)\s+` + menuDigit + `(?:\b|$)`)
var optionBefore = regexp.MustCompile(`\b(?:press|dial|select)\s+` + menuDigit + `\s+(?:for|to reach|to speak to|to speak with)\s+`)
var menuPrompt = regexp.MustCompile(`\b(press|dial|select|say|menu|options|department|direct your call|tell me|how can i help)\b`)
var personGreeting = regexp.MustCompile(`\b(hello|hi|good morning|good afternoon|good evening|speaking|my name is|this is|how may i help|how can i help|can i help you)\b`)

type navigationChoice struct{ kind, value string }

func routePriority(label string) int {
	for rank, names := range [][]string{
		{"front desk", "reception"},
		{"customer service", "guest service", "guest assistance"},
		{"operator", "representative", "agent"},
		{"english"},
	} {
		for _, name := range names {
			if strings.Contains(label, name) {
				return rank + 1
			}
		}
	}
	return 0
}
func navigationAction(speech string, requested bool) navigationChoice {
	v := strings.ToLower(strings.Join(strings.Fields(speech), " "))
	if len(v) > 2000 {
		return navigationChoice{"unsupported", ""}
	}
	for _, phrase := range []string{"leave a message", "after the tone", "after the beep", "mailbox", "voicemail", "office is closed"} {
		if strings.Contains(v, phrase) {
			return navigationChoice{"voicemail", ""}
		}
	}
	// Select only digits actually announced for a relevant destination.
	best, digit := 99, ""
	consider := func(label, spoken string) {
		rank := routePriority(label)
		if rank == 0 || rank >= best {
			return
		}
		words := map[string]string{"zero": "0", "one": "1", "two": "2", "three": "3", "four": "4", "five": "5", "six": "6", "seven": "7", "eight": "8", "nine": "9", "star": "*", "pound": "#"}
		digit = spoken
		if d, ok := words[spoken]; ok {
			digit = d
		}
		best = rank
	}
	for _, m := range optionAfter.FindAllStringSubmatch(v, -1) {
		consider(m[1], m[2])
	}
	matches := optionBefore.FindAllStringSubmatchIndex(v, -1)
	for i, m := range matches {
		end := len(v)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		label := v[m[1]:end]
		if n := strings.IndexAny(label, ",.;!?"); n >= 0 {
			label = label[:n]
		}
		consider(label, v[m[2]:m[3]])
	}
	if digit != "" {
		return navigationChoice{"digits", digit}
	}
	for _, phrase := range []string{"please hold", "on hold", "please wait", "your call is important", "all our agents", "all representatives", "connecting you", "transferring you", "next available"} {
		if strings.Contains(v, phrase) {
			return navigationChoice{"wait", ""}
		}
	}
	if v == "" {
		return navigationChoice{"wait", ""}
	}
	if menuPrompt.MatchString(v) && !(requested && personGreeting.MatchString(v) && !strings.Contains(v, "press") && !strings.Contains(v, "say")) {
		if strings.Contains(v, "say") || strings.Contains(v, "tell me") || strings.Contains(v, "direct your call") || strings.Contains(v, "how can i help") {
			return navigationChoice{"request", ""}
		}
		return navigationChoice{"unsupported", ""}
	}
	if personGreeting.MatchString(v) || (requested && (v == "front desk" || v == "customer service" || v == "yes")) {
		return navigationChoice{"person", ""}
	}
	return navigationChoice{"request", ""}
}
func (s *server) navigationGather(c *call, prefix string) string {
	action := s.cfg.Base + "/navigate?" + url.Values{"run": {s.cfg.Run}, "target": {c.Target.ID}, "step": {strconv.Itoa(c.NavStep)}}.Encode()
	// DTMF playback must be outside Gather. Gather transcribes only the response.
	return prefix + "<Gather input=\"speech\" timeout=\"5\" speechTimeout=\"2\" actionOnEmptyResult=\"true\" method=\"POST\" action=\"" + escape(action) + "\"/>" + goodbye
}
func (s *server) navigate(w http.ResponseWriter, r *http.Request) {
	t, ok := s.authorize(w, r)
	if !ok {
		return
	}
	step, err := strconv.Atoi(r.URL.Query().Get("step"))
	if err != nil || step < 0 {
		http.Error(w, "invalid step", 400)
		return
	}
	s.mu.Lock()
	c := s.calls[r.PostForm.Get("CallSid")]
	if c == nil || c.Target.ID != t.ID || c.Ended {
		s.mu.Unlock()
		twiml(w, goodbye)
		return
	}
	if step == c.NavStep-1 && c.NavReply != "" {
		reply := c.NavReply
		s.mu.Unlock()
		twiml(w, reply)
		return
	}
	if !c.Navigating || step != c.NavStep {
		s.mu.Unlock()
		http.Error(w, "unexpected navigation step", 409)
		return
	}
	choice := navigationAction(r.PostForm.Get("SpeechResult"), c.NavRequested)
	reason := ""
	if time.Now().After(c.NavDeadline) || time.Now().After(c.Deadline) || c.NavStep >= navigationTurns {
		reason = "navigation_limit"
	}
	if choice.kind == "voicemail" {
		reason = "voicemail"
	}
	if (choice.kind == "digits" || choice.kind == "request") && c.NavActions >= navigationSelections {
		reason = "navigation_limit"
	}
	if choice.kind == "unsupported" {
		reason = "unsupported_menu"
	}
	c.NavStep++
	if c.NavStep >= navigationTurns && choice.kind != "person" {
		reason = "navigation_limit"
	}
	prefix := ""
	if reason == "" {
		switch choice.kind {
		case "digits":
			c.NavActions++
			prefix = "<Play digits=\"ww" + escape(choice.value) + "\"/>"
		case "request":
			c.NavActions++
			c.NavRequested = true
			prefix = "<Say>Front desk or customer service, please.</Say>"
		case "person":
			c.Navigating = false
		}
	}
	reply := s.navigationGather(c, prefix)
	if choice.kind == "person" && reason == "" {
		reply = s.gather(t, false)
	}
	if reason != "" {
		reply = goodbye
	}
	c.NavReply = reply
	s.mu.Unlock()
	// Keep action evidence, not the unconsented speech or a pre-consent transcript.
	if !s.emit(c, "summary", map[string]any{"phase": "navigation", "action": choice.kind, "digit": choice.value, "reason": reason}) {
		s.finish(c, "event_delivery_failed")
		twiml(w, goodbye)
		return
	}
	if reason != "" {
		s.finish(c, reason)
	}
	twiml(w, reply)
}
