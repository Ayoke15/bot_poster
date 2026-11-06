package poster

// Protocol reference: VKBrowserClient, commit ae3494a4131ca6bb6f742a26fdcfa04471b6646b.
// This is an independent Go implementation of the VK website protocol.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

const WebCredentialPrefix = "vk-web-session-v1:"

type WebCookie struct {
	Name, Value, Domain, Path string
	Expires                   float64
	Secure, HTTPOnly          bool
}
type WebSession struct {
	UserID    int64
	GroupID   int64
	Cookies   []WebCookie
	UserAgent string
	Token     string
	Expires   int64
}
type SessionExpired struct{}

func (*SessionExpired) Error() string {
	return "Сессия VK требует повторного входа через vk-session login. Очередь приостановлена."
}

// A WebClient belongs to one job/account. It is never shared across owners.
type WebClient struct {
	Session          WebSession
	http             *http.Client
	loginURL, apiURL string
}

func NewWebAPI(base *API, credential string) (*API, error) {
	if !strings.HasPrefix(credential, WebCredentialPrefix) {
		return nil, errors.New("not a web session")
	}
	var session WebSession
	if json.Unmarshal([]byte(strings.TrimPrefix(credential, WebCredentialPrefix)), &session) != nil || len(session.Cookies) == 0 || len(session.UserAgent) > 512 {
		return nil, errors.New("invalid web session")
	}
	jar, _ := cookiejar.New(nil)
	for _, c := range session.Cookies {
		domain := strings.TrimPrefix(c.Domain, ".")
		if domain != "vk.ru" && !strings.HasSuffix(domain, ".vk.ru") {
			return nil, errors.New("invalid session cookie domain")
		}
		u := &url.URL{Scheme: "https", Host: domain, Path: "/"}
		cookie := &http.Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, Secure: true, HttpOnly: c.HTTPOnly}
		if c.Expires > 0 {
			cookie.Expires = time.Unix(int64(c.Expires), 0)
		}
		jar.SetCookies(u, []*http.Cookie{cookie})
	}
	client := *base.HTTP
	client.Jar = jar
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	w := &WebClient{Session: session, http: &client, loginURL: "https://login.vk.ru/?act=web_token", apiURL: "https://web.api.vk.ru/method/"}
	a := *base
	a.Web = w
	return &a, nil
}
func (w *WebClient) Credential() (string, error) {
	b, e := json.Marshal(w.Session)
	return WebCredentialPrefix + string(b), e
}
func (w *WebClient) request(ctx context.Context, endpoint string, values url.Values) ([]byte, error) {
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if e != nil {
		return nil, &APIError{"vk_web", 0, false}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://vk.ru")
	req.Header.Set("Referer", "https://vk.ru/")
	req.Header.Set("User-Agent", w.Session.UserAgent)
	resp, e := w.http.Do(req)
	if e != nil {
		return nil, &APIError{"vk_web", 0, true}
	}
	defer resp.Body.Close()
	// Persist rotated cookies; reject unrelated domains, even in upstream replies.
	for _, c := range resp.Cookies() {
		d := strings.TrimPrefix(c.Domain, ".")
		if d == "" {
			d = req.URL.Hostname()
		}
		if d != "vk.ru" && !strings.HasSuffix(d, ".vk.ru") {
			continue
		}
		if req.URL.Hostname() != d && !strings.HasSuffix(req.URL.Hostname(), "."+d) {
			continue
		}
		path := c.Path
		if path == "" {
			path = "/"
		}
		kept := w.Session.Cookies[:0]
		for _, old := range w.Session.Cookies {
			if !(old.Name == c.Name && strings.TrimPrefix(old.Domain, ".") == d && old.Path == path) {
				kept = append(kept, old)
			}
		}
		w.Session.Cookies = kept
		if c.MaxAge < 0 {
			continue
		}
		expires := float64(0)
		if !c.Expires.IsZero() {
			expires = float64(c.Expires.Unix())
		}
		if c.MaxAge > 0 {
			expires = float64(time.Now().Unix() + int64(c.MaxAge))
		}
		w.Session.Cookies = append(w.Session.Cookies, WebCookie{c.Name, c.Value, d, path, expires, true, c.HttpOnly})
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 || resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, &SessionExpired{}
	}
	if resp.StatusCode != 200 {
		return nil, &APIError{"vk_web", resp.StatusCode, resp.StatusCode == 429 || resp.StatusCode >= 500}
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if e != nil || len(b) > 2<<20 {
		return nil, &APIError{"vk_web", 0, true}
	}
	return b, nil
}
func (w *WebClient) refresh(ctx context.Context) error {
	b, e := w.request(ctx, w.loginURL, url.Values{"version": {"1"}, "app_id": {"6287487"}})
	if e != nil {
		return e
	}
	var response struct {
		Type string
		Data struct {
			Token   string `json:"access_token"`
			Expires int64
			UserID  int64 `json:"user_id"`
		}
	}
	if json.Unmarshal(b, &response) != nil || response.Type != "okay" || response.Data.Token == "" || response.Data.UserID <= 0 || response.Data.Expires <= time.Now().Unix() {
		return &SessionExpired{}
	}
	if w.Session.UserID != 0 && w.Session.UserID != response.Data.UserID {
		return &SessionExpired{}
	}
	w.Session.UserID = response.Data.UserID
	w.Session.Token = response.Data.Token
	w.Session.Expires = response.Data.Expires
	return nil
}
func (w *WebClient) call(ctx context.Context, method string, values url.Values, result any) error {
	for attempt := 0; attempt < 2; attempt++ {
		if w.Session.Token == "" || w.Session.Expires <= time.Now().Add(time.Minute).Unix() {
			if e := w.refresh(ctx); e != nil {
				return e
			}
		}
		values.Set("access_token", w.Session.Token)
		values.Set("v", "5.282")
		b, e := w.request(ctx, w.apiURL+method+"?v=5.282&client_id=6287487", values)
		if e != nil {
			return e
		}
		var r struct {
			Response json.RawMessage
			Error    *struct {
				Code int `json:"error_code"`
			}
		}
		if json.Unmarshal(b, &r) != nil {
			return &APIError{"vk_web", 0, true}
		}
		if r.Error != nil {
			code := r.Error.Code
			if code == 5 {
				w.Session.Token = ""
				continue
			}
			if code == 14 || code == 17 {
				return &SessionExpired{}
			}
			return &APIError{"vk_web", code, code == 1 || code == 6 || code == 9 || code == 10 || code == 29}
		}
		if len(r.Response) == 0 || bytes.Equal(r.Response, []byte("null")) {
			return &APIError{"vk_web", 0, true}
		}
		if result != nil && json.Unmarshal(r.Response, result) != nil {
			return &APIError{"vk_web", 0, true}
		}
		return nil
	}
	return &SessionExpired{}
}

func (a *API) VerifyWebGroup(ctx context.Context, group int64) error {
	if a.Web == nil || group <= 0 {
		return errors.New("invalid web group")
	}
	// User role and enabled wall must be proved, not inferred from public lookup.
	if e := a.verifyGroupIdentity(ctx, "", group, false); e != nil {
		return e
	}
	var result struct {
		URL string `json:"upload_url"`
	}
	if e := a.VK(ctx, "photos.getWallUploadServer", "", url.Values{"group_id": {intString(group)}}, &result); e != nil {
		return e
	}
	if !a.trustedUploadURL(result.URL) {
		return errors.New("untrusted photo server")
	}
	return nil
}

func (a *API) VerifyPublishedPhoto(ctx context.Context, token string, group, postID int64, attachment string) error {
	var raw json.RawMessage
	if e := a.VK(ctx, "wall.getById", token, url.Values{"posts": {intString(-group) + "_" + intString(postID)}}, &raw); e != nil {
		return e
	}
	type post struct {
		ID          int64
		Owner       int64 `json:"owner_id"`
		Attachments []struct {
			Type  string
			Photo struct {
				ID    int64
				Owner int64 `json:"owner_id"`
			}
		}
	}
	var posts []post
	if json.Unmarshal(raw, &posts) != nil {
		var r struct{ Items []post }
		if json.Unmarshal(raw, &r) != nil {
			return &APIError{"vk_read", 0, true}
		}
		posts = r.Items
	}
	for _, p := range posts {
		if p.ID != postID || p.Owner != -group {
			continue
		}
		for _, at := range p.Attachments {
			ref := "photo" + intString(at.Photo.Owner) + "_" + intString(at.Photo.ID)
			if at.Type == "photo" && (attachment == ref || strings.HasPrefix(attachment, ref+"_")) {
				return nil
			}
		}
	}
	return &VerificationError{"VK создал запись без ожидаемой фотографии. Запись сохранена для проверки; доставка не подтверждена."}
}
