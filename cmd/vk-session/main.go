// vk-session runs login on the operator's computer; only the encrypted bundle
// is transferred to the server. Passwords/OTP are entered on VK's own page.
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/Ayoke15/bot_poster/internal/poster"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: vk-session login|import --owner TELEGRAM_ID --group VK_GROUP_ID --file session.vkenc")
	}
	flags := flag.NewFlagSet("vk-session", flag.ContinueOnError)
	owner := flags.Int64("owner", 0, "Telegram owner ID")
	group := flags.Int64("group", 0, "positive VK community ID")
	file := flags.String("file", "session.vkenc", "encrypted session file (never a plaintext cookie file)")
	if e := flags.Parse(os.Args[2:]); e != nil {
		return errors.New("invalid options")
	}
	if *owner <= 0 || *group <= 0 {
		return errors.New("--owner and --group must be positive")
	}
	key, e := base64.StdEncoding.DecodeString(os.Getenv("TOKEN_ENCRYPTION_KEY"))
	if e != nil || len(key) != 32 {
		return errors.New("set TOKEN_ENCRYPTION_KEY to the bot's existing 32-byte base64 key, through the environment")
	}
	cipher, e := poster.NewTokenCipher(key)
	if e != nil {
		return errors.New("invalid encryption key")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	api := poster.NewAPI(poster.Config{VKVersion: "5.199"})
	switch os.Args[1] {
	case "login":
		if _, e = os.Lstat(*file); !os.IsNotExist(e) {
			return errors.New("output already exists or cannot be accessed; choose a new --file")
		}
		opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.Flag("headless", false), chromedp.Flag("incognito", true), chromedp.Flag("disable-sync", true))
		alloc, closeAlloc := chromedp.NewExecAllocator(ctx, opts...)
		defer closeAlloc()
		browser, closeBrowser := chromedp.NewContext(alloc)
		defer closeBrowser()
		if e = chromedp.Run(browser, chromedp.Navigate("https://vk.ru/")); e != nil {
			return errors.New("cannot open Chrome/Chromium; run login on a computer with a graphical desktop")
		}
		fmt.Println("Войдите в VK в открытом отдельном окне. Пароль и код вводите только на странице VK. После входа нажмите Enter здесь.")
		ready := make(chan struct{})
		go func() { _, _ = bufio.NewReader(os.Stdin).ReadString('\n'); close(ready) }()
		select {
		case <-ctx.Done():
			return errors.New("login cancelled or timed out")
		case <-ready:
		}
		session := poster.WebSession{GroupID: *group}
		if e = chromedp.Run(browser, chromedp.Evaluate("navigator.userAgent", &session.UserAgent), chromedp.ActionFunc(func(c context.Context) error {
			cookies, err := storage.GetCookies().Do(c)
			if err != nil {
				return err
			}
			for _, cookie := range cookies {
				d := strings.TrimPrefix(cookie.Domain, ".")
				if d != "vk.ru" && !strings.HasSuffix(d, ".vk.ru") {
					continue
				}
				session.Cookies = append(session.Cookies, poster.WebCookie{Name: cookie.Name, Value: cookie.Value, Domain: cookie.Domain, Path: cookie.Path, Expires: cookie.Expires, Secure: cookie.Secure, HTTPOnly: cookie.HTTPOnly})
			}
			return nil
		})); e != nil {
			return errors.New("cannot read the dedicated VK browser session")
		}
		data, e := json.Marshal(session)
		if e != nil {
			return errors.New("cannot encode session")
		}
		web, e := poster.NewWebAPI(api, poster.WebCredentialPrefix+string(data))
		if e != nil {
			return errors.New("VK login cookies not found; complete login and try again")
		}
		if e = web.VerifyWebGroup(ctx, *group); e != nil {
			return fmt.Errorf("VK session/community verification: %w", e)
		}
		credential, e := web.Web.Credential()
		if e != nil {
			return errors.New("cannot encode session")
		}
		encrypted, e := cipher.Encrypt(*owner, credential)
		if e != nil {
			return errors.New("cannot encrypt session")
		}
		out, e := os.OpenFile(*file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return errors.New("cannot create encrypted session file")
		}
		_, e = io.WriteString(out, encrypted)
		closeErr := out.Close()
		if e != nil || closeErr != nil {
			return errors.New("cannot write encrypted session file")
		}
		fmt.Println("Зашифрованная сессия сохранена. Перенесите файл на свой сервер по SSH и выполните import. В чат файл не отправляйте.")
	case "import":
		in, e := os.Open(*file)
		if e != nil {
			return errors.New("cannot open encrypted session file")
		}
		defer in.Close()
		data, e := io.ReadAll(io.LimitReader(in, 256*1024+1))
		if e != nil || len(data) > 256*1024 {
			return errors.New("invalid session file size")
		}
		store, e := poster.OpenStore(ctx, poster.Config{Key: key, DatabaseURL: os.Getenv("DATABASE_URL")})
		if e != nil {
			return errors.New("cannot open database; check DATABASE_URL and encryption key")
		}
		defer store.Pool.Close()
		if e = store.ImportWebSession(ctx, api, *owner, *group, strings.TrimSpace(string(data))); e != nil {
			return e
		}
		fmt.Println("Сессия импортирована. Запустите бот; /status покажет очередь. Для первого подключения настройте канал и /enable.")
	default:
		return errors.New("expected login or import")
	}
	return nil
}
