// Command noctune runs the Discord bot and its web control panel in one
// process. Everything is configured from the environment (see
// internal/config) and nothing is persisted to disk — restarting the
// process is always safe and simply starts every guild back at idle.
package main

import (
	"context"
	"errors"
	"fmt"
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

const (
	shutdownTimeout   = 10 * time.Second
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	idleTimeout       = 60 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatalf("noctune: %v", err)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	client, err := bot.NewClient(cfg)
	if err != nil {
		return fmt.Errorf("create discord client: %w", err)
	}

	ytClient := youtube.New(cfg.YtDlpPath, cfg.CacheDir)
	spClient := spotify.New(ctx, cfg.SpotifyClientID, cfg.SpotifyClientSecret)
	resolver := resolve.New(spClient, ytClient)
	players := player.NewManager(client, ytClient, cfg)
	discordBot := bot.New(cfg, client, players, resolver)

	if err := discordBot.OpenContext(ctx); err != nil {
		return fmt.Errorf("connect to discord: %w", err)
	}

	webServer, err := api.New(cfg, client, players, resolver)
	if err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = discordBot.CloseContext(closeCtx)
		return fmt.Errorf("create web server: %w", err)
	}

	httpServer := newHTTPServer(cfg.WebListenAddr, webServer.Handler())
	serverErr := make(chan error, 1)
	go func() {
		log.Printf("noctune: web GUI listening on %s", cfg.WebListenAddr)
		err := httpServer.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serverErr <- err
	}()

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-serverErr:
	}
	log.Println("noctune: shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	var shutdownErrs []error
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		shutdownErrs = append(shutdownErrs, fmt.Errorf("shut down web server: %w", err))
		_ = httpServer.Close()
	}
	if err := webServer.Close(shutdownCtx); err != nil {
		shutdownErrs = append(shutdownErrs, fmt.Errorf("close web workers: %w", err))
	}
	if err := players.Close(shutdownCtx); err != nil {
		shutdownErrs = append(shutdownErrs, fmt.Errorf("close players: %w", err))
	}
	if err := discordBot.CloseContext(shutdownCtx); err != nil {
		shutdownErrs = append(shutdownErrs, fmt.Errorf("close discord: %w", err))
	}
	if serveErr != nil {
		shutdownErrs = append(shutdownErrs, fmt.Errorf("web server: %w", serveErr))
	}
	return errors.Join(shutdownErrs...)
}

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
	}
}
