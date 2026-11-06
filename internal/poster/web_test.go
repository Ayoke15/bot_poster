package poster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func webResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}
}
func testWebCredential(t *testing.T, user int64) string {
	t.Helper()
	b, e := json.Marshal(WebSession{UserID: user, GroupID: 99, UserAgent: "test-browser", Cookies: []WebCookie{{Name: "remixsid", Value: "PRIVATE_COOKIE", Domain: ".vk.ru", Path: "/", Secure: true, HTTPOnly: true}}})
	if e != nil {
		t.Fatal(e)
	}
	return WebCredentialPrefix + string(b)
}
func TestWebRefreshAndIsolation(t *testing.T) {
	refreshes, calls := 0, 0
	api := NewAPI(Config{})
	api.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		_ = r.ParseForm()
		if r.Header.Get("User-Agent") != "test-browser" || r.Header.Get("Origin") != "https://vk.ru" {
			t.Error("web headers missing")
		}
		if r.URL.Host == "login.vk.ru" {
			refreshes++
			cookie, e := r.Cookie("remixsid")
			if e != nil || cookie.Value != "PRIVATE_COOKIE" {
				t.Error("session cookie missing")
			}
			if r.PostForm.Get("app_id") != "6287487" || r.PostForm.Get("version") != "1" {
				t.Error("incorrect token request")
			}
			return webResponse(r, 200, fmt.Sprintf(`{"type":"okay","data":{"access_token":"PRIVATE_TOKEN_%d","expires":%d,"user_id":42}}`, refreshes, time.Now().Add(time.Hour).Unix())), nil
		}
		calls++
		if r.URL.Query().Get("client_id") != "6287487" || r.PostForm.Get("v") != "5.282" {
			t.Error("incorrect web protocol parameters")
		}
		if calls == 1 {
			return webResponse(r, 200, `{"error":{"error_code":5,"error_msg":"PRIVATE_UPSTREAM"}}`), nil
		}
		if r.PostForm.Get("access_token") != "PRIVATE_TOKEN_2" {
			t.Error("refreshed token not used")
		}
		return webResponse(r, 200, `{"response":1}`), nil
	})
	web, e := NewWebAPI(api, testWebCredential(t, 42))
	if e != nil {
		t.Fatal(e)
	}
	var result int
	if e = web.VK(context.Background(), "wall.edit", "", url.Values{}, &result); e != nil {
		t.Fatal(e)
	}
	if result != 1 || refreshes != 2 || calls != 2 || api.Web != nil {
		t.Fatal("token refresh did not stay scoped to one client")
	}
	// A token for another VK user must never silently change identity.
	other, e := NewWebAPI(api, testWebCredential(t, 77))
	if e != nil {
		t.Fatal(e)
	}
	e = other.VK(context.Background(), "wall.post", "", url.Values{}, nil)
	var expired *SessionExpired
	if !errors.As(e, &expired) || calls != 2 {
		t.Fatal("mismatched identity was not rejected before publication")
	}
}

func TestWebPhotoReadbackAndPause(t *testing.T) {
	for _, scenario := range []string{"photo-present", "photo-missing", "read-retry", "session-expired"} {
		t.Run(scenario, func(t *testing.T) {
			s, f := fixture(t, false)
			seed(t, s)
			media := photoServer(t, s, f)
			credential := testWebCredential(t, 42)
			encrypted, e := s.Store.Cipher.Encrypt(1, credential)
			if e != nil {
				t.Fatal(e)
			}
			execSQL(t, s, "UPDATE poster_users SET token_cipher=$1 WHERE user_id=1", encrypted)
			transport := s.API.HTTP.Transport
			webTarget, _ := url.Parse(s.API.VKURL)
			reads := 0
			s.API.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "login.vk.ru" {
					if scenario == "session-expired" {
						return webResponse(r, 200, `{"type":"error","message":"PRIVATE_COOKIE"}`), nil
					}
					return webResponse(r, 200, fmt.Sprintf(`{"type":"okay","data":{"access_token":"PRIVATE_WEB_TOKEN","user_id":42,"expires":%d}}`, time.Now().Add(time.Hour).Unix())), nil
				}
				if r.URL.Host == "web.api.vk.ru" {
					if strings.HasSuffix(r.URL.Path, "wall.getById") {
						reads++
						if scenario == "read-retry" && reads == 1 {
							return webResponse(r, 503, "PRIVATE_UPSTREAM"), nil
						}
						if scenario == "photo-missing" {
							return webResponse(r, 200, `{"response":[{"id":700,"owner_id":-99,"attachments":[]}]}`), nil
						}
						return webResponse(r, 200, `{"response":{"items":[{"id":700,"owner_id":-99,"attachments":[{"type":"photo","photo":{"id":55,"owner_id":-99}}]}]}}`), nil
					}
					copy := r.Clone(r.Context())
					u := *r.URL
					u.Scheme = webTarget.Scheme
					u.Host = webTarget.Host
					copy.URL = &u
					return transport.RoundTrip(copy)
				}
				if r.Header.Get("Cookie") != "" {
					t.Error("web cookies leaked to Telegram or media upload")
				}
				return transport.RoundTrip(r)
			})
			accept(t, s, 1, photoMessage(1, "Caption", "large-file"), false)
			work(t, s)
			switch scenario {
			case "photo-present":
				if count(t, s, "SELECT count(*) FROM poster_posts WHERE status='done' AND vk_post_id=700") != 1 {
					t.Fatal("photo not verified")
				}
			case "photo-missing":
				if count(t, s, "SELECT count(*) FROM poster_posts WHERE status='failed' AND vk_post_id=700") != 1 {
					t.Fatal("caption-only publication reported success")
				}
			case "read-retry":
				if count(t, s, "SELECT count(*) FROM poster_posts WHERE status='pending' AND vk_post_id=700") != 1 {
					t.Fatal("post ID lost before readback retry")
				}
				execSQL(t, s, "UPDATE poster_posts SET next_attempt_at=now()")
				work(t, s)
				if f.posts != 1 || f.edits != 1 || count(t, s, "SELECT count(*) FROM poster_posts WHERE status='done'") != 1 {
					t.Fatal("read retry duplicated post or lost delivery")
				}
			case "session-expired":
				if f.posts != 0 || count(t, s, "SELECT count(*) FROM poster_users WHERE web_paused") != 1 || count(t, s, "SELECT count(*) FROM poster_posts WHERE status='pending' AND attempts=0") != 1 {
					t.Fatal("expired session did not preserve queue")
				}
				work(t, s)
				if count(t, s, "SELECT count(*) FROM poster_notifications WHERE body LIKE '%повторного входа%'") != 1 {
					t.Fatal("pause notice not deduplicated")
				}
			}
			_, uploads, _ := media.counts()
			if scenario != "session-expired" && uploads != 1 {
				t.Fatal("attachment upload not reused")
			}
		})
	}
}

func TestWebSessionEncryptionAndImport(t *testing.T) {
	s, f := fixture(t, false)
	seed(t, s)
	photoServer(t, s, f)
	target, _ := url.Parse(s.API.VKURL)
	transport := s.API.HTTP.Transport
	s.API.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "login.vk.ru" {
			return webResponse(r, 200, fmt.Sprintf(`{"type":"okay","data":{"access_token":"PRIVATE_TOKEN","user_id":42,"expires":%d}}`, time.Now().Add(time.Hour).Unix())), nil
		}
		if r.URL.Host == "web.api.vk.ru" {
			copy := r.Clone(r.Context())
			u := *r.URL
			u.Host = target.Host
			u.Scheme = target.Scheme
			copy.URL = &u
			return transport.RoundTrip(copy)
		}
		return transport.RoundTrip(r)
	})
	encrypted, e := s.Store.Cipher.Encrypt(1, testWebCredential(t, 42))
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(encrypted, "PRIVATE_COOKIE") {
		t.Fatal("plaintext session stored")
	}
	if e = s.Store.ImportWebSession(context.Background(), s.API, 2, 99, encrypted); e == nil {
		t.Fatal("cross-owner session accepted")
	}
	if e = s.Store.ImportWebSession(context.Background(), s.API, 1, 100, encrypted); e == nil {
		t.Fatal("cross-group session accepted")
	}
	// Import must not replace credentials while a worker can publish with them.
	conn, e := s.Store.Pool.Acquire(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = conn.Exec(context.Background(), "SELECT pg_advisory_lock(82015002)"); e != nil {
		t.Fatal(e)
	}
	e = s.Store.ImportWebSession(context.Background(), s.API, 1, 99, encrypted)
	_, unlockErr := conn.Exec(context.Background(), "SELECT pg_advisory_unlock(82015002)")
	conn.Release()
	if e == nil || unlockErr != nil {
		t.Fatal("active worker did not block import")
	}
	accept(t, s, 1, photoMessage(1, "Preserved caption", "large-file"), false)
	execSQL(t, s, "UPDATE poster_posts SET vk_post_id=700,vk_attachment='old-attachment',uploaded_photo_file_id='large-file'")
	execSQL(t, s, "UPDATE poster_users SET web_paused=TRUE WHERE user_id=1")
	for i := 0; i < 2; i++ {
		if e = s.Store.ImportWebSession(context.Background(), s.API, 1, 99, encrypted); e != nil {
			t.Fatal(e)
		}
	}
	if count(t, s, "SELECT count(*) FROM poster_posts WHERE status='pending' AND vk_post_id=700 AND vk_attachment='' AND body='Preserved caption' AND token_version=1") != 1 {
		t.Fatal("session import lost queue identity or retained old account attachment")
	}
	if count(t, s, "SELECT count(*) FROM poster_users WHERE user_id=1 AND group_id=99 AND NOT web_paused") != 1 {
		t.Fatal("session import did not upsert/unpause")
	}
}

func TestWebRejectsCookieExfiltrationAndRedirect(t *testing.T) {
	api := NewAPI(Config{})
	credential := strings.ReplaceAll(testWebCredential(t, 42), ".vk.ru", ".evil.example")
	if _, e := NewWebAPI(api, credential); e == nil {
		t.Fatal("unrelated cookies accepted")
	}
	calls := 0
	api.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "login.vk.ru" {
			t.Fatal("web session followed redirect")
		}
		response := webResponse(r, 302, "PRIVATE_RESPONSE")
		response.Header.Set("Location", "https://evil.example/PRIVATE_TOKEN")
		return response, nil
	})
	web, e := NewWebAPI(api, testWebCredential(t, 42))
	if e != nil {
		t.Fatal(e)
	}
	e = web.VK(context.Background(), "wall.post", "", url.Values{}, nil)
	var expired *SessionExpired
	if !errors.As(e, &expired) || calls != 1 || strings.Contains(e.Error(), "PRIVATE") {
		t.Fatal("redirect was followed or secret appeared in error")
	}
}
