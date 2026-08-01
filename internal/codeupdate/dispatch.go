package codeupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/JeremyProffittOrg/live-ninja/internal/ghost"
)

// The dispatcher: everything that happens between "the owner said update X" and
// "a coding session is running on their PC".
//
// It lives behind a queue rather than inside the voice tool for one reason: the
// Opus rewrite takes 30–90 s and the web function's timeout is 30 s, so the tool
// physically cannot wait — and more importantly, the owner should not have to.
// Once they have said what they want, hanging up must not cancel the work.
//
// Order matters at two points:
//
//   - The token row is written BEFORE the prompt is built, because the prompt
//     embeds the token; a token in a prompt with no row behind it would give the
//     agent a credential that 401s at every attempt.
//   - The launch happens LAST, after the rewrite has resolved one way or the
//     other. Nothing is dispatched that the owner will not receive an email
//     about.

// preprocessPollInterval / DefaultPreprocessTimeout bound the wait on the Opus
// rewrite. The upstream worker's own ceiling is 600 s, but this one is shorter
// on purpose: past a few minutes the right answer is to launch with the owner's
// own words and tell them, not to keep a queue message in flight.
const (
	preprocessPollInterval   = 5 * time.Second
	DefaultPreprocessTimeout = 240 * time.Second
	emailTemplateStarted     = "code-update-started"
	emailTemplateFailed      = "code-update-failed"
	emailTemplateProgress    = "code-update-progress"
)

// Rewrite notes recorded on the record and reported in the confirmation email.
// The owner is never left to wonder which words were actually sent.
const (
	noteNotRequested = "not requested — launched with your original wording"
	noteTimedOut     = "the Opus rewrite did not finish in time — launched with your original wording"
	noteFailed       = "the Opus rewrite failed — launched with your original wording"
	noteRewriteError = "the Opus rewrite reported an error — launched with your original wording"
	noteQuota        = "the Opus rewrite quota is exhausted — launched with your original wording"
	noteUnavailable  = "prompt preprocessing is not available right now — launched with your original wording"
)

// SQSAPI is the SendMessage subset of the SQS client (email enqueue).
type SQSAPI interface {
	SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

// emailMessage mirrors cmd/email-dispatch's EmailMessage body shape.
type emailMessage struct {
	Template string `json:"template"`
	To       string `json:"to"`
	Subject  string `json:"subject"`
	Text     string `json:"text"`
}

// Dispatcher owns one request from queue message to running session.
type Dispatcher struct {
	Ghost *ghost.Client
	Store *Store
	SQS   SQSAPI

	EmailQueueURL string
	OwnerEmail    string
	ProgressURL   string
	OutputFile    string

	PreprocessTimeout time.Duration
	Log               *slog.Logger

	// Seams so tests do not sleep and do not depend on a wall clock.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

func (d *Dispatcher) ready() error {
	switch {
	case d.Ghost == nil || !d.Ghost.Ready():
		return errors.New("codeupdate: ghost client is not configured")
	case d.Store == nil:
		return errors.New("codeupdate: store is not configured")
	case d.SQS == nil || d.EmailQueueURL == "":
		return errors.New("codeupdate: email queue is not configured")
	case d.OwnerEmail == "":
		return errors.New("codeupdate: owner email is not configured")
	}
	return nil
}

func (d *Dispatcher) log() *slog.Logger {
	if d.Log == nil {
		return slog.Default()
	}
	return d.Log
}

func (d *Dispatcher) now() time.Time {
	if d.Now == nil {
		return time.Now()
	}
	return d.Now()
}

func (d *Dispatcher) sleep(ctx context.Context, dur time.Duration) error {
	if d.Sleep != nil {
		return d.Sleep(ctx, dur)
	}
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (d *Dispatcher) outputFile() string {
	if strings.TrimSpace(d.OutputFile) == "" {
		return DefaultOutputFile
	}
	return d.OutputFile
}

// Dispatch runs one request to completion.
//
// The returned error means TRANSIENT — return the message to the queue. A
// permanent failure (a rejected repo, a denied principal) is reported to the
// owner by email and returns nil, because redelivering it would only produce the
// same rejection and a second email.
func (d *Dispatcher) Dispatch(ctx context.Context, req Request) error {
	if err := d.ready(); err != nil {
		// Configuration, not the request. Transient by nature: a redeploy fixes
		// it and the message should survive to be retried.
		return err
	}
	if req.Version != QueueMessageVersion {
		d.log().Error("codeupdate: unknown queue message version",
			slog.Int("version", req.Version), slog.String("request_id", req.RequestID))
		return nil // a version we cannot read will never become readable
	}

	log := d.log().With(slog.String("request_id", req.RequestID), slog.String("repo", req.Repo))

	// (1) Mint the run token and write its row FIRST — the prompt embeds the
	//     plaintext, and a prompt whose token has no row behind it hands the
	//     agent a credential that can only fail.
	token, hash, err := NewToken(req.RequestID)
	if err != nil {
		d.failed(ctx, req, "could not prepare the update request", log)
		return nil
	}
	switch err := d.Store.PutToken(ctx, TokenRow{
		RequestID: req.RequestID,
		UserID:    req.UserID,
		Repo:      req.Repo,
		TokenHash: hash,
	}); {
	case errors.Is(err, ErrTokenExists):
		// A redelivery of a message already dispatched. Stop here rather than
		// launch again: the create-only write has already refused to revoke the
		// running session's token, and re-launching would either be refused by
		// ghost-cli's in-flight guard or — past its 2h grace — start a SECOND
		// session on the same repo with a token that no longer matches its
		// prompt. Neither is worth a second email.
		log.Warn("codeupdate: duplicate delivery ignored (token already minted)")
		return nil
	case err != nil:
		log.Error("codeupdate: token row write failed", slog.String("error", err.Error()))
		return err // transient: DynamoDB
	}

	// (2) The Opus rewrite. Only the owner's own instructions are ever sent —
	//     never the token, never the directives (see prompt.go).
	body := BuildInstructionPrompt(req.Repo, req.Instructions)
	rewritten := false
	note := noteNotRequested

	if req.Preprocess {
		_ = d.Store.SetStatus(ctx, req.UserID, req.RequestID, StatusUpdate{Status: StatusPreprocessing})
		refined, n, rerr := d.preprocess(ctx, req, body)
		switch {
		case rerr != nil:
			// Reported, never silent: the owner asked for an update, not for a
			// rewrite, so the update proceeds — but the confirmation email says
			// which words were actually sent.
			// The reason is logged as well as noted: the note is written for the
			// owner, and it is deliberately vague about which of several causes
			// applied. Diagnosing a wire-vocabulary drift needs the value.
			log.Warn("codeupdate: preprocessing unavailable",
				slog.String("note", n), slog.String("reason", rerr.Error()))
			note = n
		default:
			body = refined
			rewritten = true
			note = ""
		}
	}

	// (3) Assemble the final prompt and launch.
	prompt := BuildPrompt(PromptInput{
		Repo:        req.Repo,
		OutputFile:  d.outputFile(),
		Deploy:      req.Deploy,
		Token:       token,
		ProgressURL: d.ProgressURL,
	}, body)

	_ = d.Store.SetStatus(ctx, req.UserID, req.RequestID, StatusUpdate{
		Status: StatusLaunching, Rewritten: &rewritten, RewriteNote: note,
	})

	res, err := d.Ghost.Launch(ctx, ghost.LaunchRequest{
		EventID:    EventID(req.Repo),
		Node:       req.Node,
		Repo:       req.Repo,
		CLI:        req.CLI,
		Model:      req.Model,
		Effort:     req.Effort,
		Prompt:     prompt,
		OutputFile: d.outputFile(),
		// The same decision the prompt's deploy rules carry, on the wire where a
		// mechanism can act on it. Prompt text is advice an agent may misread or
		// never receive; this is what lets ghost-cli install the pre-push hook.
		Deploy: req.Deploy,
	}, req.RequestID)
	if err != nil {
		reason := launchFailureReason(err)
		log.Error("codeupdate: launch failed", slog.String("reason", reason))
		d.failed(ctx, req, reason, log)
		if errors.Is(err, ghost.ErrUpstream) {
			// A transport blip or a 5xx is worth one more attempt; everything
			// else would fail identically on redelivery.
			return err
		}
		return nil
	}

	_ = d.Store.SetStatus(ctx, req.UserID, req.RequestID, StatusUpdate{
		Status:  StatusLaunched,
		EventID: res.EventID,
		RunID:   res.Run.RunID,
	})

	log.Info("codeupdate: launched",
		slog.String("node", req.Node), slog.String("cli", req.CLI),
		slog.String("run_id", res.Run.RunID), slog.Bool("rewritten", rewritten),
		slog.Bool("deploy", req.Deploy))

	d.emailStarted(ctx, req, res, prompt, rewritten, note, log)
	return nil
}

// preprocess starts the Opus rewrite and polls it to a conclusion. It returns
// the rewritten prompt, or a human-readable note explaining why there isn't one.
func (d *Dispatcher) preprocess(ctx context.Context, req Request, body string) (string, string, error) {
	jobID, err := d.Ghost.Preprocess(ctx, ghost.PreprocessRequest{
		Prompt:     body,
		CLI:        req.CLI,
		Model:      req.Model,
		Effort:     req.Effort,
		OutputFile: d.outputFile(),
		Node:       req.Node,
		Repo:       req.Repo,
	}, req.RequestID)
	if err != nil {
		switch {
		case errors.Is(err, ghost.ErrQuota):
			return "", noteQuota, err
		case errors.Is(err, ghost.ErrUnavailable):
			return "", noteUnavailable, err
		default:
			return "", noteFailed, err
		}
	}

	timeout := d.PreprocessTimeout
	if timeout <= 0 {
		timeout = DefaultPreprocessTimeout
	}
	deadline := d.now().Add(timeout)

	for d.now().Before(deadline) {
		if err := d.sleep(ctx, preprocessPollInterval); err != nil {
			return "", noteTimedOut, err
		}
		st, err := d.Ghost.PreprocessStatus(ctx, jobID, req.RequestID)
		if err != nil {
			// A job that cannot be found is not a job that is running late: the
			// row is created synchronously before the 202, so a 404 means it
			// aged out of its one-hour TTL or was never ours, and no amount of
			// waiting brings it back. Same for a refused poll. Waiting those out
			// reports them to the owner as a timeout, which is the same lie the
			// status-vocabulary bug told.
			if errors.Is(err, ghost.ErrNotFound) || errors.Is(err, ghost.ErrNotAuthorized) {
				return "", noteFailed, fmt.Errorf("preprocess job %s is unreachable: %w", jobID, err)
			}
			// Anything else — a 5xx, a throttle, a transport blip — is one poll
			// failing, not the job failing. Keep polling until the deadline
			// rather than throwing away a rewrite that may be seconds from
			// landing.
			continue
		}
		switch {
		case ghost.PreprocessIs(st.Status, ghost.PreprocessDone):
			// A rewrite that is empty — or that is nothing but ghost-cli's own
			// output directive, which its canonicalizer emits when the model
			// returned no usable body — carries no task at all. Launching it
			// would start a session with operating rules and nothing to do, and
			// every downstream guard would read it as success. Treat it as a
			// failed rewrite and fall back to the owner's own words.
			refined := strings.TrimSpace(st.Prompt)
			if bare := strings.TrimSpace(strings.ReplaceAll(refined,
				OutputDirective(d.outputFile()), "")); bare == "" {
				return "", noteFailed, errors.New("preprocess returned no usable prompt")
			}
			return refined, "", nil
		case ghost.PreprocessIs(st.Status, ghost.PreprocessError):
			// ghost-cli reports its own expired deadline this way too, so the
			// message it chose is the one worth repeating; it is documented as
			// provider-free and safe to carry.
			return "", noteRewriteError, fmt.Errorf("preprocess reported an error: %s",
				strings.TrimSpace(st.Error))
		case ghost.PreprocessIs(st.Status, ghost.PreprocessPending):
			// Still working. Keep polling.
		default:
			// A status neither side's vocabulary contains. Spinning to the
			// deadline would report this as a timeout, which is exactly how the
			// PENDING/pending mismatch hid for a full release — so it fails
			// immediately and names the value it could not read.
			return "", noteFailed, fmt.Errorf(
				"preprocess returned unrecognised status %q (ghost-cli sends %q, %q or %q)",
				st.Status, ghost.PreprocessPending, ghost.PreprocessDone, ghost.PreprocessError)
		}
	}
	return "", noteTimedOut, errors.New("preprocess timed out")
}

// launchFailureReason turns a client error into something worth reading in an
// email at 7am. It deliberately names the fix where there is one.
func launchFailureReason(err error) string {
	switch {
	case errors.Is(err, ghost.ErrNotAuthorized):
		return "Ghost refused the launch: the `live-ninja` principal is not on its signed launch " +
			"allowlist, or has no launch rights on that node. Re-seed the allowlist to fix it."
	case errors.Is(err, ghost.ErrConflict):
		return "An update is already running on that repository. Wait for it to finish, or check " +
			"the cockpit, then ask again."
	case errors.Is(err, ghost.ErrUnavailable):
		return "Ghost accepted the request but cannot dispatch right now (its command publisher " +
			"is not configured)."
	case errors.Is(err, ghost.ErrNotConfigured):
		return "Live Ninja is not wired to Ghost: no command function is configured."
	default:
		return "Ghost could not start the session."
	}
}

// ---------------------------------------------------------------------------
// Email
// ---------------------------------------------------------------------------

func (d *Dispatcher) enqueueEmail(ctx context.Context, template, subject, text string, log *slog.Logger) {
	body, err := json.Marshal(emailMessage{
		Template: template,
		To:       d.OwnerEmail, // never caller-controlled
		Subject:  subject,
		Text:     text,
	})
	if err != nil {
		log.Error("codeupdate: email marshal failed", slog.String("error", err.Error()))
		return
	}
	if _, err := d.SQS.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(d.EmailQueueURL),
		MessageBody: aws.String(string(body)),
	}); err != nil {
		// The launch already happened; losing the notification must not undo it
		// or drag the message back through a second dispatch.
		log.Error("codeupdate: email enqueue failed", slog.String("error", err.Error()))
	}
}

// emailStarted is the record of what a voice command actually authorized. It
// carries the FINAL prompt verbatim, which is the whole point: the owner spoke
// three sentences and a model expanded them, and they are entitled to see
// exactly what was sent to a machine that can change their code.
func (d *Dispatcher) emailStarted(ctx context.Context, req Request, res ghost.LaunchResult,
	prompt string, rewritten bool, note string, log *slog.Logger) {

	deployLine := "Deploy: NO — you asked for this one to be held, so the session will commit " +
		"locally and stop before pushing."
	if req.Deploy {
		deployLine = "Deploy: YES — the session will push through the normal delivery path and " +
			"watch the pipeline. Reply-to-hold is not a thing: say \"don't push\" up front if " +
			"you want a change staged instead."
	}
	rewriteLine := "Instructions: rewritten by Opus before launch."
	if !rewritten {
		rewriteLine = "Instructions: " + note
	}

	var b strings.Builder
	fmt.Fprintf(&b, "A code update is now running.\n\n")
	fmt.Fprintf(&b, "Repository: %s\n", req.Repo)
	fmt.Fprintf(&b, "Machine:    %s\n", req.Node)
	fmt.Fprintf(&b, "Agent:      %s", req.CLI)
	if req.Model != "" {
		fmt.Fprintf(&b, " (%s)", req.Model)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Run:        %s\n", res.Run.RunID)
	fmt.Fprintf(&b, "Requested:  %s\n\n", req.RequestedAt)
	fmt.Fprintf(&b, "%s\n%s\n\n", deployLine, rewriteLine)
	fmt.Fprintf(&b, "What you asked for:\n%s\n\n", strings.TrimSpace(req.Instructions))
	fmt.Fprintf(&b, "The exact prompt the session received:\n\n%s\n\n", prompt)
	fmt.Fprintf(&b, "You will get progress emails as it works, and a summary when it finishes.\n")

	d.enqueueEmail(ctx, emailTemplateStarted,
		fmt.Sprintf("[%s] update started", req.Repo), b.String(), log)
}

// failed records the failure and tells the owner. A voice request that quietly
// evaporates is the worst outcome here — they asked out loud and would have no
// reason to check.
func (d *Dispatcher) failed(ctx context.Context, req Request, reason string, log *slog.Logger) {
	if err := d.Store.SetStatus(ctx, req.UserID, req.RequestID, StatusUpdate{
		Status: StatusFailed, Error: reason,
	}); err != nil {
		log.Error("codeupdate: could not record failure", slog.String("error", err.Error()))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The code update you asked for did not start.\n\n")
	fmt.Fprintf(&b, "Repository: %s\n", req.Repo)
	fmt.Fprintf(&b, "Machine:    %s\n", req.Node)
	fmt.Fprintf(&b, "Requested:  %s\n\n", req.RequestedAt)
	fmt.Fprintf(&b, "Reason: %s\n\n", reason)
	fmt.Fprintf(&b, "What you asked for:\n%s\n", strings.TrimSpace(req.Instructions))

	d.enqueueEmail(ctx, emailTemplateFailed,
		fmt.Sprintf("[%s] update did NOT start", req.Repo), b.String(), log)
}

// EmailProgress delivers one mid-run report. It is on the Dispatcher because
// the web function's progress route needs exactly this and nothing else from it.
func (d *Dispatcher) EmailProgress(ctx context.Context, repo, status, summary string, remaining int, log *slog.Logger) {
	var b strings.Builder
	fmt.Fprintf(&b, "Progress on %s — %s\n\n", repo, status)
	fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(summary))
	fmt.Fprintf(&b, "(%d further progress reports allowed for this run.)\n", remaining)

	d.enqueueEmail(ctx, emailTemplateProgress,
		fmt.Sprintf("[%s] %s", repo, status), b.String(), log)
}
