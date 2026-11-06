package poster

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

type Config struct {
	DatabaseURL, BotToken, WebhookSecret, BaseURL, VKVersion, Addr string
	Key                                                            []byte
}

func LoadConfig() (Config, error) {
	c := Config{DatabaseURL: os.Getenv("DATABASE_URL"), BotToken: os.Getenv("TELEGRAM_BOT_TOKEN"), WebhookSecret: os.Getenv("TELEGRAM_WEBHOOK_SECRET"), BaseURL: strings.TrimRight(os.Getenv("APP_BASE_URL"), "/"), VKVersion: os.Getenv("VK_API_VERSION"), Addr: os.Getenv("LISTEN_ADDR")}
	if c.Addr == "" {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8000"
		}
		c.Addr = ":" + port
	}
	if c.VKVersion == "" {
		c.VKVersion = "5.199"
	}
	var err error
	c.Key, err = base64.StdEncoding.DecodeString(os.Getenv("TOKEN_ENCRYPTION_KEY"))
	if err != nil || len(c.Key) != 32 {
		return c, errors.New("TOKEN_ENCRYPTION_KEY must be base64 encoding of 32 random bytes")
	}
	if !regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`).MatchString(c.BotToken) {
		return c, errors.New("invalid TELEGRAM_BOT_TOKEN format")
	}
	botID, err := strconv.ParseInt(strings.Split(c.BotToken, ":")[0], 10, 64)
	if err != nil || botID <= 0 {
		return c, errors.New("invalid TELEGRAM_BOT_TOKEN identity")
	}
	_, port, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return c, errors.New("LISTEN_ADDR must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return c, errors.New("HTTP port must be between 1 and 65535")
	}
	if c.DatabaseURL == "" || c.BotToken == "" {
		return c, errors.New("DATABASE_URL and TELEGRAM_BOT_TOKEN are required")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{16,256}$`).MatchString(c.WebhookSecret) {
		return c, errors.New("TELEGRAM_WEBHOOK_SECRET must contain 16–256 letters, digits, underscores or hyphens")
	}
	u, e := url.Parse(c.BaseURL)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return c, errors.New("APP_BASE_URL must be a public HTTPS base URL")
	}
	return c, nil
}

type TokenCipher struct{ aead cipher.AEAD }

func NewTokenCipher(key []byte) (*TokenCipher, error) {
	b, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	a, e := cipher.NewGCM(b)
	return &TokenCipher{a}, e
}
func (c *TokenCipher) Encrypt(user int64, token string) (string, error) {
	n := make([]byte, c.aead.NonceSize())
	if _, e := io.ReadFull(rand.Reader, n); e != nil {
		return "", e
	}
	out := c.aead.Seal(n, n, []byte(token), []byte(fmt.Sprint(user)))
	return "v1:" + base64.StdEncoding.EncodeToString(out), nil
}
func (c *TokenCipher) Decrypt(user int64, encoded string) (string, error) {
	if !strings.HasPrefix(encoded, "v1:") {
		return "", errors.New("invalid ciphertext")
	}
	b, e := base64.StdEncoding.DecodeString(strings.TrimPrefix(encoded, "v1:"))
	if e != nil || len(b) < c.aead.NonceSize() {
		return "", errors.New("invalid ciphertext")
	}
	n := c.aead.NonceSize()
	p, e := c.aead.Open(nil, b[:n], b[n:], []byte(fmt.Sprint(user)))
	if e != nil {
		return "", errors.New("token authentication failed")
	}
	return string(p), nil
}
