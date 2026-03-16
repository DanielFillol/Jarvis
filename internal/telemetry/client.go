package telemetry

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/lib/pq"

	"github.com/DanielFillol/Jarvis/internal/config"
)

// Client wraps a PostgreSQL connection for fire-and-forget telemetry writes.
// A nil Client is safe to use — all methods are no-ops.
type Client struct {
	db *sql.DB
}

// Event holds all metrics captured for a single HandleMessage invocation.
type Event struct {
	Channel         string
	ChannelType     string // "dm" | "channel"
	ThreadTs        string
	OriginTs        string
	SenderUserID    string
	Question        string
	Answer          string
	QuestionLen     int
	FileCount       int
	Actions         []string
	JiraSearched    bool
	JiraIssues      int
	JiraError       bool
	MetabaseQueried bool
	MetabaseRows    int
	SlackSearched   bool
	SlackMatches    int
	OutlineSearched bool
	LLMModel        string
	AnswerLen       int
	DurationMs      int
	Success         bool
	ErrorStage      string
	CSVGenerated    bool
}

const migrateSQL = `
CREATE TABLE IF NOT EXISTS events (
    id               BIGSERIAL PRIMARY KEY,
    received_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    channel_id       TEXT        NOT NULL,
    channel_type     TEXT        NOT NULL,
    thread_ts        TEXT,
    origin_ts        TEXT        NOT NULL,
    sender_user_id   TEXT        NOT NULL,
    question_len     INT         DEFAULT 0,
    file_count       INT         DEFAULT 0,
    actions          TEXT[],
    jira_searched    BOOLEAN     DEFAULT FALSE,
    jira_issues      INT         DEFAULT 0,
    jira_error       BOOLEAN     DEFAULT FALSE,
    metabase_queried BOOLEAN     DEFAULT FALSE,
    metabase_rows    INT         DEFAULT 0,
    slack_searched   BOOLEAN     DEFAULT FALSE,
    slack_matches    INT         DEFAULT 0,
    outline_searched BOOLEAN     DEFAULT FALSE,
    llm_model        TEXT,
    answer_len       INT         DEFAULT 0,
    duration_ms      INT,
    success          BOOLEAN     DEFAULT TRUE,
    error_stage      TEXT,
    csv_generated    BOOLEAN     DEFAULT FALSE
);
CREATE INDEX IF NOT EXISTS events_received_at ON events (received_at);
CREATE INDEX IF NOT EXISTS events_sender      ON events (sender_user_id);
CREATE INDEX IF NOT EXISTS events_channel     ON events (channel_id);

CREATE TABLE IF NOT EXISTS conversations (
    id         BIGSERIAL PRIMARY KEY,
    event_id   BIGINT      NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    question   TEXT        NOT NULL,
    answer     TEXT        NOT NULL
);
CREATE INDEX IF NOT EXISTS conversations_event_id ON conversations (event_id);
`

const insertEventSQL = `
INSERT INTO events (
    channel_id, channel_type, thread_ts, origin_ts, sender_user_id,
    question_len, file_count, actions,
    jira_searched, jira_issues, jira_error,
    metabase_queried, metabase_rows,
    slack_searched, slack_matches,
    outline_searched,
    llm_model, answer_len, duration_ms, success, error_stage, csv_generated
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8,
    $9, $10, $11,
    $12, $13,
    $14, $15,
    $16,
    $17, $18, $19, $20, $21, $22
) RETURNING id`

const insertConversationSQL = `
INSERT INTO conversations (event_id, question, answer) VALUES ($1, $2, $3)`

// NewClient connects to PostgreSQL and runs migrations.
// Returns nil (silently) when TELEMETRY_DB_URL is empty, so telemetry is fully optional.
func NewClient(cfg config.Config) *Client {
	dsn := cfg.TelemetryDBURL
	if dsn == "" {
		return nil
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Printf("[TELEMETRY] open failed: %v", err)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Printf("[TELEMETRY] ping failed: %v — telemetry disabled", err)
		return nil
	}
	if _, err := db.ExecContext(ctx, migrateSQL); err != nil {
		log.Printf("[TELEMETRY] migrate failed: %v", err)
		return nil
	}
	log.Printf("[TELEMETRY] connected and migrated")
	return &Client{db: db}
}

// Record inserts e into the events table (and optionally conversations) in a
// background goroutine with a 5 s timeout. It never blocks the caller.
// Safe to call on a nil *Client.
func (c *Client) Record(e Event) {
	if c == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var eventID int64
		err := c.db.QueryRowContext(ctx, insertEventSQL,
			e.Channel, e.ChannelType, nullableStr(e.ThreadTs), e.OriginTs, e.SenderUserID,
			e.QuestionLen, e.FileCount, pq.Array(e.Actions),
			e.JiraSearched, e.JiraIssues, e.JiraError,
			e.MetabaseQueried, e.MetabaseRows,
			e.SlackSearched, e.SlackMatches,
			e.OutlineSearched,
			nullableStr(e.LLMModel), e.AnswerLen, e.DurationMs, e.Success, nullableStr(e.ErrorStage), e.CSVGenerated,
		).Scan(&eventID)
		if err != nil {
			log.Printf("[TELEMETRY] insert event failed: %v", err)
			return
		}

		if e.Question != "" || e.Answer != "" {
			if _, err := c.db.ExecContext(ctx, insertConversationSQL, eventID, e.Question, e.Answer); err != nil {
				log.Printf("[TELEMETRY] insert conversation failed: %v", err)
			}
		}
	}()
}

// Close releases the underlying database connection pool.
func (c *Client) Close() {
	if c == nil {
		return
	}
	_ = c.db.Close()
}

// nullableStr converts an empty string to nil so the column stores NULL.
func nullableStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
