package slack

import "encoding/json"

// EventEnvelope represents the outer wrapper for events sent by the
// Slack Events API.  See https://api.slack.com/apis/connections/events-api
// for details.  Only the fields used by this application are defined
// here.
type EventEnvelope struct {
	Type      string          `json:"type"`
	Challenge string          `json:"challenge,omitempty"`
	Event     json.RawMessage `json:"event,omitempty"`
}

// File represents a file attached to a Slack message.
type File struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Mimetype           string `json:"mimetype"`
	Filetype           string `json:"filetype"`
	Size               int64  `json:"size"`
	URLPrivateDownload string `json:"url_private_download"`
}

// MessageEvent represents a Slack message event.  It omits fields
// that are not currently used by this application.
type MessageEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype,omitempty"`
	Text      string `json:"text"`
	User      string `json:"user,omitempty"`
	BotID     string `json:"bot_id,omitempty"`
	Channel   string `json:"channel"`
	Ts        string `json:"ts"`
	ThreadTs  string `json:"thread_ts,omitempty"`
	DeletedTs string `json:"deleted_ts,omitempty"`
	Files     []File `json:"files,omitempty"`
	// Message is populated for message_changed events (e.g., edits and tombstone deletions in DMs).
	Message *InnerMessage `json:"message,omitempty"`
}

// InnerMessage is the nested "message" object inside message_changed events.
type InnerMessage struct {
	Ts      string `json:"ts"`
	Subtype string `json:"subtype,omitempty"`
	Text    string `json:"text"`
	Hidden  bool   `json:"hidden,omitempty"`
}

// PostMessageRequest encapsulates the body of a chat.postMessage call.
type PostMessageRequest struct {
	Channel  string `json:"channel"`
	Text     string `json:"text"`
	ThreadTs string `json:"thread_ts,omitempty"`
}

// SearchMessagesResp models the Slack search.messages response used
// by this application.  Only the fields accessed by the code are
// represented here.
type SearchMessagesResp struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Messages struct {
		Total  int `json:"total"`
		Paging struct {
			Count int `json:"count"`
			Total int `json:"total"`
			Page  int `json:"page"`
			Pages int `json:"pages"`
		} `json:"paging"`
		Matches []struct {
			Text      string `json:"text"`
			Permalink string `json:"permalink"`
			Channel   struct {
				Name string `json:"name"`
			} `json:"channel"`
			User     string  `json:"user"`
			Username string  `json:"username"`
			Ts       string  `json:"ts"`
			Score    float64 `json:"score"`
		} `json:"matches"`
	} `json:"messages"`
}

// SearchMessage is an internal representation of a search message
// result used by higher layers in the application.  It flattens
// certain fields and normalizes names.
type SearchMessage struct {
	Text      string
	Permalink string
	Channel   string
	UserID    string // Slack user ID, e.g. "U067UM4RGB"
	Username  string
	Ts        string
	Score     float64
}

// ChannelInfo holds a minimal channel summary returned by ListChannels.
type ChannelInfo struct {
	ID   string
	Name string
}
