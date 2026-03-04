package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DanielFillol/Jarvis/internal/config"
)

// Client provides a thin wrapper around Slack's HTTP APIs used by
// Jarvis.  It encapsulates configuration details such as tokens and
// signing secrets.
type Client struct {
	BotToken          string
	UserToken         string
	SigningSecret     string
	SearchMaxPages    int
	BotUserID         string
	APIBaseURL        string
	HTTPClient        *http.Client
	Tracker           *MessageTracker
	UserTokenUserID   string // user ID of the xoxp token owner (populated by AuthTestUserToken)
	UserTokenUsername string // username handle of the xoxp token owner
}

// NewClient constructs a Slack client from the supplied configuration.  The
// client may later be updated with the BotUserID returned from auth.test.
func NewClient(cfg config.Config) *Client {
	c := &Client{
		BotToken:       cfg.SlackBotToken,
		UserToken:      cfg.SlackUserToken,
		SigningSecret:  cfg.SlackSigningSecret,
		SearchMaxPages: cfg.SlackSearchMaxPages,
		Tracker:        NewMessageTracker(),
		APIBaseURL:     "https://slack.com/api",
	}
	// Authenticate Slack bot to get bot user ID
	if err := c.AuthTest(); err != nil {
		log.Printf("[SLACK] auth.test failed: %v", err)
		return nil
	}

	return c
}

func (c *Client) Do(req *http.Request, timeout time.Duration) (*http.Response, error) {
	if c.HTTPClient != nil {
		return c.HTTPClient.Do(req)
	}
	client := &http.Client{Timeout: timeout}
	return client.Do(req)
}

type Auth struct {
	OK     bool   `json:"ok"`
	UserID string `json:"user_id"`
	User   string `json:"user"`
	Error  string `json:"error"`
}

func (c *Client) call(token string) (*Auth, error) {
	req, _ := http.NewRequest("POST", strings.TrimRight(c.APIBaseURL, "/")+"/auth.test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(req, 10*time.Second)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out Auth
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if err = json.Unmarshal(b, &out); err != nil {
		return nil, err
	}

	if !out.OK {
		return nil, fmt.Errorf("auth.test error: %s", out.Error)
	}

	return &out, nil
}

// AuthTest calls Slack's auth.test API to retrieve the bot user ID. On
// success the BotUserID field of the client is updated, and the ID is
// returned.
func (c *Client) AuthTest() error {
	if c.BotToken == "" {
		return errors.New("missing Slack bot token")
	}

	bot, err := c.call(c.BotToken)
	if err != nil {
		return err
	}

	user, err := c.call(c.UserToken)
	if err != nil {
		return err
	}

	c.BotUserID = bot.UserID
	c.UserTokenUserID = user.UserID
	c.UserTokenUsername = user.User
	return nil
}

// VerifySignature verifies the Slack signing signature on the incoming
// request.  It returns an error if the signature is invalid or the
// timestamp is stale.  The raw body bytes must be the exact bytes used
// to compute the signature.
func (c *Client) VerifySignature(r *http.Request, body []byte) error {
	if c.SigningSecret == "" {
		return errors.New("missing Slack signing secret")
	}

	ts := r.Header.Get("X-Slack-Request-Timestamp")
	sig := r.Header.Get("X-Slack-Signature")
	if ts == "" || sig == "" {
		return errors.New("missing slack signature headers")
	}

	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("invalid timestamp")
	}

	if time.Since(time.Unix(tsInt, 0)) > 5*time.Minute {
		return errors.New("stale timestamp")
	}

	base := "v0:" + ts + ":" + string(body)
	mac := hmac.New(sha256.New, []byte(c.SigningSecret))
	_, err = mac.Write([]byte(base))
	if err != nil {
		return errors.New("write to hmac failed")
	}

	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return errors.New("signature mismatch")
	}

	return nil
}
