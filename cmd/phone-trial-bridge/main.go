// phone-trial-bridge accepts only signed, allowlisted calls. It cannot create calls.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type target struct{ ID, Name, Phone string }
type config struct {
	Base, Account, Auth, KeyID, KeySecret, Azure, Deployment, AzureKey, Recording, Events, Run string
	Max                                                                                        time.Duration
	Targets                                                                                    map[string]target
}
type call struct {
	SID, Token                                  string
	Target                                      target
	Deadline                                    time.Time
	Attempts                                    int
	Navigating, NavRequested                    bool
	NavStep, NavActions                         int
	NavDeadline                                 time.Time
	NavReply                                    string
	Consented, Connected, Ended                 bool
	ModelStarted, UsageComplete, EvidenceFailed bool
	InputTokens, OutputTokens                   int64
	Close                                       func()
}
type server struct {
	cfg    config
	client *http.Client
	mu     sync.Mutex
	calls  map[string]*call
}

var sidPattern = regexp.MustCompile("^CA[0-9a-fA-F]{32}$")

func loadConfig() (config, error) {
	c := config{Base: strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/"), Account: os.Getenv("TWILIO_ACCOUNT_SID"), Auth: os.Getenv("TWILIO_AUTH_TOKEN"), KeyID: os.Getenv("TWILIO_API_KEY_SID"), KeySecret: os.Getenv("TWILIO_API_KEY_SECRET"), Azure: strings.TrimRight(os.Getenv("AZURE_OPENAI_ENDPOINT"), "/"), Deployment: os.Getenv("AZURE_OPENAI_DEPLOYMENT"), AzureKey: os.Getenv("AZURE_OPENAI_API_KEY"), Recording: os.Getenv("RECORDING_CALLBACK_URL"), Events: os.Getenv("EVENT_CALLBACK_URL"), Run: os.Getenv("RUN_ID"), Max: 300 * time.Second, Targets: map[string]target{}}
	for _, v := range []string{c.Base, c.Account, c.Auth, c.KeyID, c.KeySecret, c.Azure, c.Deployment, c.AzureKey, c.Recording, c.Events, c.Run} {
		if v == "" {
			return c, errors.New("missing required configuration")
		}
	}
	for _, v := range []string{c.Base, c.Azure, c.Recording, c.Events} {
		u, e := url.Parse(v)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
			return c, errors.New("invalid HTTPS configuration")
		}
	}
	if v := os.Getenv("MAX_CALL_SECONDS"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n < 30 || n > 600 {
			return c, errors.New("MAX_CALL_SECONDS must be 30..600")
		}
		c.Max = time.Duration(n) * time.Second
	}
	var ts []target
	if json.Unmarshal([]byte(os.Getenv("TARGETS_JSON")), &ts) != nil || len(ts) == 0 || len(ts) > 20 {
		return c, errors.New("invalid targets")
	}
	phones := map[string]bool{}
	for _, t := range ts {
		if t.ID == "" || t.Name == "" || !regexp.MustCompile("^\\+1[0-9]{10}$").MatchString(t.Phone) || phones[t.Phone] || c.Targets[t.ID].ID != "" {
			return c, errors.New("invalid or duplicate target")
		}
		c.Targets[t.ID] = t
		phones[t.Phone] = true
	}
	return c, nil
}
func main() {
	probe := flag.Bool("probe", false, "verify Azure audio without making a telephone call")
	flag.Parse()
	c, e := loadConfig()
	if e != nil {
		log.Fatal(e)
	}
	if *probe {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if e := probeAzure(ctx, c); e != nil {
			log.Fatal(e)
		}
		log.Print("Azure audio probe passed")
		return
	}
	s := &server{cfg: c, client: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, calls: map[string]*call{}}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Print("phone bridge ready")
	log.Fatal((&http.Server{Addr: ":" + port, Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}).ListenAndServe())
}
func (s *server) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "{\"ready\":true}")
	})
	m.HandleFunc("/voice", s.voice)
	m.HandleFunc("/consent", s.consent)
	m.HandleFunc("/navigate", s.navigate)
	m.HandleFunc("/media/", s.media)
	return m
}
func signature(token, uri string, p url.Values) string {
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(uri)
	for _, k := range keys {
		values := append([]string(nil), p[k]...)
		sort.Strings(values)
		last := ""
		for i, v := range values {
			if i == 0 || v != last {
				b.WriteString(k)
				b.WriteString(v)
			}
			last = v
		}
	}
	h := hmac.New(sha1.New, []byte(token))
	h.Write([]byte(b.String()))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
func (s *server) signed(r *http.Request, ws bool) bool {
	uri := s.cfg.Base + r.URL.RequestURI()
	p := r.PostForm
	if ws {
		p = nil
	}
	supplied := r.Header.Get("X-Twilio-Signature")
	if supplied == "" {
		return false
	}
	if hmac.Equal([]byte(signature(s.cfg.Auth, uri, p)), []byte(supplied)) {
		return true
	}
	return ws && hmac.Equal([]byte(signature(s.cfg.Auth, uri+"/", p)), []byte(supplied))
}
func (s *server) authorize(w http.ResponseWriter, r *http.Request) (target, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", 405)
		return target{}, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	if r.ParseForm() != nil || !s.signed(r, false) || r.PostForm.Get("AccountSid") != s.cfg.Account || !sidPattern.MatchString(r.PostForm.Get("CallSid")) || r.URL.Query().Get("run") != s.cfg.Run {
		http.Error(w, "forbidden", 403)
		return target{}, false
	}
	t, ok := s.cfg.Targets[r.URL.Query().Get("target")]
	if !ok || r.PostForm.Get("To") != t.Phone {
		http.Error(w, "forbidden", 403)
		return target{}, false
	}
	return t, true
}
func escape(v string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(v))
	return b.String()
}
func twiml(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/xml")
	io.WriteString(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?><Response>"+body+"</Response>")
}

const goodbye = "<Say>Thank you for your time. Goodbye!</Say><Hangup/>"

func (s *server) gather(t target, clarify bool) string {
	q := "Hi! I'm Jeremy's AI assistant, helping plan his world-domination tour, starting with hotel Wi-Fi his work VPN will tolerate. Could you help with a quick question? May I record our chat so Jeremy can listen later? Please say yes or press 1 if you agree."
	if clarify {
		q = "May I record this conversation for Jeremy? Please say yes or press 1 to agree, or say no to end the call."
	}
	action := s.cfg.Base + "/consent?" + url.Values{"run": {s.cfg.Run}, "target": {t.ID}}.Encode()
	return "<Gather input=\"speech dtmf\" numDigits=\"1\" timeout=\"6\" speechTimeout=\"auto\" actionOnEmptyResult=\"true\" method=\"POST\" action=\"" + escape(action) + "\"><Say>" + escape(q) + "</Say></Gather>" + goodbye
}
func (s *server) voice(w http.ResponseWriter, r *http.Request) {
	t, ok := s.authorize(w, r)
	if !ok {
		return
	}
	sid := r.PostForm.Get("CallSid")
	s.mu.Lock()
	if _, exists := s.calls[sid]; exists {
		s.mu.Unlock()
		twiml(w, goodbye)
		return
	}
	token := make([]byte, 24)
	if _, e := rand.Read(token); e != nil {
		s.mu.Unlock()
		http.Error(w, "unavailable", 503)
		return
	}
	c := &call{SID: sid, Token: hex.EncodeToString(token), Target: t, Deadline: time.Now().Add(s.cfg.Max), Navigating: true, NavDeadline: time.Now().Add(navigationLimit)}
	s.calls[sid] = c
	s.mu.Unlock()
	time.AfterFunc(s.cfg.Max, func() { s.finish(c, "duration_limit") })
	time.AfterFunc(navigationLimit, func() {
		s.mu.Lock()
		navigating := c.Navigating && !c.Ended
		s.mu.Unlock()
		if navigating {
			s.finish(c, "navigation_limit")
		}
	})
	if !s.emit(c, "call_registered", map[string]any{"hotel": t.Name, "phone": t.Phone}) {
		s.finish(c, "event_delivery_failed")
		twiml(w, goodbye)
		return
	}
	twiml(w, s.navigationGather(c, ""))
}
func consentAnswer(speech, digit string) string {
	if digit == "1" {
		return "yes"
	}
	if digit != "" {
		return "no"
	}
	v := strings.Trim(strings.ToLower(strings.TrimSpace(speech)), ".!?,")
	switch v {
	case "yes", "yes please", "yes you may", "yes that's fine", "yes that is fine", "sure", "sure go ahead", "go ahead", "i agree", "okay", "ok", "absolutely":
		return "yes"
	case "no", "no thanks", "no thank you", "not interested", "do not record", "don't record":
		return "no"
	}
	return "unknown"
}
func (s *server) consent(w http.ResponseWriter, r *http.Request) {
	t, ok := s.authorize(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	c := s.calls[r.PostForm.Get("CallSid")]
	if c == nil || c.Target.ID != t.ID || c.Ended || c.Consented || c.Navigating || time.Now().After(c.Deadline) {
		s.mu.Unlock()
		twiml(w, goodbye)
		return
	}
	answer := consentAnswer(r.PostForm.Get("SpeechResult"), r.PostForm.Get("Digits"))
	c.Attempts++
	if answer == "unknown" && c.Attempts < 2 {
		s.mu.Unlock()
		twiml(w, s.gather(t, true))
		return
	}
	if answer != "yes" {
		s.mu.Unlock()
		s.emit(c, "consent", map[string]any{"result": "declined", "answer": answer, "method": "speech"})
		s.finish(c, "consent_declined")
		twiml(w, goodbye)
		return
	}
	c.Consented = true
	s.mu.Unlock()
	method := "speech"
	if r.PostForm.Get("Digits") == "1" {
		method = "dtmf"
	}
	if !s.emit(c, "consent", map[string]any{"result": "granted", "method": method, "notice": "AI identity and recording permission requested before recording"}) {
		s.finish(c, "event_delivery_failed")
		twiml(w, goodbye)
		return
	}
	data := url.Values{"RecordingTrack": {"both"}, "RecordingChannels": {"dual"}, "RecordingStatusCallback": {s.cfg.Recording}, "RecordingStatusCallbackMethod": {"POST"}, "RecordingStatusCallbackEvent": {"completed", "absent"}}
	if s.twilio(c, "/Recordings.json", data) != nil {
		s.emit(c, "error", map[string]any{"code": "recording_start_failed"})
		s.finish(c, "recording_start_failed")
		twiml(w, goodbye)
		return
	}
	stream := strings.Replace(s.cfg.Base, "https://", "wss://", 1) + "/media/" + c.Token
	twiml(w, "<Say>Thank you. Recording starts now.</Say><Connect><Stream url=\""+escape(stream)+"\"/></Connect>"+goodbye)
}
func (s *server) twilio(c *call, suffix string, data url.Values) error {
	uri := "https://api.twilio.com/2010-04-01/Accounts/" + url.PathEscape(s.cfg.Account) + "/Calls/" + c.SID + suffix
	req, e := http.NewRequest("POST", uri, strings.NewReader(data.Encode()))
	if e != nil {
		return e
	}
	req.SetBasicAuth(s.cfg.KeyID, s.cfg.KeySecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, e := s.client.Do(req)
	if e != nil {
		return errors.New("twilio transport failed")
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 65536))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("twilio status %d", resp.StatusCode)
	}
	return nil
}
func (s *server) emit(c *call, kind string, data any) (ok bool) {
	defer func() {
		if !ok {
			s.mu.Lock()
			c.EvidenceFailed = true
			s.mu.Unlock()
		}
	}()
	body, _ := json.Marshal(map[string]any{"runId": s.cfg.Run, "callSid": c.SID, "targetId": c.Target.ID, "type": kind, "at": time.Now().UTC().Format(time.RFC3339Nano), "data": data})
	mac := hmac.New(sha256.New, []byte(s.cfg.Auth))
	mac.Write(body)
	req, e := http.NewRequest("POST", s.cfg.Events, bytes.NewReader(body))
	if e != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-LN-Signature", hex.EncodeToString(mac.Sum(nil)))
	resp, e := s.client.Do(req)
	if e != nil {
		log.Print("event delivery failed")
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Print("event delivery rejected")
		return false
	}
	return true
}
func (s *server) finish(c *call, reason string) {
	s.mu.Lock()
	if c.Ended {
		s.mu.Unlock()
		return
	}
	c.Ended = true
	complete := !c.ModelStarted || c.UsageComplete
	if c.EvidenceFailed {
		complete = false
	}
	data := map[string]any{"reason": reason, "inputTokens": c.InputTokens, "outputTokens": c.OutputTokens, "usageComplete": complete}
	closeStream := c.Close
	s.mu.Unlock()
	if closeStream != nil {
		closeStream()
	}
	// Deliver the authoritative usage snapshot before the provider hangup request.
	// A missing/rejected final event must make the runner retain its full reservation.
	s.emit(c, "call_ended", data)
	if s.twilio(c, ".json", url.Values{"Twiml": {"<Response>" + goodbye + "</Response>"}}) != nil {
		log.Print("call cleanup request failed")
	}
}
