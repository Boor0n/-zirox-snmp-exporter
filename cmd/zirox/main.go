// Command zirox is an SNMP exporter for Prometheus with a web UI.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/Boor0n/zirox-snmp-exporter/internal/engine"
	"github.com/Boor0n/zirox-snmp-exporter/internal/exporter"
	"github.com/Boor0n/zirox-snmp-exporter/internal/secret"
	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
	"github.com/Boor0n/zirox-snmp-exporter/internal/web"
)

var version = "dev"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	n, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return def
	}
	return n
}

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	dataDir := env("ZIROX_DATA_DIR", "./data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	key, err := secret.LoadKey(os.Getenv("ZIROX_SECRET_KEY"), filepath.Join(dataDir, "secret.key"))
	if err != nil {
		return err
	}
	box, err := secret.New(key)
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(dataDir, "zirox.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.EnsureDefaultProfiles(); err != nil {
		return err
	}

	adminUser := env("ZIROX_ADMIN_USER", "admin")
	adminPass := os.Getenv("ZIROX_ADMIN_PASSWORD")
	generated := false
	if adminPass == "" {
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		adminPass, generated = base64.RawURLEncoding.EncodeToString(b), true
	}
	reset := os.Getenv("ZIROX_RESET_ADMIN_PASSWORD") == "1" && !generated
	created, err := web.EnsureAdmin(st, adminUser, adminPass, reset)
	if err != nil {
		return err
	}
	if created && generated {
		log.Warn("admin account created with a generated password; change it in Settings", "user", adminUser, "password", adminPass)
	} else if created {
		log.Info("admin account set", "user", adminUser)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	en := engine.New(st, box, engine.Options{
		MaxConcurrentPolls:     envInt("ZIROX_MAX_POLLS", 64),
		MaxConcurrentDiscovery: envInt("ZIROX_MAX_DISCOVERY", 8),
		Log:                    log,
	})
	if err := en.Start(ctx); err != nil {
		return err
	}

	ui, err := web.New(st, en, box, web.Config{CookieSecure: os.Getenv("ZIROX_COOKIE_SECURE") == "1", Version: version}, log)
	if err != nil {
		return err
	}
	exp := exporter.New(st, en)
	exp.SDAddress = os.Getenv("ZIROX_SD_ADDRESS")
	exp.Token = os.Getenv("ZIROX_SCRAPE_TOKEN")

	mux := http.NewServeMux()
	exp.Register(mux)
	ui.Register(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })

	srv := &http.Server{
		Addr:              env("ZIROX_LISTEN", ":9116"),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr, "version", version)
		cert, keyFile := os.Getenv("ZIROX_TLS_CERT"), os.Getenv("ZIROX_TLS_KEY")
		if cert != "" && keyFile != "" {
			errc <- srv.ListenAndServeTLS(cert, keyFile)
		} else {
			errc <- srv.ListenAndServe()
		}
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shCtx)
	en.Wait()
	return nil
}
