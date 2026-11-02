package main

import (
	"context"
	"github.com/Ayoke15/bot_poster/internal/poster"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	c, e := poster.LoadConfig()
	if e != nil {
		log.Error(e.Error())
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, e := poster.OpenStore(ctx, c)
	if e != nil {
		log.Error("database initialization failed; check connectivity, schema and encryption key")
		os.Exit(1)
	}
	defer store.Pool.Close()
	s := poster.NewServer(c, store, poster.NewAPI(c), log)
	s.BotID, e = strconv.ParseInt(strings.Split(c.BotToken, ":")[0], 10, 64)
	if e != nil || s.BotID <= 0 {
		log.Error("invalid Telegram bot token format")
		os.Exit(1)
	}
	var wg sync.WaitGroup
	for _, run := range []func(context.Context){s.RunWorker, s.RunWebhookMonitor} {
		wg.Add(1)
		go func(f func(context.Context)) { defer wg.Done(); f(ctx) }(run)
	}
	server := &http.Server{Addr: c.Addr, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 45 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		log.Info("HTTP server starting", "address", c.Addr)
		if e := server.ListenAndServe(); e != nil && e != http.ErrServerClosed {
			log.Error("HTTP server failed")
			stop()
		}
	}()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	wg.Wait()
}
