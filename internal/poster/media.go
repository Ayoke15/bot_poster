package poster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"strings"
)

const maxPhotoBytes = 20 << 20

type PhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size"`
}

func largestPhoto(photos []PhotoSize) string {
	var chosen string
	var area int64
	for _, p := range photos {
		if p.FileID == "" || p.Width <= 0 || p.Height <= 0 {
			continue
		}
		size := int64(p.Width) * int64(p.Height)
		if chosen == "" || size > area {
			chosen = p.FileID
			area = size
		}
	}
	return chosen
}

// Download only the file path supplied by Telegram; never log credential URLs.
func (a *API) downloadPhoto(ctx context.Context, fileID string) ([]byte, string, error) {
	var file struct {
		Path string `json:"file_path"`
		Size int64  `json:"file_size"`
	}
	if e := a.Telegram(ctx, "getFile", map[string]any{"file_id": fileID}, &file); e != nil {
		return nil, "", fmt.Errorf("Telegram getFile: %w", e)
	}
	if file.Path == ".." || strings.HasPrefix(file.Path, "../") || file.Path == "" || strings.HasPrefix(file.Path, "/") || path.Clean(file.Path) != file.Path || strings.ContainsAny(file.Path, "?#\\") || file.Size > maxPhotoBytes {
		return nil, "", &VerificationError{"Telegram photo path is invalid or photo exceeds 20 MiB"}
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, a.TelegramURL+"/file/bot"+a.BotToken+"/"+file.Path, nil)
	if e != nil {
		return nil, "", &APIError{"telegram_file", 0, false}
	}
	client := *a.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, e := client.Do(req)
	if e != nil {
		return nil, "", &APIError{"telegram_file", 0, true}
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, "", &APIError{"telegram_file", response.StatusCode, response.StatusCode == 429 || response.StatusCode >= 500}
	}
	data, e := io.ReadAll(io.LimitReader(response.Body, maxPhotoBytes+1))
	if e != nil {
		return nil, "", &APIError{"telegram_file", 0, true}
	}
	if len(data) == 0 || len(data) > maxPhotoBytes {
		return nil, "", &VerificationError{"Telegram photo is empty or exceeds 20 MiB"}
	}
	switch http.DetectContentType(data) {
	case "image/jpeg":
		return data, "photo.jpg", nil
	case "image/png":
		return data, "photo.png", nil
	}
	return nil, "", &VerificationError{"Unsupported Telegram photo format; JPEG or PNG required"}
}

func multipartPhoto(photo []byte, filename string, fields map[string]string) (*bytes.Buffer, string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for key, value := range fields {
		if e := w.WriteField(key, value); e != nil {
			return nil, "", e
		}
	}
	headers := make(textproto.MIMEHeader)
	headers.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "photo", "filename": filename}))
	headers.Set("Content-Type", http.DetectContentType(photo))
	part, e := w.CreatePart(headers)
	if e != nil {
		return nil, "", e
	}
	if _, e = part.Write(photo); e != nil {
		return nil, "", e
	}
	if e = w.Close(); e != nil {
		return nil, "", e
	}
	return &body, w.FormDataContentType(), nil
}

func (a *API) trustedUploadURL(endpoint string) bool {
	u, e := url.Parse(endpoint)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	base, e := url.Parse(a.VKURL)
	if e == nil && u.Host == base.Host {
		return true
	}
	host := strings.ToLower(u.Hostname())
	for _, suffix := range []string{"vk.com", "vk.ru", "vkuserphoto.ru", "vkuserphoto.com", "userapi.com"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func (a *API) UploadWallPhoto(ctx context.Context, token string, group int64, fileID string) (string, error) {
	data, filename, e := a.downloadPhoto(ctx, fileID)
	if e != nil {
		return "", e
	}
	var server struct {
		URL string `json:"upload_url"`
	}
	saveMethod := "photos.saveWallPhoto"
	if e = a.VK(ctx, "photos.getWallUploadServer", token, url.Values{"group_id": {intString(group)}}, &server); e != nil {
		apiError, ok := e.(*APIError)
		if !ok || apiError.Code != 27 {
			return "", fmt.Errorf("VK photos.getWallUploadServer (check photos permission): %w", e)
		}
		// VK community keys cannot use wall upload endpoints. The supported
		// community photo upload creates an attachment without sending a message.
		if e = a.VK(ctx, "photos.getMessagesUploadServer", token, url.Values{}, &server); e != nil {
			return "", fmt.Errorf("VK photos.getMessagesUploadServer (check photos permission): %w", e)
		}
		saveMethod = "photos.saveMessagesPhoto"
	}
	if !a.trustedUploadURL(server.URL) {
		return "", &VerificationError{"VK returned an untrusted photo upload URL"}
	}
	body, contentType, e := multipartPhoto(data, filename, nil)
	if e != nil {
		return "", &APIError{"vk_photo_upload", 0, false}
	}
	// Upload links contain signatures: disallow redirects and return only safe errors.
	uploadAPI := *a
	client := *a.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	uploadAPI.HTTP = &client
	raw, e := uploadAPI.request(ctx, "vk_photo_upload", server.URL, body, contentType)
	if e != nil {
		endpoint, _ := url.Parse(server.URL)
		return "", fmt.Errorf("VK multipart photo upload host=%s, file_type=%s: %w", endpoint.Hostname(), http.DetectContentType(data), e)
	}
	var uploaded struct {
		Photo  string `json:"photo"`
		Server int64  `json:"server"`
		Hash   string `json:"hash"`
	}
	if json.Unmarshal(raw, &uploaded) != nil || uploaded.Photo == "" || uploaded.Photo == "[]" || uploaded.Server <= 0 || uploaded.Hash == "" {
		return "", &APIError{"vk_photo_upload", 0, true}
	}
	var saved []struct {
		ID        int64  `json:"id"`
		Owner     int64  `json:"owner_id"`
		AccessKey string `json:"access_key"`
	}
	saveParams := url.Values{"photo": {uploaded.Photo}, "server": {intString(uploaded.Server)}, "hash": {uploaded.Hash}}
	if saveMethod == "photos.saveWallPhoto" {
		saveParams.Set("group_id", intString(group))
	}
	if e = a.VK(ctx, saveMethod, token, saveParams, &saved); e != nil {
		return "", fmt.Errorf("VK %s: %w", saveMethod, e)
	}
	if len(saved) != 1 || saved[0].ID <= 0 || saved[0].Owner == 0 {
		return "", &APIError{"vk_photo_upload", 0, true}
	}
	attachment := "photo" + intString(saved[0].Owner) + "_" + intString(saved[0].ID)
	if saved[0].AccessKey != "" {
		attachment += "_" + saved[0].AccessKey
	}
	return attachment, nil
}

// SendPhoto is used by the live creation test with a generated PNG fixture.
func (a *API) SendPhoto(ctx context.Context, chat int64, caption string, photo []byte, result any) error {
	body, contentType, e := multipartPhoto(photo, "test.png", map[string]string{"chat_id": intString(chat), "caption": caption})
	if e != nil {
		return &APIError{"telegram", 0, false}
	}
	raw, e := a.request(ctx, "telegram", a.TelegramURL+"/bot"+a.BotToken+"/sendPhoto", body, contentType)
	if e != nil {
		return e
	}
	return parseTelegramResponse(raw, result)
}
