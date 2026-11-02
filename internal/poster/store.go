package poster

import (
	"context"
	_ "embed"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

type Store struct {
	Pool   *pgxpool.Pool
	Cipher *TokenCipher
}

func OpenStore(ctx context.Context, c Config) (*Store, error) {
	cipher, e := NewTokenCipher(c.Key)
	if e != nil {
		return nil, e
	}
	p, e := pgxpool.New(ctx, c.DatabaseURL)
	if e != nil {
		return nil, fmt.Errorf("invalid database configuration")
	}
	s := &Store{p, cipher}
	if e = p.Ping(ctx); e != nil {
		p.Close()
		return nil, fmt.Errorf("database unavailable")
	}
	if e = s.migrate(ctx); e != nil {
		p.Close()
		return nil, e
	}
	return s, nil
}
func (s *Store) migrate(ctx context.Context) error {
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return fmt.Errorf("migration transaction unavailable")
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(82015001)"); e != nil {
		return fmt.Errorf("migration lock unavailable")
	}
	if _, e = tx.Exec(ctx, schema); e != nil {
		return fmt.Errorf("schema migration failed")
	}
	var done bool
	if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM poster_migrations WHERE name='python_v1')").Scan(&done); e != nil {
		return e
	}
	if !done {
		var legacy bool
		if e = tx.QueryRow(ctx, "SELECT to_regclass('vk_accounts') IS NOT NULL AND to_regclass('user_settings') IS NOT NULL").Scan(&legacy); e != nil {
			return e
		}
		if legacy {
			rows, e := tx.Query(ctx, "SELECT coalesce(s.tg_user_id,a.tg_user_id),s.selected_vk_group_id,coalesce(a.access_token,'') FROM user_settings s FULL JOIN vk_accounts a ON a.tg_user_id=s.tg_user_id")
			if e != nil {
				return fmt.Errorf("legacy data read failed")
			}
			type oldUser struct {
				id    int64
				group *int64
				token string
			}
			var old []oldUser
			for rows.Next() {
				var u oldUser
				if e = rows.Scan(&u.id, &u.group, &u.token); e != nil {
					rows.Close()
					return fmt.Errorf("legacy data decode failed")
				}
				old = append(old, u)
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return fmt.Errorf("legacy data read failed")
			}
			for _, u := range old {
				var encrypted *string
				if u.token != "" {
					v, e := s.Cipher.Encrypt(u.id, u.token)
					if e != nil {
						return e
					}
					encrypted = &v
				}
				if _, e = tx.Exec(ctx, "INSERT INTO poster_users(user_id,group_id,token_cipher,token_version) VALUES($1,$2,$3,1)", u.id, u.group, encrypted); e != nil {
					return fmt.Errorf("legacy token migration failed")
				}
			}
			// Plaintext is erased only in the same transaction that persists encrypted copies.
			if _, e = tx.Exec(ctx, "UPDATE vk_accounts SET access_token=''; UPDATE user_settings SET enabled=FALSE"); e != nil {
				return fmt.Errorf("legacy plaintext cleanup failed")
			}
		}
		if _, e = tx.Exec(ctx, "INSERT INTO poster_migrations(name) VALUES('python_v1')"); e != nil {
			return e
		}
	}
	rows, e := tx.Query(ctx, "SELECT user_id,token_cipher FROM poster_users WHERE token_cipher IS NOT NULL")
	if e != nil {
		return e
	}
	for rows.Next() {
		var id int64
		var token string
		if e = rows.Scan(&id, &token); e != nil {
			rows.Close()
			return e
		}
		if _, e = s.Cipher.Decrypt(id, token); e != nil {
			rows.Close()
			return fmt.Errorf("TOKEN_ENCRYPTION_KEY cannot decrypt stored tokens; restore the original key")
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func ensureUser(ctx context.Context, tx pgx.Tx, id int64) error {
	_, e := tx.Exec(ctx, "INSERT INTO poster_users(user_id) VALUES($1) ON CONFLICT DO NOTHING", id)
	return e
}
func notify(ctx context.Context, tx pgx.Tx, user int64, text string) error {
	_, e := tx.Exec(ctx, "INSERT INTO poster_notifications(owner_id,body) VALUES($1,$2)", user, text)
	return e
}
