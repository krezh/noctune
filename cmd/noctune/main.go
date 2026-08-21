// Command noctune runs the Discord bot and its web control panel in one
// process. Everything is configured from the environment (see
// internal/config) and nothing is persisted to disk — restarting the
// process is always safe and simply starts every guild back at idle.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/krezh/noctune/internal/api"
	"github.com/krezh/noctune/internal/bot"
	"github.com/krezh/noctune/internal/config"
	"github.com/krezh/noctune/internal/player"
	"github.com/krezh/noctune/internal/resolve"
	"github.com/krezh/noctune/internal/spotify"
	"github.com/krezh/noctune/internal/youtube"
)

const shutdownTimeout = 10 * time.Second

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("noctune: config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := bot.NewClient(cfg)
	if err != nil {
		log.Fatalf("noctune: create discord client: %v", err)
	}

	ytClient := youtube.New(cfg.YtDlpPath, cfg.CacheDir)
	spClient := spotify.New(ctx, cfg.SpotifyClientID, cfg.SpotifyClientSecret)
	resolver := resolve.New(spClient, ytClient)
	players := player.NewManager(client, ytClient, cfg)
	discordBot := bot.New(cfg, client, players, resolver)

	if err := discordBot.Open(); err != nil {
		log.Fatalf("noctune: connect to discord: %v", err)
	}
	defer func() {
		if err := discordBot.Close(); err != nil {
			log.Printf("noctune: close discord connection: %v", err)
		}
	}()

	webServer, err := api.New(cfg, client, players, resolver)
	if err != nil {
		log.Fatalf("noctune: create web server: %v", err)
	}

	httpServer := &http.Server{
		Addr:    cfg.WebListenAddr,
		Handler: webServer.Handler(),
	}
	go func() {
		log.Printf("noctune: web GUI listening on %s", cfg.WebListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("noctune: web server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("noctune: shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}
