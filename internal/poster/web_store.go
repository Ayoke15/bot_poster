package poster

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
)

// ImportWebSession is an operator command, never an HTTP/Telegram credential
// endpoint. The leader lock prevents changing accounts during a publication.
func (s *Store) ImportWebSession(ctx context.Context, api *API, owner, group int64, encrypted string) error {
	if owner <= 0 || group <= 0 {
		return errors.New("positive Telegram owner and VK group IDs required")
	}
	credential, e := s.Cipher.Decrypt(owner, encrypted)
	if e != nil {
		return errors.New("cannot decrypt session for this owner")
	}
	a, e := NewWebAPI(api, credential)
	if e != nil {
		return e
	}
	if a.Web.Session.GroupID != group {
		return errors.New("session belongs to another community")
	}
	if e = a.VerifyWebGroup(ctx, group); e != nil {
		return e
	}
	credential, e = a.Web.Credential()
	if e != nil {
		return e
	}
	encrypted, e = s.Cipher.Encrypt(owner, credential)
	if e != nil {
		return e
	}
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var locked bool
	if e = tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(82015002)").Scan(&locked); e != nil {
		return e
	}
	if !locked {
		return errors.New("stop the bot worker before importing a session; then restart it")
	}
	var oldGroup *int64
	if e = tx.QueryRow(ctx, "SELECT group_id FROM poster_users WHERE user_id=$1 FOR UPDATE", owner).Scan(&oldGroup); e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return e
	}
	if oldGroup != nil && *oldGroup != group {
		return errors.New("disconnect the previous VK community explicitly before importing a different one")
	}
	if _, e = tx.Exec(ctx, `INSERT INTO poster_users(user_id,group_id,token_cipher,token_version) VALUES($1,$2,$3,1)
 ON CONFLICT(user_id) DO UPDATE SET group_id=excluded.group_id,token_cipher=excluded.token_cipher,web_paused=FALSE,connect_step=0,pending_group=NULL`, owner, group, encrypted); e != nil {
		return e
	}
	// Keep post IDs/guid and queue versions, but do not reuse photos uploaded using
	// a different account or the old messages-upload workaround.
	if _, e = tx.Exec(ctx, `UPDATE poster_posts SET status='pending',vk_attachment='',uploaded_photo_file_id='',next_attempt_at=now()
 WHERE owner_id=$1 AND group_id=$2 AND status IN ('pending','processing')`, owner, group); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
