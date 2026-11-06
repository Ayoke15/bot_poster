package poster

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTokenEncryption(t *testing.T) {
	c, e := NewTokenCipher(bytes.Repeat([]byte{7}, 32))
	if e != nil {
		t.Fatal(e)
	}
	a, e := c.Encrypt(123, "secret-token")
	if e != nil {
		t.Fatal(e)
	}
	b, _ := c.Encrypt(123, "secret-token")
	if a == b || strings.Contains(a, "secret-token") {
		t.Fatal("nonce reuse or plaintext")
	}
	v, e := c.Decrypt(123, a)
	if e != nil || v != "secret-token" {
		t.Fatal("round trip", e)
	}
	if _, e = c.Decrypt(124, a); e == nil {
		t.Fatal("cross-user decrypt succeeded")
	}
	other, _ := NewTokenCipher(bytes.Repeat([]byte{8}, 32))
	if _, e = other.Decrypt(123, a); e == nil {
		t.Fatal("wrong key accepted")
	}
	for _, bad := range []string{"plaintext", "v1:", a[:len(a)-5] + "AAAAA"} {
		if _, e = c.Decrypt(123, bad); e == nil {
			t.Fatal("bad ciphertext accepted")
		}
	}
}
func TestParseGroup(t *testing.T) {
	for input, want := range map[string]int64{"123": 123, "https://vk.com/club123": 123, "vk.com/public456": 456, "https://m.vk.ru/event789": 789, "https://vk.com/im?sel=-123": 123} {
		got, e := parseGroup(input)
		if e != nil || got != want {
			t.Fatalf("%q: %d %v", input, got, e)
		}
	}
	for _, input := range []string{"-1", "0", "https://evil.com/club123", "vk.com/name", "https://vk.com/club0", "99999999999999999999999999"} {
		if _, e := parseGroup(input); e == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}
func TestConfig(t *testing.T) {
	for k, v := range map[string]string{"DATABASE_URL": "postgresql://localhost/test", "TELEGRAM_BOT_TOKEN": "1:test", "TELEGRAM_WEBHOOK_SECRET": "development-secret", "APP_BASE_URL": "https://example.com", "TOKEN_ENCRYPTION_KEY": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))} {
		t.Setenv(k, v)
	}
	if _, e := LoadConfig(); e != nil {
		t.Fatal(e)
	}
	t.Setenv("TOKEN_ENCRYPTION_KEY", "short")
	if _, e := LoadConfig(); e == nil {
		t.Fatal("weak key accepted")
	}
}

type fakeAPI struct {
	mu                           sync.Mutex
	role                         string
	vkCode                       int
	groupID                      int64
	posts, edits, deletes, sends int
	last                         url.Values
	guids                        []string
	webhookBad                   bool
	notifyBad                    bool
	acceptThenFail               bool
	verifyDenied                 bool
	verifyTemporary              bool
	published                    map[string]int64
	tg                           *httptest.Server
}

func (f *fakeAPI) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.Contains(r.URL.Path, "/method/") {
		_ = r.ParseForm()
		f.last = r.PostForm
		switch strings.TrimPrefix(r.URL.Path, "/method/") {
		case "account.getAppPermissions":
			io.WriteString(w, `{"response":8192}`)
			return
		case "groups.getById":
			if f.verifyTemporary {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if f.verifyDenied {
				io.WriteString(w, `{"response":{"groups":[{"id":99,"can_post":0,"wall":1,"is_admin":1,"admin_level":3}]}}`)
				return
			}
			if r.Form.Get("group_ids") == "" {
				http.Error(w, "selected group must be specified", 400)
				return
			}
			group := f.groupID
			if group == 0 {
				group = 99
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"response": map[string]any{"groups": []map[string]any{{"id": group, "can_post": 1, "wall": 1, "is_admin": 1, "admin_level": 3}}}})
			return
		case "wall.post":
			f.posts++
			f.guids = append(f.guids, r.Form.Get("guid"))
			if f.vkCode != 0 {
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"error_code": f.vkCode, "error_msg": "SECRET MUST NOT LEAK"}})
				return
			}
			if f.published == nil {
				f.published = map[string]int64{}
			}
			f.published[r.Form.Get("guid")] = 700
			if f.acceptThenFail {
				f.acceptThenFail = false
				w.WriteHeader(502)
				return
			}
			io.WriteString(w, `{"response":{"post_id":700}}`)
			return
		case "wall.edit":
			f.edits++
			io.WriteString(w, `{"response":1}`)
			return
		}
		tpanic := r.URL.Path
		http.Error(w, tpanic, 404)
		return
	}
	var payload map[string]any
	_ = json.NewDecoder(r.Body).Decode(&payload)
	switch r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:] {
	case "getMe":
		io.WriteString(w, `{"ok":true,"result":{"id":10}}`)
	case "getChat":
		io.WriteString(w, `{"ok":true,"result":{"id":-100,"type":"channel"}}`)
	case "getChatMember":
		role := f.role
		if payload["user_id"] == float64(10) {
			role = "administrator"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]string{"status": role}})
	case "getWebhookInfo":
		if f.webhookBad {
			io.WriteString(w, `{"ok":false,"error_code":401}`)
		} else {
			io.WriteString(w, `{"ok":true,"result":{"url":"https://example.com/telegram/webhook/development-secret"}}`)
		}
	case "sendMessage":
		f.sends++
		if f.notifyBad {
			io.WriteString(w, `{"ok":false,"error_code":429}`)
		} else {
			io.WriteString(w, `{"ok":true,"result":{}}`)
		}
	case "deleteMessage":
		f.deletes++
		io.WriteString(w, `{"ok":true,"result":true}`)
	default:
		io.WriteString(w, `{"ok":true,"result":true}`)
	}
}
func fixture(t *testing.T, legacy bool) (*Server, *fakeAPI) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	schemaName := "poster_test_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")) + "_" + intString(time.Now().UnixNano())
	if len(schemaName) > 60 {
		schemaName = "poster_test_" + intString(time.Now().UnixNano())
	}
	if _, e = admin.Exec(ctx, "CREATE SCHEMA "+schemaName); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP SCHEMA "+schemaName+" CASCADE"); admin.Close() })
	u, e := url.Parse(dsn)
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schemaName)
	u.RawQuery = q.Encode()
	c := Config{DatabaseURL: u.String(), BotToken: "10:test-token", WebhookSecret: "development-secret", BaseURL: "https://example.com", Key: bytes.Repeat([]byte{7}, 32), VKVersion: "5.199"}
	if legacy {
		p, e := pgxpool.New(ctx, c.DatabaseURL)
		if e != nil {
			t.Fatal(e)
		}
		_, e = p.Exec(ctx, `CREATE TABLE user_settings(tg_user_id BIGINT PRIMARY KEY,selected_vk_group_id BIGINT,enabled BOOL); CREATE TABLE vk_accounts(tg_user_id BIGINT PRIMARY KEY,access_token TEXT NOT NULL); INSERT INTO user_settings VALUES(1,99,TRUE);INSERT INTO vk_accounts VALUES(1,'legacy-plaintext-token'),(2,'orphan-plaintext-token');`)
		p.Close()
		if e != nil {
			t.Fatal(e)
		}
	}
	store, e := OpenStore(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(store.Pool.Close)
	f := &fakeAPI{role: "administrator"}
	f.tg = httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(f.tg.Close)
	api := NewAPI(c)
	api.HTTP = f.tg.Client()
	api.TelegramURL = f.tg.URL
	api.VKURL = f.tg.URL + "/method"
	s := NewServer(c, store, api, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.BotID = 10
	return s, f
}
func execSQL(t *testing.T, s *Server, sql string, args ...any) {
	t.Helper()
	if _, e := s.Store.Pool.Exec(context.Background(), sql, args...); e != nil {
		t.Fatal(e)
	}
}
func count(t *testing.T, s *Server, sql string, args ...any) int {
	t.Helper()
	var n int
	if e := s.Store.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}
func accept(t *testing.T, s *Server, id int64, m *Message, edited bool) {
	t.Helper()
	u := Update{ID: &id}
	if m.Chat.Type == "private" {
		u.Message = m
	} else if edited {
		u.Edited = m
	} else {
		u.Post = m
	}
	if e := s.accept(context.Background(), u); e != nil {
		t.Fatal(e)
	}
}
func commandMessage(user int64, text string) *Message {
	m := &Message{ID: 1, Chat: Chat{ID: user, Type: "private"}, Text: text}
	_ = json.Unmarshal([]byte(`{"id":`+intString(user)+`}`), &m.From)
	return m
}
func seed(t *testing.T, s *Server) {
	t.Helper()
	cipher, e := s.Store.Cipher.Encrypt(1, "test-community-token")
	if e != nil {
		t.Fatal(e)
	}
	execSQL(t, s, "INSERT INTO poster_users(user_id,group_id,token_cipher,token_version,enabled) VALUES(1,99,$1,1,TRUE)", cipher)
	execSQL(t, s, "INSERT INTO poster_channels(chat_id,owner_id) VALUES(-100,1)")
}
func post(id int64, text string) *Message {
	return &Message{ID: id, Chat: Chat{ID: -100, Type: "channel"}, Text: text}
}
func work(t *testing.T, s *Server) {
	t.Helper()
	conn, e := s.Store.Pool.Acquire(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Release()
	if _, e = s.processJob(context.Background(), conn); e != nil {
		t.Fatal(e)
	}
}
func TestChannelOwnership(t *testing.T) {
	s, f := fixture(t, false)
	accept(t, s, 1, post(1, "unknown"), false)
	if count(t, s, "SELECT count(*) FROM poster_posts") != 0 {
		t.Fatal("unknown channel bound")
	}
	f.role = "member"
	accept(t, s, 2, commandMessage(1, "/connect_channel -100"), false)
	if count(t, s, "SELECT count(*) FROM poster_channels") != 0 {
		t.Fatal("nonadmin accepted")
	}
	f.role = "administrator"
	accept(t, s, 3, commandMessage(1, "/connect_channel -100"), false)
	accept(t, s, 4, commandMessage(2, "/connect_channel -100"), false)
	if count(t, s, "SELECT count(*) FROM poster_channels WHERE owner_id=1") != 1 {
		t.Fatal("owner overwritten")
	}
	accept(t, s, 5, commandMessage(2, "/disconnect_channel -100"), false)
	if count(t, s, "SELECT count(*) FROM poster_channels") != 1 {
		t.Fatal("other user disconnected channel")
	}
}
func TestDeduplicationAndEdits(t *testing.T) {
	s, f := fixture(t, false)
	seed(t, s)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := int64(100)
			if e := s.accept(context.Background(), Update{ID: &id, Post: post(1, "original")}); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	accept(t, s, 101, post(1, "same message"), false)
	if count(t, s, "SELECT count(*) FROM poster_posts") != 1 {
		t.Fatal("duplicate job")
	}
	work(t, s)
	accept(t, s, 103, post(1, "edited latest"), true)
	accept(t, s, 102, post(1, "stale edit"), true)
	work(t, s)
	if f.posts != 1 || f.edits != 1 || f.last.Get("post_id") != "700" || f.last.Get("message") != "edited latest" {
		t.Fatalf("post/edit counts %d/%d: %v", f.posts, f.edits, f.last)
	}
	accept(t, s, 104, post(2, "unknown edit"), true)
	if count(t, s, "SELECT count(*) FROM poster_posts") != 1 {
		t.Fatal("unknown edit created publication")
	}
}
func TestRetriesAndPermanentFailure(t *testing.T) {
	s, f := fixture(t, false)
	seed(t, s)
	accept(t, s, 1, post(1, "body"), false)
	f.vkCode = 6
	work(t, s)
	if count(t, s, "SELECT count(*) FROM poster_posts WHERE status='pending' AND attempts=1 AND last_error IS NOT NULL") != 1 {
		t.Fatal("retry not persisted")
	}
	f.vkCode = 0
	execSQL(t, s, "UPDATE poster_posts SET next_attempt_at=now()")
	work(t, s)
	if len(f.guids) != 2 || f.guids[0] != f.guids[1] {
		t.Fatal("retry changed guid")
	}
	accept(t, s, 2, post(2, "bad token"), false)
	f.vkCode = 5
	work(t, s)
	if count(t, s, "SELECT count(*) FROM poster_posts WHERE status='failed'") != 1 || count(t, s, "SELECT count(*) FROM poster_notifications") != 1 {
		t.Fatal("permanent failure lost")
	}
	conn, e := s.Store.Pool.Acquire(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Release()
	f.notifyBad = true
	if e = s.sendNotification(context.Background(), conn); e == nil {
		t.Fatal("expected notification error")
	}
	if count(t, s, "SELECT count(*) FROM poster_notifications WHERE NOT sent AND attempts=1") != 1 {
		t.Fatal("notification lost")
	}
	execSQL(t, s, "UPDATE poster_notifications SET next_attempt_at=now()")
	f.notifyBad = false
	if e = s.sendNotification(context.Background(), conn); e != nil {
		t.Fatal(e)
	}
	if count(t, s, "SELECT count(*) FROM poster_posts WHERE last_error LIKE '%SECRET%'") != 0 {
		t.Fatal("upstream error leaked")
	}
}
func TestAmbiguousResponseAndExpiredWindow(t *testing.T) {
	s, f := fixture(t, false)
	seed(t, s)
	accept(t, s, 1, post(1, "body"), false)
	f.acceptThenFail = true
	work(t, s)
	execSQL(t, s, "UPDATE poster_posts SET next_attempt_at=now()")
	work(t, s)
	if len(f.published) != 1 || f.guids[0] != f.guids[1] {
		t.Fatal("ambiguous retry not idempotent")
	}
	accept(t, s, 2, post(2, "expired"), false)
	execSQL(t, s, "UPDATE poster_posts SET status='processing',first_attempt_at=now()-interval '46 minutes' WHERE message_id=2")
	work(t, s)
	if f.posts != 2 || count(t, s, "SELECT count(*) FROM poster_posts WHERE message_id=2 AND status='failed'") != 1 {
		t.Fatal("expired create was retried")
	}
}
func TestMigrationAndTokenInput(t *testing.T) {
	s, f := fixture(t, true)
	if count(t, s, "SELECT count(*) FROM poster_users WHERE token_cipher IS NOT NULL AND NOT enabled") != 2 || count(t, s, "SELECT count(*) FROM vk_accounts WHERE access_token<>''") != 0 {
		t.Fatal("legacy encryption incomplete")
	}
	var encrypted string
	if e := s.Store.Pool.QueryRow(context.Background(), "SELECT token_cipher FROM poster_users WHERE user_id=1").Scan(&encrypted); e != nil {
		t.Fatal(e)
	}
	plain, e := s.Store.Cipher.Decrypt(1, encrypted)
	if e != nil || plain != "legacy-plaintext-token" {
		t.Fatal("legacy token not preserved")
	}
	if count(t, s, "SELECT count(*) FROM poster_channels") != 0 {
		t.Fatal("unsafe ownership imported")
	}
	accept(t, s, 1, commandMessage(1, "/set_vk 99 new-community-token"), false)
	if f.deletes != 1 {
		t.Fatal("token message not deleted")
	}
	if count(t, s, "SELECT count(*) FROM poster_users WHERE token_version=2 AND token_cipher NOT LIKE '%new-community-token%'") != 1 {
		t.Fatal("new token not encrypted")
	}
	c := s.Config
	c.Key = bytes.Repeat([]byte{8}, 32)
	bad, e := OpenStore(context.Background(), c)
	if e == nil {
		bad.Pool.Close()
		t.Fatal("wrong encryption key accepted")
	}
	if e = s.Store.migrate(context.Background()); e != nil {
		t.Fatal("migration not repeatable", e)
	}
}
func TestWebhookAuthAndReadiness(t *testing.T) {
	s, f := fixture(t, false)
	handler := s.Handler()
	request := func(path, secret, body string) int {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		if secret != "" {
			r.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if request("/telegram/webhook/development-secret", "", `{"update_id":1}`) != 404 || request("/telegram/webhook/bad", "development-secret", `{"update_id":1}`) != 404 {
		t.Fatal("webhook authentication missing")
	}
	if request("/telegram/webhook/development-secret", "development-secret", `{"update_id":1}`) != 200 {
		t.Fatal("valid webhook rejected")
	}
	if request("/telegram/webhook/development-secret", "development-secret", `{}`) != 400 {
		t.Fatal("missing update id accepted")
	}
	health := func() int {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/health", nil))
		return w.Code
	}
	if health() != 503 {
		t.Fatal("unready app healthy")
	}
	s.syncWebhook(context.Background())
	s.heartbeat()
	if health() != 200 {
		t.Fatal("ready app unhealthy")
	}
	f.webhookBad = true
	s.syncWebhook(context.Background())
	if health() != 503 {
		t.Fatal("webhook failure hidden")
	}
	s.Store.Pool.Close()
	if health() != 503 {
		t.Fatal("database failure hidden")
	}
}
func TestRevocationAndDisable(t *testing.T) {
	s, f := fixture(t, false)
	seed(t, s)
	accept(t, s, 1, post(1, "body"), false)
	f.role = "member"
	work(t, s)
	if f.posts != 0 || count(t, s, "SELECT count(*) FROM poster_posts WHERE status='failed'") != 1 {
		t.Fatal("revoked admin published")
	}
	f.role = "administrator"
	accept(t, s, 2, post(2, "body"), false)
	accept(t, s, 3, commandMessage(1, "/disable"), false)
	work(t, s)
	if f.posts != 0 || count(t, s, "SELECT count(*) FROM poster_posts WHERE status='cancelled'") != 1 {
		t.Fatal("disabled job published")
	}
}
func TestSafeAPIErrors(t *testing.T) {
	f := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "secret token", 500) }))
	defer f.Close()
	a := NewAPI(Config{BotToken: "PRIVATE"})
	a.TelegramURL = f.URL
	e := a.Telegram(context.Background(), "getMe", nil, nil)
	var api *APIError
	if !errors.As(e, &api) || !api.Temporary || strings.Contains(e.Error(), "PRIVATE") || strings.Contains(e.Error(), "secret") {
		t.Fatal("unsafe error", e)
	}
}

func TestWorkerReplicasAndRestart(t *testing.T) {
	s, f := fixture(t, false)
	seed(t, s)
	accept(t, s, 1, post(1, "body"), false)
	// A previously crashed worker left a committed lease, but no mapping yet.
	execSQL(t, s, "UPDATE poster_posts SET status='processing',attempts=1,first_attempt_at=now()-interval '1 minute',next_attempt_at=now()-interval '1 second'")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); s.RunWorker(ctx) }()
	}
	deadline := time.Now().Add(10 * time.Second)
	for count(t, s, "SELECT count(*) FROM poster_posts WHERE status='done'") != 1 {
		if time.Now().After(deadline) {
			t.Fatal("workers did not recover job")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	f.mu.Lock()
	n := f.posts
	f.mu.Unlock()
	if n != 1 {
		t.Fatalf("replicas published %d times", n)
	}
	// Check only connections actually reusable by this pool. PostgreSQL may
	// still be reaping closed backends from another isolated fixture/schema.
	checkCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	probe, e := s.Store.Pool.Acquire(checkCtx)
	if e != nil {
		t.Fatal(e)
	}
	defer probe.Release()
	idle := s.Store.Pool.AcquireAllIdle(checkCtx)
	pids := []int32{int32(probe.Conn().PgConn().PID())}
	for _, conn := range idle {
		pids = append(pids, int32(conn.Conn().PgConn().PID()))
		defer conn.Release()
	}
	var locks int
	if e = probe.QueryRow(checkCtx, "SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND objid=82015002 AND pid=ANY($1::int[])", pids).Scan(&locks); e != nil {
		t.Fatal(e)
	}
	if locks != 0 {
		t.Fatal("worker leader lock leaked into pool")
	}
}
func TestRetryExhaustion(t *testing.T) {
	s, f := fixture(t, false)
	seed(t, s)
	accept(t, s, 1, post(1, "body"), false)
	f.vkCode = 6
	for range 6 {
		execSQL(t, s, "UPDATE poster_posts SET next_attempt_at=now()")
		work(t, s)
	}
	if f.posts != 6 || count(t, s, "SELECT count(*) FROM poster_posts WHERE status='failed' AND attempts=6") != 1 || count(t, s, "SELECT count(*) FROM poster_notifications") != 1 {
		t.Fatal("retry limit or notification failed")
	}
	work(t, s)
	if f.posts != 6 {
		t.Fatal("permanent failure retried")
	}
}
func TestHealthStaleAndBodyLimits(t *testing.T) {
	s, _ := fixture(t, false)
	s.syncWebhook(context.Background())
	s.heartbeat()
	s.mu.Lock()
	s.webhookAt = time.Now().Add(-4 * time.Minute)
	s.mu.Unlock()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/health", nil))
	if w.Code != 503 {
		t.Fatal("stale webhook healthy")
	}
	r := httptest.NewRequest("POST", "/telegram/webhook/development-secret", strings.NewReader(`{"update_id":99,"junk":"`+strings.Repeat("x", 1<<20)+`"}`))
	r.Header.Set("X-Telegram-Bot-Api-Secret-Token", "development-secret")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal("oversized update accepted")
	}
	s.Store.Pool.Close()
	r = httptest.NewRequest("POST", "/telegram/webhook/development-secret", strings.NewReader(`{"update_id":100}`))
	r.Header.Set("X-Telegram-Bot-Api-Secret-Token", "development-secret")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal("database failure acknowledged")
	}
}

func TestVerifyUserTokenForGroup(t *testing.T) {
	for _, tc := range []struct {
		name, permissions, groups string
		wantError                 bool
	}{
		{"own group", `8192`, `{"groups":[{"id":99,"can_post":1,"wall":1,"is_admin":1,"admin_level":3}]}`, false},
		{"different group", `8192`, `{"groups":[{"id":100}]}`, true},
		{"no wall permission", `0`, `{"groups":[{"id":99,"can_post":1,"wall":1,"is_admin":1,"admin_level":3}]}`, true},
		{"posting denied", `8192`, `{"groups":[{"id":99,"can_post":0,"wall":1,"is_admin":1,"admin_level":3}]}`, true},
		{"wall disabled", `8192`, `{"groups":[{"id":99,"can_post":1,"wall":0,"is_admin":1,"admin_level":3}]}`, true},
		{"capability missing", `8192`, `{"groups":[{"id":99}]}`, true},
		{"group blocked", `8192`, `{"groups":[{"id":99,"can_post":1,"wall":1,"is_admin":1,"admin_level":3,"deactivated":"banned"}]}`, true},
		{"ordinary wall contributor", `8192`, `{"groups":[{"id":99,"can_post":1,"wall":1,"is_admin":0,"admin_level":0}]}`, true},
		{"moderator cannot post as group", `8192`, `{"groups":[{"id":99,"can_post":1,"wall":1,"is_admin":1,"admin_level":1}]}`, true},
		{"editor can post", `8192`, `{"groups":[{"id":99,"can_post":1,"wall":1,"is_admin":1,"admin_level":2}]}`, false},
		{"legacy response", `8192`, `[{"id":99,"can_post":1,"wall":1,"is_admin":1,"admin_level":3}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				if strings.HasSuffix(r.URL.Path, "getById") && r.Form.Get("group_ids") != "99" {
					t.Error("selected group not requested")
				}
				if strings.HasSuffix(r.URL.Path, "getById") && r.Form.Get("fields") != "can_post,wall" {
					t.Error("posting capabilities not requested")
				}
				body := tc.groups
				if strings.HasSuffix(r.URL.Path, "getAppPermissions") {
					body = tc.permissions
				}
				io.WriteString(w, `{"response":`+body+`}`)
			}))
			defer server.Close()
			a := NewAPI(Config{})
			a.VKURL = server.URL
			e := a.VerifyGroup(context.Background(), "private", 99)
			if (e != nil) != tc.wantError {
				t.Fatal("verification result", e)
			}
		})
	}
}
func TestEditArrivesDuringPublication(t *testing.T) {
	s, f := fixture(t, false)
	seed(t, s)
	accept(t, s, 1, post(1, "before"), false)
	originalURL := s.API.VKURL
	entered, resume := make(chan struct{}), make(chan struct{})
	interceptor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "wall.post") {
			close(entered)
			<-resume
		}
		f.handler(w, r)
	}))
	defer interceptor.Close()
	s.API.VKURL = interceptor.URL + "/method"
	conn, e := s.Store.Pool.Acquire(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Release()
	done := make(chan error, 1)
	go func() { _, e := s.processJob(context.Background(), conn); done <- e }()
	<-entered
	accept(t, s, 2, post(1, "after"), true)
	close(resume)
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	if count(t, s, "SELECT count(*) FROM poster_posts WHERE status='pending' AND vk_post_id=700 AND desired_version=2 AND delivered_version=1") != 1 {
		t.Fatal("edit during HTTP request lost")
	}
	s.API.VKURL = originalURL
	work(t, s)
	if f.posts != 1 || f.edits != 1 || f.last.Get("message") != "after" {
		t.Fatal("edit created new post or lost text")
	}
}

func TestMigrationFailurePreservesPlaintext(t *testing.T) {
	s, _ := fixture(t, false)
	execSQL(t, s, `DELETE FROM poster_migrations WHERE name='python_v1';
 CREATE TABLE user_settings(tg_user_id BIGINT PRIMARY KEY,selected_vk_group_id BIGINT,enabled BOOL);
 CREATE TABLE vk_accounts(tg_user_id BIGINT PRIMARY KEY,access_token TEXT NOT NULL);
 INSERT INTO user_settings VALUES(1,99,TRUE);INSERT INTO vk_accounts VALUES(1,'do-not-lose-this-token');
 CREATE FUNCTION reject_legacy_cleanup() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'simulated migration failure'; END $$;
 CREATE TRIGGER reject_cleanup BEFORE UPDATE ON vk_accounts FOR EACH ROW EXECUTE FUNCTION reject_legacy_cleanup();`)
	if e := s.Store.migrate(context.Background()); e == nil {
		t.Fatal("expected migration failure")
	}
	if count(t, s, "SELECT count(*) FROM vk_accounts WHERE access_token='do-not-lose-this-token'") != 1 || count(t, s, "SELECT count(*) FROM poster_users") != 0 {
		t.Fatal("partial migration committed")
	}
	execSQL(t, s, "DROP TRIGGER reject_cleanup ON vk_accounts")
	if e := s.Store.migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	if count(t, s, "SELECT count(*) FROM vk_accounts WHERE access_token=''") != 1 || count(t, s, "SELECT count(*) FROM poster_users WHERE token_cipher IS NOT NULL") != 1 {
		t.Fatal("migration retry failed")
	}
}

func TestHTTPBotSetupWorkflow(t *testing.T) {
	s, f := fixture(t, false)
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	id := int64(0)
	send := func(text string) {
		t.Helper()
		id++
		m := commandMessage(1, text)
		u := Update{ID: &id, Message: m}
		b, e := json.Marshal(u)
		if e != nil {
			t.Fatal(e)
		}
		req, e := http.NewRequest("POST", server.URL+"/telegram/webhook/development-secret", bytes.NewReader(b))
		if e != nil {
			t.Fatal(e)
		}
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "development-secret")
		resp, e := server.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatal("command rejected", text, resp.StatusCode)
		}
	}
	send("/start")
	send("/enable")
	if count(t, s, "SELECT count(*) FROM poster_users WHERE enabled") != 0 {
		t.Fatal("enabled without credentials")
	}
	send("/connect_channel -100")
	send("🔗 Подключить VK")
	send("https://vk.com/club99")
	send("a-private-community-token")
	if f.deletes != 1 || count(t, s, "SELECT count(*) FROM poster_users WHERE group_id=99 AND token_cipher IS NOT NULL AND connect_step=0") != 1 {
		t.Fatal("two-step token setup failed")
	}
	send("/enable")
	send("/status")
	if count(t, s, "SELECT count(*) FROM poster_users WHERE enabled") != 1 {
		t.Fatal("complete setup not enabled")
	}
	if count(t, s, "SELECT count(*) FROM poster_notifications WHERE body LIKE '%a-private-community-token%'") != 0 {
		t.Fatal("token leaked into reply")
	}
	accept(t, s, 100, post(1, "publish me"), false)
	work(t, s)
	if f.posts != 1 {
		t.Fatal("configured bot did not publish")
	}
	accept(t, s, 101, post(2, "cancel me"), false)
	send("/clear_vk")
	work(t, s)
	if f.posts != 1 || count(t, s, "SELECT count(*) FROM poster_users WHERE token_cipher IS NOT NULL OR enabled") != 0 || count(t, s, "SELECT count(*) FROM poster_posts WHERE status='cancelled'") != 1 {
		t.Fatal("clear did not remove token and cancel queued work")
	}
}

func TestTokenPostingCheckRepliesAndPreservesExistingConfig(t *testing.T) {
	for _, tc := range []struct {
		name               string
		twoStep, temporary bool
	}{
		{"command denied", false, false}, {"two-step denied", true, false}, {"temporary failure", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f := fixture(t, false)
			seed(t, s)
			accept(t, s, 100, post(1, "queued before token change"), false)
			var original string
			if e := s.Store.Pool.QueryRow(context.Background(), "SELECT token_cipher FROM poster_users WHERE user_id=1").Scan(&original); e != nil {
				t.Fatal(e)
			}
			f.verifyDenied = !tc.temporary
			f.verifyTemporary = tc.temporary
			if tc.twoStep {
				accept(t, s, 1, commandMessage(1, "/connect_vk"), false)
				accept(t, s, 2, commandMessage(1, "vk.com/club99"), false)
				accept(t, s, 3, commandMessage(1, "new-private-token"), false)
			} else {
				accept(t, s, 1, commandMessage(1, "/set_vk 99 new-private-token"), false)
			}
			var current string
			if e := s.Store.Pool.QueryRow(context.Background(), "SELECT token_cipher FROM poster_users WHERE user_id=1").Scan(&current); e != nil {
				t.Fatal(e)
			}
			if current != original || count(t, s, "SELECT count(*) FROM poster_users WHERE token_version=1 AND enabled") != 1 {
				t.Fatal("rejected token replaced existing configuration")
			}
			if count(t, s, "SELECT count(*) FROM poster_posts WHERE status='pending'") != 1 {
				t.Fatal("rejected token cancelled existing work")
			}
			var reply string
			if e := s.Store.Pool.QueryRow(context.Background(), "SELECT body FROM poster_notifications ORDER BY id DESC LIMIT 1").Scan(&reply); e != nil {
				t.Fatal(e)
			}
			if !strings.Contains(reply, "Токен не сохранён") && !strings.Contains(reply, "токен не сохранён") {
				t.Fatalf("missing rejection feedback: %s", reply)
			}
			if tc.temporary && !strings.Contains(reply, "временно") {
				t.Fatal("missing temporary failure explanation")
			}
			if !tc.temporary && !strings.Contains(reply, "не может публиковать") {
				t.Fatal("missing posting permission explanation")
			}
			if strings.Contains(reply, "new-private-token") || f.posts != 0 || f.deletes != 1 {
				t.Fatal("verification leaked token, published a probe, or failed to delete token message")
			}
		})
	}
}

func TestVKBindingUpsertAndSingleCommunityPerUser(t *testing.T) {
	s, f := fixture(t, false)
	// Exercise the INSERT path directly: no preceding ensureUser call.
	tx, e := s.Store.Pool.Begin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(context.Background())
	if e = s.saveToken(context.Background(), tx, *commandMessage(2, ""), 99, "second-user-private-token"); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(context.Background()); e != nil {
		t.Fatal(e)
	}
	if count(t, s, "SELECT count(*) FROM poster_users WHERE user_id=2 AND group_id=99 AND token_version=1 AND NOT enabled") != 1 {
		t.Fatal("UPSERT did not insert missing user")
	}
	seed(t, s)
	accept(t, s, 100, post(1, "old target"), false)
	f.groupID = 100
	accept(t, s, 1, commandMessage(1, "/set_vk 100 replacement-private-token"), false)
	if count(t, s, "SELECT count(*) FROM poster_users WHERE user_id=1") != 1 || count(t, s, "SELECT count(*) FROM poster_users WHERE user_id=1 AND group_id=100 AND token_version=2") != 1 {
		t.Fatal("replacement created a second binding or failed to update")
	}
	if count(t, s, "SELECT count(*) FROM poster_posts WHERE owner_id=1 AND group_id=99 AND status='cancelled'") != 1 {
		t.Fatal("old group's queued publication was not cancelled")
	}
	var replacementNotice string
	if e = s.Store.Pool.QueryRow(context.Background(), "SELECT body FROM poster_notifications WHERE owner_id=1 ORDER BY id DESC LIMIT 1").Scan(&replacementNotice); e != nil {
		t.Fatal(e)
	}
	for _, expected := range []string{"99 → 100", "Старая привязка отключена", "Ожидающие задания отменены: 1", "может успеть опубликоваться"} {
		if !strings.Contains(replacementNotice, expected) {
			t.Fatalf("missing replacement warning %q: %s", expected, replacementNotice)
		}
	}
	if count(t, s, "SELECT count(*) FROM poster_users WHERE user_id=2 AND group_id=99 AND token_version=1") != 1 {
		t.Fatal("another user's binding changed")
	}
	var encrypted string
	if e = s.Store.Pool.QueryRow(context.Background(), "SELECT token_cipher FROM poster_users WHERE user_id=1").Scan(&encrypted); e != nil {
		t.Fatal(e)
	}
	token, e := s.Store.Cipher.Decrypt(1, encrypted)
	if e != nil || token != "replacement-private-token" {
		t.Fatal("replacement token not saved")
	}
	// Reconnecting, including switching back, must still leave exactly one binding.
	f.groupID = 99
	accept(t, s, 2, commandMessage(1, "/set_vk 99 third-private-token"), false)
	if count(t, s, "SELECT count(*) FROM poster_users WHERE user_id=1") != 1 || count(t, s, "SELECT count(*) FROM poster_users WHERE user_id=1 AND group_id=99 AND token_version=3") != 1 {
		t.Fatal("reconnecting duplicated the binding")
	}
	if count(t, s, "SELECT count(*) FROM poster_users") != 2 {
		t.Fatal("unexpected user records")
	}
	if _, e = s.Store.Pool.Exec(context.Background(), "INSERT INTO poster_users(user_id,group_id) VALUES(1,200)"); e == nil {
		t.Fatal("database accepted a second community row for one user")
	}
	if f.posts != 0 {
		t.Fatal("token verification published a probe")
	}
}

func TestVKConnectionWarnings(t *testing.T) {
	s, _ := fixture(t, false)
	for i, command := range []string{"/start", "/format", "/connect_vk", "vk.com/club99"} {
		accept(t, s, int64(i+1), commandMessage(1, command), false)
		var reply string
		if e := s.Store.Pool.QueryRow(context.Background(), "SELECT body FROM poster_notifications WHERE owner_id=1 ORDER BY id DESC LIMIT 1").Scan(&reply); e != nil {
			t.Fatal(e)
		}
		if !strings.Contains(reply, "заменит прежнюю") || !strings.Contains(reply, "ожидающие публикации будут отменены") {
			t.Fatalf("command %s lacks advance warning: %s", command, reply)
		}
	}
	accept(t, s, 5, commandMessage(1, "first-private-token"), false)
	if count(t, s, "SELECT count(*) FROM poster_notifications WHERE body LIKE '%Сообщество VK заменено%'") != 0 {
		t.Fatal("first connection reported a replacement")
	}
	accept(t, s, 6, commandMessage(1, "/set_vk 99 another-private-token"), false)
	var reply string
	if e := s.Store.Pool.QueryRow(context.Background(), "SELECT body FROM poster_notifications WHERE owner_id=1 ORDER BY id DESC LIMIT 1").Scan(&reply); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(reply, "Токен сообщества VK 99 обновлён") || strings.Contains(reply, "Сообщество VK заменено") {
		t.Fatalf("same-group token update misleading: %s", reply)
	}
}

func TestInvalidCommunityTokenRejectedBeforeGroupLookup(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !strings.HasSuffix(r.URL.Path, "account.getAppPermissions") && !strings.HasSuffix(r.URL.Path, "groups.getTokenPermissions") {
			t.Error("group lookup performed without token authorization")
		}
		io.WriteString(w, `{"error":{"error_code":27,"error_msg":"secret upstream details"}}`)
	}))
	defer server.Close()
	api := NewAPI(Config{})
	api.VKURL = server.URL
	err := api.VerifyGroup(context.Background(), "private-community-key", 99)
	if err == nil || calls != 2 {
		t.Fatal("invalid key accepted")
	}
	var upstream *APIError
	if !errors.As(err, &upstream) || upstream.Code != 27 {
		t.Fatal("VK error classification lost")
	}
	if !strings.Contains(err.Error(), "groups.getTokenPermissions") {
		t.Fatal("missing actionable authorization diagnosis")
	}
	reply := tokenVerificationMessage(err)
	if !strings.Contains(reply, "код 27") || !strings.Contains(reply, "Токен не сохранён") {
		t.Fatal("bot did not explain authorization failure")
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private-community-key") {
		t.Fatal("sensitive VK details leaked")
	}
}

func TestVerifyCommunityTokenForGroup(t *testing.T) {
	for _, tc := range []struct {
		name, rights, group string
		denied              bool
	}{
		{"own community", `{"permissions":[{"name":"wall","setting":1}]}`, `{"groups":[{"id":99,"wall":1}]}`, false},
		{"legacy own community", `{"permissions":[{"name":"wall","setting":1}]}`, `[{"id":99}]`, false},
		{"wrong community", `{"permissions":[{"name":"wall","setting":1}]}`, `{"groups":[{"id":100,"wall":1}]}`, true},
		{"wall not allowed", `{"permissions":[{"name":"wall","setting":0}]}`, `{"groups":[{"id":99}]}`, true},
		{"disabled wall", `{"permissions":[{"name":"wall","setting":1}]}`, `{"groups":[{"id":99,"wall":0}]}`, true},
		{"blocked group", `{"permissions":[{"name":"wall","setting":1}]}`, `{"groups":[{"id":99,"deactivated":"banned"}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				switch r.URL.Path {
				case "/account.getAppPermissions":
					io.WriteString(w, `{"error":{"error_code":27}}`)
				case "/groups.getTokenPermissions":
					io.WriteString(w, `{"response":`+tc.rights+`}`)
				case "/groups.getById":
					if r.Form.Has("group_ids") {
						t.Error("community token identity must not come from arbitrary group lookup")
					}
					io.WriteString(w, `{"response":`+tc.group+`}`)
				default:
					t.Error("verification attempted to publish or used unexpected method")
					w.WriteHeader(400)
				}
			}))
			defer server.Close()
			api := NewAPI(Config{})
			api.VKURL = server.URL
			if e := api.VerifyGroup(context.Background(), "community-secret", 99); (e != nil) != tc.denied {
				t.Fatalf("unexpected verification result: %v", e)
			}
		})
	}
}
