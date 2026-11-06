package poster

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/url"
	"strings"
	"time"
)

type job struct {
	Chat, Message, Owner, Group, TokenVersion, Version int64
	Body, Guid, Token                                  string
	Photo, Attachment, UploadedPhoto                   string
	PostID                                             *int64
	Attempts                                           int
	First                                              time.Time
}

func (s *Server) RunWorker(ctx context.Context) {
	for ctx.Err() == nil {
		conn, e := s.Store.Pool.Acquire(ctx)
		if e == nil {
			var leader bool
			e = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(82015002)").Scan(&leader)
			if e == nil && leader {
				s.workAsLeader(ctx, conn)
				cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				_, unlockErr := conn.Exec(cleanup, "SELECT pg_advisory_unlock(82015002)")
				if unlockErr != nil {
					raw := conn.Hijack()
					_ = raw.Close(cleanup)
					conn = nil
				}
				cancel()
			} else if e == nil {
				s.heartbeat()
			}
			if conn != nil {
				conn.Release()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
func (s *Server) heartbeat() { s.mu.Lock(); s.workerAt = time.Now(); s.mu.Unlock() }
func (s *Server) workAsLeader(ctx context.Context, conn *pgxpool.Conn) {
	for ctx.Err() == nil {
		if e := conn.Ping(ctx); e != nil {
			return
		}
		s.heartbeat()
		worked, e := s.processJob(ctx, conn)
		if e != nil {
			s.Log.Error("queue database operation failed")
			return
		}
		if e = s.sendNotification(ctx, conn); e != nil {
			s.Log.Warn("notification delivery deferred")
		}
		if !worked {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}
func (s *Server) processJob(ctx context.Context, conn *pgxpool.Conn) (bool, error) {
	// The committed lease and stable guid survive a process crash.
	var j job
	e := conn.QueryRow(ctx, `UPDATE poster_posts SET status='processing',attempts=attempts+1,
 first_attempt_at=coalesce(first_attempt_at,now()),next_attempt_at=now()+interval '60 seconds'
 WHERE (chat_id,message_id)=(SELECT chat_id,message_id FROM poster_posts WHERE status IN ('pending','processing') AND next_attempt_at<=now() AND NOT EXISTS(SELECT 1 FROM poster_users u WHERE u.user_id=poster_posts.owner_id AND u.web_paused) ORDER BY next_attempt_at,chat_id,message_id LIMIT 1)
 RETURNING chat_id,message_id,owner_id,group_id,token_version,body,desired_version,guid,vk_post_id,attempts,first_attempt_at,photo_file_id,vk_attachment,uploaded_photo_file_id`).Scan(&j.Chat, &j.Message, &j.Owner, &j.Group, &j.TokenVersion, &j.Body, &j.Version, &j.Guid, &j.PostID, &j.Attempts, &j.First, &j.Photo, &j.Attachment, &j.UploadedPhoto)
	if errors.Is(e, pgx.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	var valid bool
	e = conn.QueryRow(ctx, `SELECT coalesce(u.token_cipher,''),coalesce(u.enabled AND u.group_id=$3 AND u.token_version=$4 AND EXISTS(SELECT 1 FROM poster_channels c WHERE c.chat_id=$1 AND c.owner_id=u.user_id),FALSE) FROM poster_users u WHERE u.user_id=$2`, j.Chat, j.Owner, j.Group, j.TokenVersion).Scan(&j.Token, &valid)
	if e != nil || !valid {
		if e != nil && !errors.Is(e, pgx.ErrNoRows) {
			return true, e
		}
		_, e = conn.Exec(ctx, "UPDATE poster_posts SET status='cancelled',last_error='configuration no longer active' WHERE chat_id=$1 AND message_id=$2 AND status='processing'", j.Chat, j.Message)
		return true, e
	}
	if j.Attempts > 6 || (j.PostID == nil && time.Since(j.First) > 45*time.Minute) {
		return true, s.failJob(ctx, conn, j, errors.New("retry window expired; verify VK manually before reposting"), false)
	}
	if j.Body == "" && j.Photo == "" {
		return true, s.failJob(ctx, conn, j, errors.New("empty edited text; VK post left unchanged"), false)
	}
	token, e := s.Store.Cipher.Decrypt(j.Owner, j.Token)
	if e != nil {
		return true, s.failJob(ctx, conn, j, errors.New("stored token cannot be decrypted"), false)
	}
	api := s.API
	if strings.HasPrefix(token, WebCredentialPrefix) {
		api, e = NewWebAPI(s.API, token)
		if e != nil {
			return true, s.failJob(ctx, conn, j, errors.New("stored web session is invalid"), false)
		}
		if api.Web.Session.GroupID != j.Group {
			return true, s.failJob(ctx, conn, j, errors.New("web session community mismatch"), false)
		}
		token = ""
		defer func() {
			credential, err := api.Web.Credential()
			if err != nil {
				return
			}
			cipher, err := s.Store.Cipher.Encrypt(j.Owner, credential)
			if err != nil {
				return
			}
			cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, err = conn.Exec(cleanup, "UPDATE poster_users SET token_cipher=$3 WHERE user_id=$1 AND token_cipher=$2", j.Owner, j.Token, cipher); err != nil {
				s.Log.Warn("web session refresh persistence failed")
			}
		}()
	}
	if e = s.checkAdmin(ctx, j.Chat, j.Owner); e != nil {
		return true, s.failJob(ctx, conn, j, errors.New("channel administrator verification failed"), temporary(e))
	}
	attachment := ""
	if j.Photo != "" {
		if j.UploadedPhoto == j.Photo && j.Attachment != "" {
			attachment = j.Attachment
		} else {
			attachment, e = api.UploadWallPhoto(ctx, token, j.Group, j.Photo)
			if e != nil {
				return true, s.failJob(ctx, conn, j, e, temporary(e))
			}
			// Cache the attachment before publication. Caption edits and retries reuse it.
			if _, e = conn.Exec(ctx, "UPDATE poster_posts SET vk_attachment=$3,uploaded_photo_file_id=$4 WHERE chat_id=$1 AND message_id=$2", j.Chat, j.Message, attachment, j.Photo); e != nil {
				return true, e
			}
		}
	}
	values := url.Values{"owner_id": {intString(-j.Group)}, "message": {j.Body}, "attachments": {attachment}}
	var postID int64
	if j.PostID == nil {
		values.Set("from_group", "1")
		values.Set("guid", j.Guid)
		var result struct {
			PostID int64 `json:"post_id"`
		}
		e = api.VK(ctx, "wall.post", token, values, &result)
		postID = result.PostID
		if e == nil && postID <= 0 {
			e = &APIError{"vk", 0, true}
		}
	} else {
		postID = *j.PostID
		values.Set("post_id", intString(postID))
		var result int
		e = api.VK(ctx, "wall.edit", token, values, &result)
		if e == nil && result != 1 {
			e = &APIError{"vk", 0, true}
		}
	}
	if e != nil {
		return true, s.failJob(ctx, conn, j, e, temporary(e))
	}
	if api.Web != nil && j.Photo != "" {
		// Persist the accepted post BEFORE readback: failures must never create a
		// second wall.post, even when VK silently discarded the attachment.
		if _, e = conn.Exec(ctx, "UPDATE poster_posts SET vk_post_id=$3 WHERE chat_id=$1 AND message_id=$2", j.Chat, j.Message, postID); e != nil {
			return true, e
		}
		if e = api.VerifyPublishedPhoto(ctx, token, j.Group, postID, attachment); e != nil {
			return true, s.failJob(ctx, conn, j, e, temporary(e))
		}
	}
	_, e = conn.Exec(ctx, `UPDATE poster_posts SET vk_post_id=$3,delivered_version=$4,status=CASE WHEN status='cancelled' THEN status WHEN desired_version>$4 THEN 'pending' ELSE 'done' END,attempts=0,last_error=NULL,next_attempt_at=now() WHERE chat_id=$1 AND message_id=$2`, j.Chat, j.Message, postID, j.Version)
	if e == nil {
		s.Log.Info("VK delivery completed", "chat_id", j.Chat, "message_id", j.Message, "vk_post_id", postID)
	}
	return true, e
}
func (s *Server) failJob(ctx context.Context, conn *pgxpool.Conn, j job, cause error, retry bool) error {
	var expired *SessionExpired
	if errors.As(cause, &expired) {
		tx, e := conn.Begin(ctx)
		if e != nil {
			return e
		}
		defer tx.Rollback(ctx)
		tag, e := tx.Exec(ctx, "UPDATE poster_users SET web_paused=TRUE WHERE user_id=$1 AND token_version=$2 AND group_id=$3 AND NOT web_paused", j.Owner, j.TokenVersion, j.Group)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "UPDATE poster_posts SET status='pending',attempts=greatest(attempts-1,0),first_attempt_at=CASE WHEN attempts=1 THEN NULL ELSE first_attempt_at END,last_error=$3 WHERE chat_id=$1 AND message_id=$2 AND status='processing'", j.Chat, j.Message, expired.Error()); e != nil {
			return e
		}
		if tag.RowsAffected() > 0 {
			if e = notify(ctx, tx, j.Owner, expired.Error()); e != nil {
				return e
			}
		}
		return tx.Commit(ctx)
	}
	final := !retry || j.Attempts >= 6
	tx, e := conn.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	state := "pending"
	if final {
		state = "failed"
	}
	delay := time.Duration(1<<min(j.Attempts, 8)) * 5 * time.Second
	tag, e := tx.Exec(ctx, "UPDATE poster_posts SET status=$3,last_error=$4,next_attempt_at=$5 WHERE chat_id=$1 AND message_id=$2 AND status='processing'", j.Chat, j.Message, state, cause.Error(), time.Now().Add(delay))
	if e != nil {
		return e
	}
	if final && tag.RowsAffected() > 0 {
		if e = notify(ctx, tx, j.Owner, fmt.Sprintf("Не удалось доставить пост %d из канала %d в VK: %s. Задание сохранено. Проверьте /status и стену VK: при потерянном ответе публикация могла состояться. Новую копию автоматически создавать не буду.", j.Message, j.Chat, cause.Error())); e != nil {
			return e
		}
	}
	if e = tx.Commit(ctx); e == nil {
		s.Log.Warn("VK delivery deferred or failed", "chat_id", j.Chat, "message_id", j.Message, "attempt", j.Attempts, "final", final, "reason", cause.Error())
	}
	return e
}
func (s *Server) sendNotification(ctx context.Context, conn *pgxpool.Conn) error {
	var id, owner int64
	var body string
	var attempts int
	e := conn.QueryRow(ctx, "SELECT id,owner_id,body,attempts FROM poster_notifications WHERE NOT sent AND next_attempt_at<=now() ORDER BY id LIMIT 1").Scan(&id, &owner, &body, &attempts)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	e = s.API.Telegram(ctx, "sendMessage", map[string]any{"chat_id": owner, "text": body, "reply_markup": keyboard(), "disable_web_page_preview": true}, nil)
	if e == nil {
		_, e = conn.Exec(ctx, "UPDATE poster_notifications SET sent=TRUE,last_error=NULL WHERE id=$1", id)
		return e
	}
	delay := time.Duration(1<<min(attempts+1, 10)) * 5 * time.Second
	_, dbErr := conn.Exec(ctx, "UPDATE poster_notifications SET attempts=attempts+1,last_error=$2,next_attempt_at=$3 WHERE id=$1", id, e.Error(), time.Now().Add(delay))
	if dbErr != nil {
		return dbErr
	}
	return e
}
