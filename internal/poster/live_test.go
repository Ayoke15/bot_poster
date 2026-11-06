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

// This opt-in test creates real posts and leaves them for manual inspection. It never registers or deletes
// webhooks or polls updates. A synthetic channel_post wraps the real sendMessage
// response; inbound Telegram event delivery is deliberately outside this test.
func TestLiveTelegramToVK(t *testing.T)      { runLivePost(t, false) }
func TestLiveTelegramPhotoToVK(t *testing.T) { runLivePost(t, true) }
func runLivePost(t *testing.T, withPhoto bool) {
	t.Helper()
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
	botToken := required("TG_TEST_BOT_TOKEN")
	vkToken := os.Getenv("VK_TEST_USER_TOKEN")
	if vkToken == "" {
		vkToken = required("VK_TEST_COMMUNITY_TOKEN")
	} // Both verified community and user tokens are supported.
	channel, owner, group := number("TG_TEST_CHANNEL_ID"), number("TG_TEST_OWNER_ID"), number("VK_TEST_GROUP_ID")
	if channel >= 0 || owner <= 0 || group <= 0 {
		t.Fatal("channel must be negative; owner and VK group must be positive")
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
	c := Config{DatabaseURL: required("TEST_DATABASE_URL"), BotToken: botToken, WebhookSecret: "live-integration-secret", VKVersion: "5.199", Key: key}
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
	var me struct {
		ID int64 `json:"id"`
	}
	if e = api.Telegram(ctx, "getMe", map[string]any{}, &me); e != nil {
		t.Fatal(e)
	}
	s := NewServer(c, store, api, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.BotID = me.ID
	var hook struct {
		URL string `json:"url"`
	}
	if e = api.Telegram(ctx, "getWebhookInfo", map[string]any{}, &hook); e != nil {
		t.Fatal(e)
	}
	if hook.URL != "" {
		t.Fatal("test bot has a webhook; use a dedicated bot to avoid triggering a deployed application")
	}
	if e = s.checkAdmin(ctx, channel, owner); e != nil {
		t.Fatal("owner and test bot must be channel administrators")
	}
	if e = api.VerifyGroup(ctx, vkToken, group); e != nil {
		t.Fatalf("VK token verification failed: %v. Check the configured VK_TEST_USER_TOKEN or VK_TEST_COMMUNITY_TOKEN and its wall permission. No test posts have been created.", e)
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
	var sent Message
	if withPhoto {
		e = api.SendPhoto(ctx, channel, marker, testPhotoPNG(t), &sent)
	} else {
		e = api.Telegram(ctx, "sendMessage", map[string]any{"chat_id": channel, "text": marker}, &sent)
	}
	if e != nil {
		t.Fatal(e)
	}
	if withPhoto && (largestPhoto(sent.Photo) == "" || sent.Caption != marker) {
		t.Fatal("Telegram did not return a photo and caption")
	}
	if sent.ID <= 0 || sent.Chat.ID != channel || sent.Chat.Type != "channel" {
		t.Fatal("Telegram did not return a message from the test channel")
	}
	httpServer := httptest.NewServer(s.Handler())
	t.Cleanup(httpServer.Close)
	workerCtx, stopWorker := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.RunWorker(workerCtx) }()
	t.Cleanup(func() { stopWorker(); wg.Wait() })
	// sendMessage returns the real channel message. Bots do not receive their
	// own outgoing messages as incoming updates. Construct only the envelope;
	// its update_id is local to this disposable schema, not a Telegram update ID.
	updateID := sent.ID
	received := Update{ID: &updateID, Post: &sent}
	payload, e := json.Marshal(received)
	if e != nil {
		t.Fatal("cannot encode Telegram update")
	}
	// Submit the real message in a synthetic update twice to exercise deduplication.
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
			t.Fatal("handler did not acknowledge the synthetic update")
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
	if withPhoto && count(t, s, "SELECT count(*) FROM poster_posts WHERE chat_id=$1 AND message_id=$2 AND photo_file_id<>'' AND uploaded_photo_file_id=photo_file_id AND vk_attachment<>''", channel, sent.ID) != 1 {
		t.Fatal("photo attachment was not saved before VK publication")
	}
	if *postID <= 0 {
		t.Fatal("VK did not return a positive post ID")
	}
	t.Logf("Created Telegram message %d and VK post %d_%d; both left for manual inspection. VK URL: https://vk.ru/wall%d_%d", sent.ID, -group, *postID, -group, *postID)
	t.Log("Inbound Telegram delivery and VK content/read/delete APIs are outside this creation-only test")
}
