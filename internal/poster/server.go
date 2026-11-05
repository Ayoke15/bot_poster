package poster

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

type Chat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
}
type Message struct {
	ID   int64 `json:"message_id"`
	Chat Chat  `json:"chat"`
	From *struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Text    string `json:"text"`
	Caption string `json:"caption"`
}
type Update struct {
	ID      *int64   `json:"update_id"`
	Message *Message `json:"message"`
	Post    *Message `json:"channel_post"`
	Edited  *Message `json:"edited_channel_post"`
}
type Server struct {
	Config    Config
	Store     *Store
	API       *API
	BotID     int64
	Log       *slog.Logger
	mu        sync.RWMutex
	webhookOK bool
	webhookAt time.Time
	workerAt  time.Time
}

func NewServer(c Config, s *Store, a *API, log *slog.Logger) *Server {
	return &Server{Config: c, Store: s, API: a, Log: log}
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "TG → VK Autoposter (Go)")
	})
	mux.HandleFunc("GET /live", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"ok": true}) })
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("POST /telegram/webhook/{secret}", s.webhook)
	return mux
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func equalSecret(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	dbOK := s.Store.Pool.Ping(ctx) == nil
	s.mu.RLock()
	wh := s.webhookOK && time.Since(s.webhookAt) < 3*time.Minute
	worker := !s.workerAt.IsZero() && time.Since(s.workerAt) < time.Minute
	s.mu.RUnlock()
	ok := dbOK && wh && worker
	status := 200
	if !ok {
		status = 503
	}
	writeJSON(w, status, map[string]any{"ok": ok, "database": dbOK, "webhook": wh, "worker": worker})
}
func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	if !equalSecret(r.PathValue("secret"), s.Config.WebhookSecret) || !equalSecret(r.Header.Get("X-Telegram-Bot-Api-Secret-Token"), s.Config.WebhookSecret) {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()
	var u Update
	dec := json.NewDecoder(r.Body)
	if dec.Decode(&u) != nil || u.ID == nil || *u.ID < 0 {
		http.Error(w, "invalid update", 400)
		return
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		http.Error(w, "invalid update", 400)
		return
	}
	if e := s.accept(r.Context(), u); e != nil {
		s.Log.Error("webhook transaction failed", "update_id", *u.ID)
		http.Error(w, "temporary failure", 503)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) accept(ctx context.Context, u Update) error {
	tx, e := s.Store.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	// The unique event insert waits for concurrent deliveries to commit or roll back.
	tag, e := tx.Exec(ctx, "INSERT INTO poster_events(update_id) VALUES($1) ON CONFLICT DO NOTHING", *u.ID)
	if e != nil {
		return e
	}
	if tag.RowsAffected() == 0 {
		return tx.Commit(ctx)
	}
	if u.Message != nil && u.Message.Chat.Type == "private" && u.Message.From != nil && u.Message.From.ID > 0 && u.Message.ID > 0 && u.Message.Chat.ID == u.Message.From.ID {
		if e = s.command(ctx, tx, *u.Message); e != nil {
			return e
		}
	} else if u.Post != nil || u.Edited != nil {
		m := u.Post
		edited := false
		if m == nil {
			m = u.Edited
			edited = true
		}
		if m.Chat.Type != "channel" || m.ID <= 0 {
			return tx.Commit(ctx)
		}
		body := m.Text
		if body == "" {
			body = m.Caption
		}
		if !edited && body == "" {
			return tx.Commit(ctx)
		}
		if edited {
			// Never create a new publication for an edit of a pre-connection/unseen post.
			_, e = tx.Exec(ctx, `UPDATE poster_posts SET body=$3,desired_version=$4,
      status=CASE WHEN status='processing' THEN status ELSE 'pending' END,
      attempts=CASE WHEN status='processing' THEN attempts ELSE 0 END,next_attempt_at=now()
      WHERE chat_id=$1 AND message_id=$2 AND desired_version<$4 AND status NOT IN ('cancelled','failed')`, m.Chat.ID, m.ID, body, *u.ID)
		} else {
			var owner, group, version int64
			e = tx.QueryRow(ctx, `SELECT c.owner_id,u.group_id,u.token_version FROM poster_channels c JOIN poster_users u ON u.user_id=c.owner_id
      WHERE c.chat_id=$1 AND u.enabled AND u.token_cipher IS NOT NULL AND u.group_id IS NOT NULL FOR SHARE OF u`, m.Chat.ID).Scan(&owner, &group, &version)
			if errors.Is(e, pgx.ErrNoRows) {
				return tx.Commit(ctx)
			}
			if e != nil {
				return e
			}
			h := sha256.Sum256([]byte(intString(s.BotID) + ":" + intString(m.Chat.ID) + ":" + intString(m.ID)))
			_, e = tx.Exec(ctx, `INSERT INTO poster_posts(chat_id,message_id,owner_id,group_id,token_version,body,desired_version,guid)
      VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(chat_id,message_id) DO NOTHING`, m.Chat.ID, m.ID, owner, group, version, body, *u.ID, hex.EncodeToString(h[:16]))
		}
		if e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
func (s *Server) checkAdmin(ctx context.Context, chat, user int64) error {
	var info Chat
	if e := s.API.Telegram(ctx, "getChat", map[string]any{"chat_id": chat}, &info); e != nil {
		return e
	}
	if info.Type != "channel" {
		return errors.New("not channel")
	}
	role, e := s.API.Member(ctx, chat, user)
	if e != nil {
		return e
	}
	if role != "creator" && role != "administrator" {
		return errors.New("not admin")
	}
	role, e = s.API.Member(ctx, chat, s.BotID)
	if e != nil {
		return e
	}
	if role != "administrator" {
		return errors.New("bot not admin")
	}
	return nil
}
func (s *Server) RunWebhookMonitor(ctx context.Context) {
	for {
		s.syncWebhook(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}
func (s *Server) syncWebhook(ctx context.Context) {
	var me struct {
		ID int64 `json:"id"`
	}
	e := s.API.Telegram(ctx, "getMe", map[string]any{}, &me)
	if e == nil && me.ID != s.BotID {
		e = errors.New("unexpected bot identity")
	}
	if e == nil {
		e = s.API.Telegram(ctx, "setWebhook", map[string]any{"url": s.Config.BaseURL + "/telegram/webhook/" + s.Config.WebhookSecret, "secret_token": s.Config.WebhookSecret, "allowed_updates": []string{"message", "channel_post", "edited_channel_post"}}, nil)
	}
	if e == nil {
		var info struct {
			URL           string `json:"url"`
			LastErrorDate int64  `json:"last_error_date"`
		}
		e = s.API.Telegram(ctx, "getWebhookInfo", map[string]any{}, &info)
		if e == nil && (info.URL != s.Config.BaseURL+"/telegram/webhook/"+s.Config.WebhookSecret || info.LastErrorDate > time.Now().Add(-5*time.Minute).Unix()) {
			e = errors.New("webhook delivery unhealthy")
		}
	}
	if e == nil {
		commands := []map[string]string{
			{"command": "start", "description": "Главная и инструкция"},
			{"command": "connect_channel", "description": "Подключить свой канал по ID"},
			{"command": "disconnect_channel", "description": "Отключить свой канал"},
			{"command": "connect_vk", "description": "Подключить сообщество VK"},
			{"command": "set_vk", "description": "Сохранить ID группы и токен"},
			{"command": "status", "description": "Настройки, очередь и ошибки"},
			{"command": "enable", "description": "Включить автопостинг"},
			{"command": "disable", "description": "Выключить автопостинг"},
			{"command": "clear_vk", "description": "Удалить токен VK"},
			{"command": "token_help", "description": "Как получить пользовательский ключ VK"},
		}
		if commandErr := s.API.Telegram(ctx, "setMyCommands", map[string]any{"commands": commands}, nil); commandErr != nil {
			s.Log.Warn("telegram command menu registration failed")
		}
	}
	s.mu.Lock()
	s.webhookOK = e == nil
	s.webhookAt = time.Now()
	s.mu.Unlock()
	if e != nil {
		s.Log.Warn("telegram webhook is not ready", "reason", e.Error())
	}
}
