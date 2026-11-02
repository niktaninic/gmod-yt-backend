// Command server is the GMod 3D Stream Radio YouTube/Spotify converter.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/niktaninic/gmod-yt-backend/yt-converter-server/internal/config"
	"github.com/niktaninic/gmod-yt-backend/yt-converter-server/internal/server"
	"github.com/niktaninic/gmod-yt-backend/yt-converter-server/internal/store"
	"github.com/niktaninic/gmod-yt-backend/yt-converter-server/internal/ytdlp"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe /api/health on localhost and exit (for Docker HEALTHCHECK)")
	envFile := flag.String("env", ".env", "optional .env file")
	flag.Parse()

	setupLogging()

	if *healthcheck {
		os.Exit(runHealthcheck())
	}

	cfg, err := config.Load(*envFile)
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		slog.Error("database", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	yt := ytdlp.New(cfg)
	srv := server.New(ctx, cfg, st, yt)

	httpSrv := &http.Server{
		Addr:              net.JoinHostPort(cfg.ListenAddr, strconv.Itoa(cfg.Port)),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// no WriteTimeout: /stream responses for slow clients and the SSE
		// endpoint may legitimately run for minutes
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 32 << 10,
		BaseContext:    func(net.Listener) context.Context { return ctx },
	}

	go func() {
		slog.Info("listening", "addr", httpSrv.Addr,
			"spotify", cfg.SpotifyEnabled(), "devConsole", cfg.DevConsoleEnabled,
			"workers", cfg.MaxConcurrentConversions, "maxDuration", cfg.MaxDurationSeconds)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	_ = st.Close()
	slog.Info("bye")
}

func setupLogging() {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if strings.EqualFold(os.Getenv("LOG_FORMAT"), "json") {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(h))
}

func runHealthcheck() int {
	port := os.Getenv("PORT")
	if port == "" {
		port = "9999"
	}
	c := http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%s/api/health", port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "health status", resp.StatusCode)
		return 1
	}
	return 0
}
