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
		failure := &APIError{service, resp.StatusCode, resp.StatusCode == 429 || resp.StatusCode >= 500}
		if service == "vk_photo_upload" {
			allowed := resp.Header.Get("Allow")
			for _, ch := range allowed {
				if !(ch >= 'A' && ch <= 'Z') && ch != ',' && ch != ' ' {
					allowed = "unavailable"
					break
				}
			}
			return nil, fmt.Errorf("HTTP POST rejected (allowed methods: %s): %w", allowed, failure)
		}
		return nil, failure
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
	return parseTelegramResponse(data, result)
}
func parseTelegramResponse(data []byte, result any) error {
	var e error
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
	// account.getAppPermissions accepts user tokens. A public group lookup alone
	// does not establish either the token type or the right to post as the group.
	var permissions int64
	if e := a.VK(ctx, "account.getAppPermissions", token, url.Values{}, &permissions); e != nil {
		if apiError, ok := e.(*APIError); ok && apiError.Code == 27 {
			return a.verifyCommunityGroup(ctx, token, group)
		}
		return e
	}
	if permissions&8192 == 0 {
		return &VerificationError{"У пользовательского ключа нет права wall. Авторизуйте приложение с доступом к стене."}
	}
	return a.verifyGroupIdentity(ctx, token, group, false)
}

func (a *API) verifyCommunityGroup(ctx context.Context, token string, group int64) error {
	var rights struct {
		Permissions []struct {
			Name    string `json:"name"`
			Setting int    `json:"setting"`
		} `json:"permissions"`
	}
	if e := a.VK(ctx, "groups.getTokenPermissions", token, url.Values{}, &rights); e != nil {
		return fmt.Errorf("groups.getTokenPermissions: %w", e)
	}
	wall := false
	for _, p := range rights.Permissions {
		if p.Name == "wall" && p.Setting > 0 {
			wall = true
		}
	}
	if !wall {
		return &VerificationError{"У ключа сообщества нет права wall. Проверьте разрешения ключа."}
	}
	return a.verifyGroupIdentity(ctx, token, group, true)
}

func (a *API) verifyGroupIdentity(ctx context.Context, token string, group int64, community bool) error {
	var raw json.RawMessage
	// A community token must identify its own group; user tokens select a group
	// and must additionally prove an editor/administrator role.
	params := url.Values{"fields": {"can_post,wall"}}
	if !community {
		params.Set("group_ids", intString(group))
	}
	if e := a.VK(ctx, "groups.getById", token, params, &raw); e != nil {
		return e
	}
	type groupInfo struct {
		ID          int64  `json:"id"`
		CanPost     *int   `json:"can_post"`
		Wall        *int   `json:"wall"`
		Deactivated string `json:"deactivated"`
		IsAdmin     *int   `json:"is_admin"`
		AdminLevel  *int   `json:"admin_level"`
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
		return &VerificationError{"VK не вернул выбранное сообщество. Проверьте ID группы и доступ аккаунта."}
	}
	g := groups[0]
	if g.Deactivated != "" {
		return &VerificationError{"Сообщество VK заблокировано или удалено. Публикация недоступна."}
	}
	if !community && (g.IsAdmin == nil || *g.IsAdmin != 1 || g.AdminLevel == nil || *g.AdminLevel < 2) {
		return &VerificationError{"Владелец пользовательского ключа должен быть редактором или администратором выбранного сообщества, чтобы публиковать от его имени."}
	}
	if g.Wall != nil && *g.Wall == 0 {
		return &VerificationError{"Стена сообщества отключена. Включите её в настройках VK и пришлите токен снова."}
	}
	if !community && g.CanPost != nil && *g.CanPost == 0 {
		return &VerificationError{"VK сообщает, что этот токен не может публиковать на стене сообщества. Проверьте права ключа и настройки стены."}
	}
	if !community && (g.CanPost == nil || *g.CanPost != 1 || g.Wall == nil || *g.Wall < 1 || *g.Wall > 3) {
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
