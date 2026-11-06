package poster

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func testPhotoPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 256, 256))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{R: 240, G: 160, B: 70, A: 255}}, image.Point{}, draw.Src)
	var b bytes.Buffer
	if e := png.Encode(&b, img); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}

type photoFixture struct {
	wallServerError, communityServers, communitySaves int
	mu                                                sync.Mutex
	downloads, uploads, saves                         int
	fileID                                            string
	path                                              string
	data                                              []byte
	size                                              int64
	uploadURL                                         string
	uploadStatus, saveError                           int
}

func (p *photoFixture) counts() (int, int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.downloads, p.uploads, p.saves
}
func photoServer(t *testing.T, s *Server, f *fakeAPI) *photoFixture {
	t.Helper()
	p := &photoFixture{path: "photos/test.png", data: testPhotoPNG(t)}
	var base string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		switch r.URL.Path {
		case "/bot10:test-token/getFile":
			var payload struct {
				ID string `json:"file_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			p.fileID = payload.ID
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_path": p.path, "file_size": p.size}})
		case "/file/bot10:test-token/photos/test.png":
			p.downloads++
			_, _ = w.Write(p.data)
		case "/method/photos.getWallUploadServer":
			if p.wallServerError != 0 {
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"error_code": p.wallServerError}})
				return
			}
			_ = r.ParseForm()
			if r.Form.Get("group_id") != "99" {
				t.Error("wrong upload group")
			}
			endpoint := p.uploadURL
			if endpoint == "" {
				endpoint = base + "/upload?signature=private-upload-signature"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"response": map[string]string{"upload_url": endpoint}})
		case "/method/photos.getMessagesUploadServer":
			_ = r.ParseForm()
			if r.Form.Has("peer_id") {
				t.Error("community upload must not address the group itself as a message peer")
			}
			p.communityServers++
			_ = json.NewEncoder(w).Encode(map[string]any{"response": map[string]string{"upload_url": base + "/upload"}})
		case "/upload":
			if r.ProtoMajor != 1 {
				t.Error("VK multipart upload must use HTTP/1.1 even when the API supports HTTP/2")
			}
			p.uploads++
			if p.uploadStatus != 0 {
				http.Error(w, "private upstream details", p.uploadStatus)
				return
			}
			if e := r.ParseMultipartForm(maxPhotoBytes + 1024); e != nil {
				t.Error(e)
				w.WriteHeader(400)
				return
			}
			defer r.MultipartForm.RemoveAll()
			photo, fileHeader, e := r.FormFile("file0")
			if e != nil {
				t.Error(e)
				return
			}
			defer photo.Close()
			if fileHeader.Header.Get("Content-Type") != "image/png" {
				t.Error("image MIME type missing from multipart part")
			}
			data, _ := io.ReadAll(photo)
			if !bytes.Equal(data, p.data) {
				t.Error("uploaded bytes differ from Telegram photo")
			}
			io.WriteString(w, `{"photo":"[{\"photo\":\"server-data\"}]","server":10,"hash":"private-save-hash"}`)
		case "/method/photos.saveWallPhoto", "/method/photos.saveMessagesPhoto":
			p.saves++
			_ = r.ParseForm()
			if r.URL.Path == "/method/photos.saveMessagesPhoto" {
				p.communitySaves++
				if r.Form.Has("group_id") {
					t.Error("group_id sent to saveMessagesPhoto")
				}
			} else if r.Form.Get("group_id") != "99" {
				t.Error("wrong save group")
			}
			if r.Form.Get("server") != "10" || r.Form.Get("hash") != "private-save-hash" || r.Form.Get("photo") == "" {
				t.Error("wrong save parameters")
			}
			if p.saveError != 0 {
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"error_code": p.saveError, "error_msg": "private upstream details"}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"response": []map[string]any{{"id": 54 + p.saves, "owner_id": -99}}})
		case "/bot10:test-token/sendPhoto":
			if e := r.ParseMultipartForm(maxPhotoBytes + 1024); e != nil {
				t.Error(e)
				return
			}
			defer r.MultipartForm.RemoveAll()
			imageFile, _, e := r.FormFile("photo")
			if e != nil {
				t.Error(e)
				return
			}
			defer imageFile.Close()
			data, _ := io.ReadAll(imageFile)
			if !bytes.Equal(data, p.data) {
				t.Error("wrong generated photo")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 1, "chat": map[string]any{"id": -100, "type": "channel"}, "caption": r.FormValue("caption"), "photo": []map[string]any{{"file_id": "largest", "width": 256, "height": 256}}}})
		default:
			f.handler(w, r)
		}
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	base = server.URL
	t.Cleanup(server.Close)
	s.API.HTTP = server.Client()
	s.API.TelegramURL = base
	s.API.VKURL = base + "/method"
	return p
}
func photoMessage(id int64, caption, fileID string) *Message {
	m := post(id, "")
	m.Caption = caption
	m.Photo = []PhotoSize{{FileID: fileID, Width: 1024, Height: 768}, {FileID: "thumbnail", Width: 90, Height: 90}}
	return m
}
func TestPhotoCaptionDeliveryAndEdits(t *testing.T) {
	s, f := fixture(t, false)
	seed(t, s)
	media := photoServer(t, s, f)
	m := photoMessage(1, "Caption", "large-file")
	accept(t, s, 1, m, false)
	accept(t, s, 1, m, false)
	work(t, s)
	if f.posts != 1 || f.last.Get("attachments") != "photo-99_55" || f.last.Get("message") != "Caption" {
		t.Fatal("photo and caption were not published together")
	}
	media.mu.Lock()
	selected := media.fileID
	media.mu.Unlock()
	if selected != "large-file" {
		t.Fatal("largest Telegram size not selected")
	}
	accept(t, s, 2, photoMessage(1, "Changed caption", "large-file"), true)
	work(t, s)
	_, uploads, saves := media.counts()
	if uploads != 1 || saves != 1 || f.edits != 1 || f.last.Get("attachments") != "photo-99_55" || f.last.Get("message") != "Changed caption" {
		t.Fatal("caption edit reuploaded or lost photo")
	}
	accept(t, s, 3, photoMessage(1, "New image", "replacement-file"), true)
	work(t, s)
	_, uploads, saves = media.counts()
	if uploads != 2 || saves != 2 || f.edits != 2 || f.last.Get("attachments") != "photo-99_56" {
		t.Fatal("photo replacement reused stale attachment")
	}
	if count(t, s, "SELECT count(*) FROM poster_posts WHERE vk_post_id=700 AND uploaded_photo_file_id='replacement-file' AND status='done'") != 1 {
		t.Fatal("photo mapping not persisted")
	}
}
func TestPhotoWithoutCaptionAndCachedRetry(t *testing.T) {
	s, f := fixture(t, false)
	seed(t, s)
	media := photoServer(t, s, f)
	accept(t, s, 1, photoMessage(1, "", "large-file"), false)
	f.vkCode = 6
	work(t, s)
	if count(t, s, "SELECT count(*) FROM poster_posts WHERE status='pending' AND vk_attachment='photo-99_55'") != 1 {
		t.Fatal("photo cache lost on wall retry")
	}
	f.vkCode = 0
	execSQL(t, s, "UPDATE poster_posts SET next_attempt_at=now()")
	work(t, s)
	_, uploads, saves := media.counts()
	if uploads != 1 || saves != 1 || f.posts != 2 || f.guids[0] != f.guids[1] || f.last.Get("message") != "" {
		t.Fatal("photo-only post duplicated upload or lost stable guid")
	}
}
func TestPhotoUploadFailures(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		name := "temporary"
		if permanent {
			name = "permanent"
		}
		t.Run(name, func(t *testing.T) {
			s, f := fixture(t, false)
			seed(t, s)
			media := photoServer(t, s, f)
			if permanent {
				media.saveError = 7
			} else {
				media.uploadStatus = 503
			}
			accept(t, s, 1, photoMessage(1, "Caption", "large-file"), false)
			work(t, s)
			if f.posts != 0 {
				t.Fatal("caption was published without the failed photo")
			}
			if permanent {
				if count(t, s, "SELECT count(*) FROM poster_posts WHERE status='failed'") != 1 || count(t, s, "SELECT count(*) FROM poster_notifications") != 1 {
					t.Fatal("permanent photo failure lost")
				}
			} else {
				if count(t, s, "SELECT count(*) FROM poster_posts WHERE status='pending'") != 1 {
					t.Fatal("temporary upload failure not retried")
				}
				media.mu.Lock()
				media.uploadStatus = 0
				media.mu.Unlock()
				execSQL(t, s, "UPDATE poster_posts SET next_attempt_at=now()")
				work(t, s)
				if f.posts != 1 {
					t.Fatal("photo retry did not recover")
				}
			}
			if count(t, s, "SELECT count(*) FROM poster_posts WHERE last_error LIKE '%private-%' OR last_error LIKE '%signature=%'") != 0 {
				t.Fatal("photo credentials leaked in errors")
			}
		})
	}
}
func TestPhotoInputValidation(t *testing.T) {
	for _, tc := range []string{"oversized", "invalid-path", "invalid-format", "untrusted-upload"} {
		t.Run(tc, func(t *testing.T) {
			s, f := fixture(t, false)
			p := photoServer(t, s, f)
			switch tc {
			case "oversized":
				p.size = maxPhotoBytes + 1
			case "invalid-path":
				p.path = "../secret"
			case "invalid-format":
				p.data = []byte("not an image")
			case "untrusted-upload":
				p.uploadURL = "https://example.invalid/upload?secret=private"
			}
			_, e := s.API.UploadWallPhoto(context.Background(), "secret-token", 99, "file")
			if e == nil {
				t.Fatal("invalid input accepted")
			}
			if strings.Contains(e.Error(), "secret-token") || strings.Contains(e.Error(), "?secret=") {
				t.Fatal("unsafe error")
			}
			_, uploads, _ := p.counts()
			if uploads != 0 {
				t.Fatal("invalid photo was uploaded")
			}
		})
	}
}
func TestPhotoSchemaUpgrade(t *testing.T) {
	s, _ := fixture(t, false)
	seed(t, s)
	accept(t, s, 1, post(1, "Existing text"), false)
	execSQL(t, s, "ALTER TABLE poster_posts DROP COLUMN photo_file_id,DROP COLUMN vk_attachment,DROP COLUMN uploaded_photo_file_id")
	if e := s.Store.migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	if count(t, s, "SELECT count(*) FROM poster_posts WHERE body='Existing text' AND photo_file_id='' AND vk_attachment='' AND uploaded_photo_file_id=''") != 1 {
		t.Fatal("schema upgrade damaged text post")
	}
}
func TestSendPhotoMultipart(t *testing.T) {
	s, f := fixture(t, false)
	p := photoServer(t, s, f)
	var message Message
	if e := s.API.SendPhoto(context.Background(), -100, "Test caption", p.data, &message); e != nil {
		t.Fatal(e)
	}
	if message.Caption != "Test caption" || largestPhoto(message.Photo) != "largest" || message.Chat.ID != -100 {
		t.Fatal("sendPhoto response not decoded")
	}
}
func TestUploadURLValidation(t *testing.T) {
	api := NewAPI(Config{})
	for _, endpoint := range []string{"http://pu.vk.com/upload", "https://vk.com.evil.example/upload", "https://user:secret@pu.vk.com/upload", "https://127.0.0.1/upload"} {
		if api.trustedUploadURL(endpoint) {
			t.Fatal("untrusted upload URL accepted")
		}
	}
	for _, endpoint := range []string{"https://pu.vk.com/upload?sig=private", "https://pu.vkuserphoto.ru/upload"} {
		if !api.trustedUploadURL(endpoint) {
			t.Fatal("VK upload URL rejected")
		}
	}
}

func TestCommunityPhotoUploadFallback(t *testing.T) {
	s, f := fixture(t, false)
	seed(t, s)
	media := photoServer(t, s, f)
	media.wallServerError = 27
	accept(t, s, 1, photoMessage(1, "Community caption", "large-file"), false)
	work(t, s)
	media.mu.Lock()
	servers, saves := media.communityServers, media.communitySaves
	media.mu.Unlock()
	if servers != 1 || saves != 1 || f.posts != 1 || f.last.Get("attachments") != "photo-99_55" || f.last.Get("message") != "Community caption" {
		t.Fatal("community photo was not attached to wall post")
	}
}
func TestPhotoScopeDenialDoesNotSelectDifferentUpload(t *testing.T) {
	s, f := fixture(t, false)
	seed(t, s)
	media := photoServer(t, s, f)
	media.wallServerError = 7
	accept(t, s, 1, photoMessage(1, "Caption", "large-file"), false)
	work(t, s)
	media.mu.Lock()
	servers := media.communityServers
	media.mu.Unlock()
	if servers != 0 || f.posts != 0 || count(t, s, "SELECT count(*) FROM poster_posts WHERE status='failed'") != 1 {
		t.Fatal("scope denial was not preserved")
	}
}
