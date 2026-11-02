//go:build live

package poster

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// This opt-in test creates and deletes real posts. It never registers or deletes
// webhooks; only dedicated test bots without an active webhook may be used.
func TestLiveTelegramToVK(t *testing.T) {
	if os.Getenv("RUN_LIVE_INTEGRATION") != "true" {
		t.Skip("live integration is opt-in")
	}
	required := func(name string) string {
		t.Helper()
		v := os.Getenv(name)
		if v == "" {
			t.Fatalf("missing live test setting: %s", name)
		}
		return v
	}
	number := func(name string) int64 {
		t.Helper()
		v, e := strconv.ParseInt(required(name), 10, 64)
		if e != nil || v == 0 {
			t.Fatalf("invalid numeric setting: %s", name)
		}
		return v
	}
	receiverToken := required("TG_TEST_BOT_TOKEN")
	senderToken := required("TG_TEST_SENDER_BOT_TOKEN")
	vkToken := required("VK_TEST_COMMUNITY_TOKEN")
	readToken := required("VK_TEST_READ_TOKEN")
	channel, owner, group := number("TG_TEST_CHANNEL_ID"), number("TG_TEST_OWNER_ID"), number("VK_TEST_GROUP_ID")
	if channel >= 0 || owner <= 0 || group <= 0 {
		t.Fatal("channel must be negative; owner and VK group must be positive")
	}
	if receiverToken == senderToken {
		t.Fatal("publisher and receiver must be different dedicated test bots")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	key := make([]byte, 32)
	if _, e := rand.Read(key); e != nil {
		t.Fatal("cannot generate test encryption key")
	}
	suffix := make([]byte, 12)
	if _, e := rand.Read(suffix); e != nil {
		t.Fatal("cannot generate test marker")
	}
	marker := "autoposter-ci-" + hex.EncodeToString(suffix)
	c := Config{DatabaseURL: required("TEST_DATABASE_URL"), BotToken: receiverToken, WebhookSecret: "live-integration-secret", VKVersion: "5.199", Key: key}
	// One disposable schema per run; application data is never read or migrated.
	admin, e := pgxpool.New(ctx, c.DatabaseURL)
	if e != nil {
		t.Fatal("cannot configure test database")
	}
	t.Cleanup(admin.Close)
	schemaName := "poster_live_" + hex.EncodeToString(suffix)
	if _, e = admin.Exec(ctx, "CREATE SCHEMA "+schemaName); e != nil {
		t.Fatal("cannot create isolated test schema")
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if _, e := admin.Exec(cleanup, "DROP SCHEMA "+schemaName+" CASCADE"); e != nil {
			t.Error("test schema cleanup failed")
		}
	})
	u, e := url.Parse(c.DatabaseURL)
	if e != nil {
		t.Fatal("invalid test database URL")
	}
	q := u.Query()
	q.Set("search_path", schemaName)
	u.RawQuery = q.Encode()
	c.DatabaseURL = u.String()
	store, e := OpenStore(ctx, c)
	if e != nil {
		t.Fatal("test database initialization failed")
	}
	t.Cleanup(store.Pool.Close)
	api := NewAPI(c)
	sender := NewAPI(Config{BotToken: senderToken})
	var me struct {
		ID int64 `json:"id"`
	}
	if e = api.Telegram(ctx, "getMe", map[string]any{}, &me); e != nil {
		t.Fatal(e)
	}
	s := NewServer(c, store, api, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.BotID = me.ID
	var publisher struct {
		ID int64 `json:"id"`
	}
	if e = sender.Telegram(ctx, "getMe", map[string]any{}, &publisher); e != nil {
		t.Fatal(e)
	}
	if publisher.ID == me.ID {
		t.Fatal("test bots must be different")
	}
	var hook struct {
		URL string `json:"url"`
	}
	if e = api.Telegram(ctx, "getWebhookInfo", map[string]any{}, &hook); e != nil {
		t.Fatal(e)
	}
	if hook.URL != "" {
		t.Fatal("receiver has a webhook; use a dedicated polling test bot, not the deployed bot")
	}
	if e = s.checkAdmin(ctx, channel, owner); e != nil {
		t.Fatal("owner and receiver bot must be channel administrators")
	}
	role, e := sender.Member(ctx, channel, publisher.ID)
	if e != nil || role != "administrator" {
		t.Fatal("publisher bot must be a channel administrator with post/delete permissions")
	}
	if e = api.VerifyGroup(ctx, vkToken, group); e != nil {
		t.Fatal(e)
	}
	// Confirm the read credential before creating any content.
	var before json.RawMessage
	if e = api.VK(ctx, "wall.get", readToken, url.Values{"owner_id": {intString(-group)}, "count": {"1"}}, &before); e != nil {
		t.Fatal("VK_TEST_READ_TOKEN cannot read the test community wall")
	}
	encrypted, e := store.Cipher.Encrypt(owner, vkToken)
	if e != nil {
		t.Fatal("cannot encrypt test credential")
	}
	if _, e = store.Pool.Exec(ctx, "INSERT INTO poster_users(user_id,group_id,token_cipher,token_version,enabled) VALUES($1,$2,$3,1,TRUE)", owner, group, encrypted); e != nil {
		t.Fatal("cannot seed test owner")
	}
	if _, e = store.Pool.Exec(ctx, "INSERT INTO poster_channels(chat_id,owner_id) VALUES($1,$2)", channel, owner); e != nil {
		t.Fatal("cannot seed verified test channel")
	}
	// Acknowledge old updates on this dedicated bot; never forward backlog to VK.
	var backlog []Update
	if e = api.Telegram(ctx, "getUpdates", map[string]any{"offset": -1, "limit": 1, "timeout": 0, "allowed_updates": []string{"channel_post"}}, &backlog); e != nil {
		t.Fatal(e)
	}
	offset := int64(0)
	for _, u := range backlog {
		if u.ID != nil {
			offset = *u.ID + 1
		}
	}
	var sent Message
	if e = sender.Telegram(ctx, "sendMessage", map[string]any{"chat_id": channel, "text": marker}, &sent); e != nil {
		t.Fatal(e)
	}
	if sent.ID <= 0 {
		t.Fatal("Telegram returned no message ID")
	}
	// Cleanup runs before the database pools close, even when an assertion fails.
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		var postID *int64
		if e := store.Pool.QueryRow(cleanup, "SELECT vk_post_id FROM poster_posts WHERE chat_id=$1 AND message_id=$2", channel, sent.ID).Scan(&postID); e == nil && postID != nil {
			if e = api.VK(cleanup, "wall.delete", vkToken, url.Values{"owner_id": {intString(-group)}, "post_id": {intString(*postID)}}, nil); e != nil {
				t.Error("VK test post cleanup failed; remove the post bearing marker " + marker)
			}
		}
		if e := sender.Telegram(cleanup, "deleteMessage", map[string]any{"chat_id": channel, "message_id": sent.ID}, nil); e != nil {
			t.Error("Telegram test post cleanup failed")
		}
	})
	httpServer := httptest.NewServer(s.Handler())
	t.Cleanup(httpServer.Close)
	workerCtx, stopWorker := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.RunWorker(workerCtx) }()
	t.Cleanup(func() { stopWorker(); wg.Wait() })
	var received *Update
	for received == nil && ctx.Err() == nil {
		var updates []Update
		if e = api.Telegram(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 5, "limit": 100, "allowed_updates": []string{"channel_post"}}, &updates); e != nil {
			t.Fatal(e)
		}
		for _, update := range updates {
			if update.ID != nil && *update.ID >= offset {
				offset = *update.ID + 1
			}
			if update.Post != nil && update.Post.Chat.ID == channel && update.Post.ID == sent.ID {
				copy := update
				received = &copy
			}
		}
	}
	if received == nil {
		t.Fatal("receiver did not receive the real Telegram channel_post; check bot permissions and delivery")
	}
	payload, e := json.Marshal(received)
	if e != nil {
		t.Fatal("cannot encode Telegram update")
	}
	// Replay the real Telegram update through the production HTTP handler twice.
	for range 2 {
		req, e := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL+"/telegram/webhook/"+c.WebhookSecret, bytes.NewReader(payload))
		if e != nil {
			t.Fatal("cannot construct webhook request")
		}
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", c.WebhookSecret)
		response, e := httpServer.Client().Do(req)
		if e != nil {
			t.Fatal("local webhook request failed")
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal("webhook did not acknowledge real update")
		}
	}
	var postID *int64
	for ctx.Err() == nil {
		var state string
		e = store.Pool.QueryRow(ctx, "SELECT status,vk_post_id FROM poster_posts WHERE chat_id=$1 AND message_id=$2", channel, sent.ID).Scan(&state, &postID)
		if e != nil {
			t.Fatal("cannot read delivery status")
		}
		if state == "failed" || state == "cancelled" {
			t.Fatal("live VK delivery failed; inspect test community permissions and limits")
		}
		if state == "done" && postID != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	if postID == nil {
		t.Fatal("timed out waiting for VK publication")
	}
	type vkPost struct {
		ID        int64           `json:"id"`
		Owner     int64           `json:"owner_id"`
		IsDeleted json.RawMessage `json:"is_deleted"`
	}
	var raw json.RawMessage
	if e = api.VK(ctx, "wall.getById", readToken, url.Values{"posts": {intString(-group) + "_" + intString(*postID)}}, &raw); e != nil {
		t.Fatal(e)
	}
	var posts []vkPost
	if json.Unmarshal(raw, &posts) != nil {
		var object struct {
			Items []vkPost `json:"items"`
		}
		if json.Unmarshal(raw, &object) != nil {
			t.Fatal("invalid VK read response")
		}
		posts = object.Items
	}
	if len(posts) != 1 || posts[0].ID != *postID || posts[0].Owner != -group || string(posts[0].IsDeleted) == "true" || string(posts[0].IsDeleted) == "1" {
		t.Fatal("created VK post does not exist in the target community")
	}
	t.Log("Real Telegram update delivered to VK; post existence and target verified; duplicate webhook acknowledged")
}
