package poster

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

func intString(i int64) string { return strconv.FormatInt(i, 10) }

var groupPattern = regexp.MustCompile(`^(?:club|public|event)?([0-9]+)$`)

func parseGroup(text string) (int64, error) {
	text = strings.TrimSpace(text)
	if !strings.Contains(text, "/") && !strings.Contains(text, ".") {
		n, e := strconv.ParseInt(text, 10, 64)
		if e == nil && n > 0 {
			return n, nil
		}
	}
	if !strings.Contains(text, "://") {
		text = "https://" + text
	}
	u, e := url.Parse(text)
	if e != nil {
		return 0, errors.New("invalid group")
	}
	host := strings.TrimPrefix(u.Hostname(), "www.")
	if host != "vk.com" && host != "m.vk.com" && host != "vk.ru" && host != "m.vk.ru" {
		return 0, errors.New("invalid group host")
	}
	path := strings.Trim(u.Path, "/")
	m := groupPattern.FindStringSubmatch(path)
	if m != nil {
		n, e := strconv.ParseInt(m[1], 10, 64)
		if e == nil && n > 0 {
			return n, nil
		}
	}
	for _, k := range []string{"sel", "oid", "z"} {
		v := u.Query().Get(k)
		if strings.HasPrefix(v, "-") {
			n, e := strconv.ParseInt(strings.TrimPrefix(v, "-"), 10, 64)
			if e == nil && n > 0 {
				return n, nil
			}
		}
	}
	return 0, errors.New("numeric group ID required")
}

const bindingWarning = "Важно: у одного Telegram-пользователя может быть только одно сообщество VK. Подключение новой группы заменит прежнюю; ожидающие публикации будут отменены. Уже отправляемый пост может успеть опубликоваться."

const helpText = "Я переношу текст и фотографии с подписями из Telegram в VK.\n1. Добавьте бота администратором канала.\n2. /connect_channel -100… — подключить канал (проверю права ваши и бота). ID можно взять у доверенного Telegram-клиента; пересылка поста не подтверждает владение.\n3. 🔗 Подключить VK — ссылка vk.com/club123, затем ключ сообщества с правом wall (или пользовательский ключ редактора/администратора группы). Или /set_vk <id> <токен>. Токен отправляйте только сюда. Для фотографий владелец сервера подключает браузерную сессию VK; cookies в чат не отправляйте.\n4. /enable — включить.\n/status — состояние и последние ошибки; /disable; /disconnect_channel <id>; /clear_vk.\nРедактирование обновляет существующий пост VK. Старые и неизвестные посты не публикуются.\n\n" + bindingWarning

var buttonCommands = map[string]string{"🔗 Подключить VK": "/connect_vk", "🔌 Как подключить": "/token_help", "📝 Формат /set_vk": "/format", "📊 Статус": "/status", "▶️ Включить": "/enable", "⏹ Выключить": "/disable", "🗑 Сбросить VK": "/clear_vk", "🏠 Главная": "/start"}

func keyboard() map[string]any {
	return map[string]any{"keyboard": [][]string{{"🔗 Подключить VK"}, {"🔌 Как подключить"}, {"📝 Формат /set_vk", "📊 Статус"}, {"▶️ Включить", "⏹ Выключить"}, {"🗑 Сбросить VK", "🏠 Главная"}}, "resize_keyboard": true}
}
func (s *Server) command(ctx context.Context, tx pgx.Tx, m Message) error {
	id := m.From.ID
	if e := ensureUser(ctx, tx, id); e != nil {
		return e
	}
	var step int
	var pending *int64
	if e := tx.QueryRow(ctx, "SELECT connect_step,pending_group FROM poster_users WHERE user_id=$1 FOR UPDATE", id).Scan(&step, &pending); e != nil {
		return e
	}
	text := strings.TrimSpace(m.Text)
	if mapped, ok := buttonCommands[text]; ok {
		text = mapped
	}
	parts := strings.Fields(text)
	cmd := ""
	if len(parts) > 0 {
		cmd = strings.Split(parts[0], "@")[0]
	}
	reply := func(t string) error { return notify(ctx, tx, id, t) }
	switch cmd {
	case "/start", "/token_help", "/help":
		if _, e := tx.Exec(ctx, "UPDATE poster_users SET connect_step=0,pending_group=NULL WHERE user_id=$1", id); e != nil {
			return e
		}
		return reply(helpText)
	case "/format":
		return reply("/set_vk <числовой_id_группы> <пользовательский токен VK>\nИли нажмите «Подключить VK».\n\n" + bindingWarning)
	case "/connect_vk":
		if _, e := tx.Exec(ctx, "UPDATE poster_users SET connect_step=1,pending_group=NULL WHERE user_id=$1", id); e != nil {
			return e
		}
		return reply("Пришлите ссылку на сообщество VK, например https://vk.com/club123456789.\n\n" + bindingWarning)
	case "/connect_channel":
		if len(parts) != 2 {
			return reply("Формат: /connect_channel -1001234567890. Вы и бот должны быть администраторами канала.")
		}
		chat, e := strconv.ParseInt(parts[1], 10, 64)
		if e != nil || chat >= 0 {
			return reply("Нужен отрицательный числовой ID канала.")
		}
		if e = s.checkAdmin(ctx, chat, id); e != nil {
			if temporary(e) {
				return e
			}
			return reply("Не удалось подтвердить права: вы и бот должны быть администраторами указанного канала.")
		}
		tag, e := tx.Exec(ctx, "INSERT INTO poster_channels(chat_id,owner_id) VALUES($1,$2) ON CONFLICT(chat_id) DO UPDATE SET owner_id=excluded.owner_id WHERE poster_channels.owner_id=excluded.owner_id", chat, id)
		if e != nil {
			return e
		}
		if tag.RowsAffected() == 0 {
			return reply("Канал уже подключён другим пользователем. Текущий владелец должен сначала отключить его.")
		}
		return reply("Канал подключён. Автоматической привязки к другим пользователям нет.")
	case "/disconnect_channel":
		if len(parts) != 2 {
			return reply("Формат: /disconnect_channel <id канала>")
		}
		chat, e := strconv.ParseInt(parts[1], 10, 64)
		if e != nil {
			return reply("Некорректный ID канала.")
		}
		tag, e := tx.Exec(ctx, "DELETE FROM poster_channels WHERE chat_id=$1 AND owner_id=$2", chat, id)
		if e != nil {
			return e
		}
		if tag.RowsAffected() == 0 {
			return reply("Канал не принадлежит вам.")
		}
		if _, e = tx.Exec(ctx, "UPDATE poster_posts SET status='cancelled',last_error='channel disconnected' WHERE chat_id=$1 AND owner_id=$2 AND status IN ('pending','processing')", chat, id); e != nil {
			return e
		}
		return reply("Канал отключён, ожидающие публикации отменены.")
	case "/set_vk":
		s.deleteTokenMessage(ctx, m)
		if len(parts) != 3 {
			return reply("Формат: /set_vk <id группы> <токен>.")
		}
		group, e := parseGroup(parts[1])
		if e != nil {
			return reply("Нужен положительный числовой ID группы VK.")
		}
		return s.saveToken(ctx, tx, m, group, parts[2])
	case "/enable":
		var ready bool
		if e := tx.QueryRow(ctx, "SELECT group_id IS NOT NULL AND token_cipher IS NOT NULL AND EXISTS(SELECT 1 FROM poster_channels WHERE owner_id=$1) FROM poster_users WHERE user_id=$1", id).Scan(&ready); e != nil {
			return e
		}
		if !ready {
			return reply("Сначала подключите свой канал и сообщество VK.")
		}
		if _, e := tx.Exec(ctx, "UPDATE poster_users SET enabled=TRUE WHERE user_id=$1", id); e != nil {
			return e
		}
		return reply("Автопостинг включён.")
	case "/disable":
		if _, e := tx.Exec(ctx, "UPDATE poster_users SET enabled=FALSE WHERE user_id=$1", id); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, "UPDATE poster_posts SET status='cancelled',last_error='disabled by owner' WHERE owner_id=$1 AND status IN ('pending','processing')", id); e != nil {
			return e
		}
		return reply("Автопостинг выключен. Ожидающие задания отменены; /enable действует на новые посты.")
	case "/clear_vk":
		if _, e := tx.Exec(ctx, "UPDATE poster_users SET token_cipher=NULL,group_id=NULL,enabled=FALSE,web_paused=FALSE,token_version=token_version+1,connect_step=0,pending_group=NULL WHERE user_id=$1", id); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, "UPDATE poster_posts SET status='cancelled',last_error='VK disconnected' WHERE owner_id=$1 AND status IN ('pending','processing')", id); e != nil {
			return e
		}
		return reply("Реквизиты VK удалены. Автопостинг выключен. Если подключали браузерную сессию, её можно завершить в настройках безопасности VK.")
	case "/status":
		var group *int64
		var enabled, hasToken, paused bool
		var channels, pendingJobs, failed int
		if e := tx.QueryRow(ctx, `SELECT group_id,enabled,token_cipher IS NOT NULL,web_paused,
   (SELECT count(*) FROM poster_channels WHERE owner_id=$1),
   (SELECT count(*) FROM poster_posts WHERE owner_id=$1 AND status IN ('pending','processing')),
   (SELECT count(*) FROM poster_posts WHERE owner_id=$1 AND status='failed') FROM poster_users WHERE user_id=$1`, id).Scan(&group, &enabled, &hasToken, &paused, &channels, &pendingJobs, &failed); e != nil {
			return e
		}
		g := "не подключена"
		if group != nil {
			g = intString(*group)
		}
		status := fmt.Sprintf("VK: %s; токен сохранён: %t; включено: %t\nКаналов: %d; в очереди: %d; ошибок: %d", g, hasToken, enabled, channels, pendingJobs, failed)
		if paused {
			status += "\nВеб-сессия VK: требуется повторный вход. Очередь приостановлена."
		}
		rows, e := tx.Query(ctx, "SELECT chat_id,message_id,last_error FROM poster_posts WHERE owner_id=$1 AND last_error IS NOT NULL ORDER BY desired_version DESC LIMIT 3", id)
		if e != nil {
			return e
		}
		for rows.Next() {
			var chat, msg int64
			var reason string
			if e = rows.Scan(&chat, &msg, &reason); e != nil {
				rows.Close()
				return e
			}
			status += fmt.Sprintf("\nКанал %d, пост %d: %s", chat, msg, reason)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		return reply(status)
	}
	if text != "" && !strings.HasPrefix(text, "/") && step == 1 {
		group, e := parseGroup(text)
		if e != nil {
			return reply("Нужна ссылка vk.com/club123 или числовой ID группы.")
		}
		if _, e = tx.Exec(ctx, "UPDATE poster_users SET connect_step=2,pending_group=$2 WHERE user_id=$1", id, group); e != nil {
			return e
		}
		return reply("Теперь пришлите токен выбранного сообщества с правом wall (или пользовательский токен редактора/администратора группы) отдельным сообщением. Попробую удалить сообщение после получения.\n\n" + bindingWarning)
	}
	if text != "" && !strings.HasPrefix(text, "/") && step == 2 && pending != nil {
		if group, e := parseGroup(text); e == nil {
			_, e = tx.Exec(ctx, "UPDATE poster_users SET pending_group=$2 WHERE user_id=$1", id, group)
			if e != nil {
				return e
			}
			return reply("Группа изменена. Пришлите токен.")
		}
		s.deleteTokenMessage(ctx, m)
		return s.saveToken(ctx, tx, m, *pending, text)
	}
	return reply("Неизвестная команда. /help — подключение и список команд.")
}
func (s *Server) deleteTokenMessage(ctx context.Context, m Message) {
	if e := s.API.Telegram(ctx, "deleteMessage", map[string]any{"chat_id": m.Chat.ID, "message_id": m.ID}, nil); e != nil {
		s.Log.Warn("token message deletion failed", "chat_id", m.Chat.ID, "message_id", m.ID)
	}
}
func (s *Server) saveToken(ctx context.Context, tx pgx.Tx, m Message, group int64, token string) error {
	id := m.From.ID
	if len(token) < 12 {
		return notify(ctx, tx, id, "Токен слишком короткий.")
	}
	if e := s.API.VerifyGroup(ctx, token, group); e != nil {
		return notify(ctx, tx, id, tokenVerificationMessage(e))
	}
	var oldGroup *int64
	if e := tx.QueryRow(ctx, "SELECT group_id FROM poster_users WHERE user_id=$1 FOR UPDATE", id).Scan(&oldGroup); e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return e
	}
	cipher, e := s.Store.Cipher.Encrypt(id, token)
	if e != nil {
		return e
	}
	// One row per Telegram user: a new community replaces the current binding.
	if _, e = tx.Exec(ctx, `INSERT INTO poster_users(user_id,group_id,token_cipher,token_version)
		VALUES($1,$2,$3,1)
		ON CONFLICT(user_id) DO UPDATE SET
		group_id=excluded.group_id,token_cipher=excluded.token_cipher,
		token_version=poster_users.token_version+1,web_paused=FALSE,connect_step=0,pending_group=NULL`, id, group, cipher); e != nil {
		return e
	}
	cancelled, e := tx.Exec(ctx, "UPDATE poster_posts SET status='cancelled',last_error='VK credentials changed' WHERE owner_id=$1 AND status IN ('pending','processing')", id)
	if e != nil {
		return e
	}
	notice := ""
	if oldGroup != nil {
		if *oldGroup != group {
			notice = fmt.Sprintf("Сообщество VK заменено: %d → %d. Старая привязка отключена.\n", *oldGroup, group)
		} else {
			notice = fmt.Sprintf("Токен сообщества VK %d обновлён.\n", group)
		}
		notice += fmt.Sprintf("Ожидающие задания отменены: %d. Уже отправляемый пост может успеть опубликоваться.\n\n", cancelled.RowsAffected())
	}
	return notify(ctx, tx, id, notice+"VK подключён. Проверены право wall и доступ к выбранному сообществу. Токен зашифрован. Если сообщение с токеном осталось в чате, удалите его вручную. Проверьте подключение канала через /connect_channel и включите /enable.")
}
func temporary(e error) bool { var api *APIError; return errors.As(e, &api) && api.Temporary }

func tokenVerificationMessage(err error) string {
	var verification *VerificationError
	if errors.As(err, &verification) {
		return "Токен не сохранён. " + verification.Message
	}
	if temporary(err) {
		return "VK временно недоступен или ограничил запросы. Проверить публикацию сейчас не удалось, токен не сохранён. Пришлите его повторно немного позже."
	}
	var api *APIError
	if errors.As(err, &api) && api.Code == 27 {
		return "VK отклонил авторизацию ключа (код 27). Токен не сохранён. Проверьте действительность ключа и право wall. Поддерживаются ключ выбранного сообщества и пользовательский ключ редактора/администратора группы."
	}
	if errors.As(err, &api) && (api.Code == 5 || api.Code == 28 || api.Code == 401) {
		return "VK отклонил токен: он неверный, отозван или истёк. Токен не сохранён. Получите новый пользовательский ключ VK с правом wall."
	}
	return "VK не подтвердил право публикации. Токен не сохранён. Проверьте, что это пользовательский ключ VK с правом wall от редактора или администратора выбранного сообщества, и что стена включена."
}
