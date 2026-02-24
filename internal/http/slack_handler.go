// internal/http/slack_handler.go
package http

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/DanielFillol/Jarvis/internal/app"
	"github.com/DanielFillol/Jarvis/internal/parse"
	"github.com/DanielFillol/Jarvis/internal/slack"
)

// SlackHandler handles incoming requests from the Slack Events API.
// It performs signature verification, URL verification and routing
// of message events to the Jarvis service.  Non-message events are
// ignored.
type SlackHandler struct {
	Slack   *slack.Client
	Service *app.Service
}

// NewSlackHandler constructs a new SlackHandler.
func NewSlackHandler(slackClient *slack.Client, service *app.Service) *SlackHandler {
	return &SlackHandler{Slack: slackClient, Service: service}
}

// ServeHTTP implements http.Handler.  It acknowledges requests from
// Slack immediately and delegates processing of message events to the
// service.  Signature verification is performed to ensure requests
// originate from Slack.
func (h *SlackHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	log.Printf("[HTTP] /slack/events method=%s remote=%s ua=%q", r.Method, r.RemoteAddr, r.UserAgent())
	if r.Method != http.MethodPost {
		http.Error(w, "method_not_allowed", http.StatusMethodNotAllowed)
		return
	}

	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("[ERR] read body: %v", err)
		http.Error(w, "bad_request", 400)
		return
	}

	var env slack.SlackEventEnvelope
	if err := json.Unmarshal(rawBody, &env); err != nil {
		log.Printf("[ERR] unmarshal envelope: %v body=%s", err, preview(string(rawBody), 500))
		http.Error(w, "bad_request", 400)
		return
	}

	log.Printf("[SLACK] envelope type=%q body_len=%d", env.Type, len(rawBody))

	if env.Type == "url_verification" {
		log.Printf("[SLACK] url_verification challenge_len=%d", len(env.Challenge))
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(env.Challenge))
		log.Printf("[HTTP] /slack/events url_verification done dur=%s", time.Since(start))
		return
	}

	if err := h.Slack.VerifySignature(r, rawBody); err != nil {
		log.Printf("[SEC] signature verification failed: %v", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	log.Printf("[SEC] signature OK")

	// Ack immediately
	w.WriteHeader(200)
	log.Printf("[HTTP] /slack/events ack sent")

	if env.Type != "event_callback" {
		log.Printf("[SLACK] ignoring non event_callback")
		return
	}

	var msg slack.SlackMessageEvent
	if err := json.Unmarshal(env.Event, &msg); err != nil {
		log.Printf("[ERR] unmarshal event: %v event=%s", err, preview(string(env.Event), 600))
		return
	}

	log.Printf("[SLACK] event type=%q subtype=%q channel=%q user=%q ts=%q thread_ts=%q text_len=%d",
		msg.Type, msg.Subtype, msg.Channel, msg.User, msg.Ts, msg.ThreadTs, len(msg.Text))

	if msg.Type != "message" {
		log.Printf("[SLACK] ignoring non-message event")
		return
	}
	if msg.Subtype != "" || msg.BotID != "" {
		log.Printf("[SLACK] ignoring bot/subtype message subtype=%q bot_id_present=%t", msg.Subtype, msg.BotID != "")
		return
	}

	text := strings.TrimSpace(msg.Text)
	if text == "" {
		log.Printf("[BOT] empty text; ignoring")
		return
	}

	threadTs := msg.ThreadTs
	if threadTs == "" {
		threadTs = msg.Ts
	}
	originTs := msg.Ts

	// (4) Direct messages (channel starting with "D") should not require summon.
	isDM := strings.HasPrefix(msg.Channel, "D")

	// Determine if bot was summoned.
	// - In DMs: always accept.
	// - In channels: accept ONLY on a direct Slack mention (<@BOTID>), no prefixes, no auto-followups.
	summoned := isDM
	if !summoned {
		summoned = parse.LooksLikeDirectMention(text, h.Slack.BotUserID)
	}

	if !summoned {
		log.Printf("[BOT] not summoned; ignoring. text_preview=%q", preview(text, 160))
		return
	}

	question := parse.StripSummon(text, h.Slack.BotUserID)
	if question == "" {
		log.Printf("[BOT] summoned but empty after strip; ignoring")
		return
	}

	log.Printf("[BOT] handling question=%q channel=%q thread=%q originTs=%q user=%q", preview(question, 220), msg.Channel, threadTs, originTs, msg.User)

	go func() {
		if err := h.Service.HandleMessage(msg.Channel, threadTs, originTs, text, question, msg.User); err != nil {
			log.Printf("[ERR] handleQuestion: %v", err)
			_ = h.Slack.PostMessage(msg.Channel, threadTs, "Não consegui gerar a resposta (erro interno).")
		}
	}()

	log.Printf("[HTTP] /slack/events done dur=%s", time.Since(start))
}

// preview truncates a string to at most n runes for logging.
func preview(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
