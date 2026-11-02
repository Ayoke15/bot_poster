package poster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type APIError struct {
	Service   string
	Code      int
	Temporary bool
}

func (e *APIError) Error() string { return fmt.Sprintf("%s API failure (code %d)", e.Service, e.Code) }

type API struct {
	HTTP                                    *http.Client
	TelegramURL, VKURL, BotToken, VKVersion string
}

func NewAPI(c Config) *API {
	return &API{HTTP: &http.Client{Timeout: 15 * time.Second}, TelegramURL: "https://api.telegram.org", VKURL: "https://api.vk.com/method", BotToken: c.BotToken, VKVersion: c.VKVersion}
}

// Never return transport URLs or upstream descriptions: they can contain credentials.
func (a *API) request(ctx context.Context, service, endpoint string, body io.Reader, contentType string) ([]byte, error) {
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if e != nil {
		return nil, &APIError{service, 0, false}
	}
	req.Header.Set("Content-Type", contentType)
	resp, e := a.HTTP.Do(req)
	if e != nil {
		return nil, &APIError{service, 0, true}
	}
	defer resp.Body.Close()
	data, e := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if e != nil {
		return nil, &APIError{service, 0, true}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{service, resp.StatusCode, resp.StatusCode == 429 || resp.StatusCode >= 500}
	}
	return data, nil
}
func (a *API) Telegram(ctx context.Context, method string, payload any, result any) error {
	b, e := json.Marshal(payload)
	if e != nil {
		return e
	}
	data, e := a.request(ctx, "telegram", a.TelegramURL+"/bot"+a.BotToken+"/"+method, bytes.NewReader(b), "application/json")
	if e != nil {
		return e
	}
	var r struct {
		OK     bool            `json:"ok"`
		Code   int             `json:"error_code"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(data, &r) != nil {
		return &APIError{"telegram", 0, true}
	}
	if !r.OK {
		return &APIError{"telegram", r.Code, r.Code == 429 || r.Code >= 500}
	}
	if result != nil {
		if e = json.Unmarshal(r.Result, result); e != nil {
			return &APIError{"telegram", 0, true}
		}
	}
	return nil
}
func (a *API) VK(ctx context.Context, method, token string, values url.Values, result any) error {
	values.Set("access_token", token)
	values.Set("v", a.VKVersion)
	data, e := a.request(ctx, "vk", a.VKURL+"/"+method, bytes.NewBufferString(values.Encode()), "application/x-www-form-urlencoded")
	if e != nil {
		return e
	}
	var r struct {
		Response json.RawMessage `json:"response"`
		Error    *struct {
			Code int `json:"error_code"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &r) != nil {
		return &APIError{"vk", 0, true}
	}
	if r.Error != nil {
		code := r.Error.Code
		return &APIError{"vk", code, code == 1 || code == 6 || code == 9 || code == 10 || code == 29}
	}
	if len(r.Response) == 0 || string(r.Response) == "null" {
		return &APIError{"vk", 0, true}
	}
	if result != nil {
		if e = json.Unmarshal(r.Response, result); e != nil {
			return &APIError{"vk", 0, true}
		}
	}
	return nil
}

// VerificationError contains only a fixed, safe explanation for the bot user.
type VerificationError struct{ Message string }

func (e *VerificationError) Error() string { return e.Message }

func (a *API) VerifyGroup(ctx context.Context, token string, group int64) error {
	// Only a community token may pass getTokenPermissions. Require wall access.
	var rights struct {
		Permissions []struct {
			Name    string `json:"name"`
			Setting int    `json:"setting"`
		} `json:"permissions"`
	}
	if e := a.VK(ctx, "groups.getTokenPermissions", token, url.Values{}, &rights); e != nil {
		return e
	}
	wall := false
	for _, p := range rights.Permissions {
		if p.Name == "wall" && p.Setting > 0 {
			wall = true
		}
	}
	if !wall {
		return &VerificationError{"У ключа нет права wall. Создайте ключ сообщества с правом публикации на стене."}
	}
	var raw json.RawMessage
	// With a community key and no group_ids, VK returns the token's own group.
	// Supplying the requested group ID here would only check public visibility.
	if e := a.VK(ctx, "groups.getById", token, url.Values{"fields": {"can_post,wall"}}, &raw); e != nil {
		return e
	}
	type groupInfo struct {
		ID          int64  `json:"id"`
		CanPost     *int   `json:"can_post"`
		Wall        *int   `json:"wall"`
		Deactivated string `json:"deactivated"`
	}
	var groups []groupInfo
	if json.Unmarshal(raw, &groups) != nil {
		var object struct {
			Groups []groupInfo `json:"groups"`
		}
		if json.Unmarshal(raw, &object) != nil {
			return &APIError{"vk", 0, false}
		}
		groups = object.Groups
	}
	if len(groups) != 1 || groups[0].ID != group {
		return &VerificationError{"Ключ выдан другому сообществу. Пришлите ключ именно выбранной группы."}
	}
	g := groups[0]
	if g.Deactivated != "" {
		return &VerificationError{"Сообщество VK заблокировано или удалено. Публикация недоступна."}
	}
	if g.Wall != nil && *g.Wall == 0 {
		return &VerificationError{"Стена сообщества отключена. Включите её в настройках VK и пришлите токен снова."}
	}
	if g.CanPost != nil && *g.CanPost == 0 {
		return &VerificationError{"VK сообщает, что этот токен не может публиковать на стене сообщества. Проверьте права ключа и настройки стены."}
	}
	if g.CanPost == nil || *g.CanPost != 1 || g.Wall == nil || *g.Wall < 1 || *g.Wall > 3 {
		return &VerificationError{"VK не подтвердил возможность публикации: не получены корректные can_post и wall. Проверьте настройки сообщества и повторите подключение."}
	}
	return nil
}
func (a *API) Member(ctx context.Context, chat, user int64) (string, error) {
	var r struct {
		Status string `json:"status"`
	}
	e := a.Telegram(ctx, "getChatMember", map[string]any{"chat_id": chat, "user_id": user}, &r)
	return r.Status, e
}
