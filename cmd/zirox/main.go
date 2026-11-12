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
	"strings"
	"syscall"
	"time"

	"github.com/Boor0n/zirox-snmp-exporter/internal/engine"
	"github.com/Boor0n/zirox-snmp-exporter/internal/exporter"
	"github.com/Boor0n/zirox-snmp-exporter/internal/secret"
	"github.com/Boor0n/zirox-snmp-exporter/internal/snmpx"
	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
	"github.com/Boor0n/zirox-snmp-exporter/internal/tlsx"
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
	// The key goes into UI backups only when it lives in the data directory.
	var fileKey []byte
	if os.Getenv("ZIROX_SECRET_KEY") == "" {
		fileKey = key
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
	if st.NewerSchema > 0 {
		log.Warn("the database was written by a newer Zirox; running anyway, newer features stay unused", "db_schema", st.NewerSchema, "this_build", store.SchemaVersion())
	}
	if path, err := st.NoteVersion(version); err != nil {
		log.Warn("database backup before upgrade failed", "err", err)
	} else if path != "" {
		log.Info("database backed up after a version change", "file", path)
	}
	if err := st.EnsureDefaultProfiles(); err != nil {
		return err
	}
	custom, err := st.CustomOIDs()
	if err != nil {
		return err
	}
	snmpx.SetCustom(custom)

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
		MaxConcurrentPolls:     envInt("ZIROX_MAX_POLLS", 128),
		MaxConcurrentDiscovery: envInt("ZIROX_MAX_DISCOVERY", 32),
		Log:                    log,
	})
	if err := en.Start(ctx); err != nil {
		return err
	}

	tl, err := tlsx.Load(os.Getenv("ZIROX_TLS"), os.Getenv("ZIROX_TLS_CERT"), os.Getenv("ZIROX_TLS_KEY"), dataDir,
		strings.FieldsFunc(os.Getenv("ZIROX_TLS_HOSTS"), func(r rune) bool { return r == ',' || r == ' ' }))
	if err != nil {
		return err
	}
	// Over HTTPS the session cookie is Secure unless explicitly turned off.
	cookieSecure := os.Getenv("ZIROX_COOKIE_SECURE") == "1" || (tl != nil && os.Getenv("ZIROX_COOKIE_SECURE") != "0")
	wcfg := web.Config{CookieSecure: cookieSecure, Version: version, SecretKey: fileKey}
	if tl != nil {
		wcfg.TLSCertFile, wcfg.TLSSelfSigned, wcfg.TLSFingerprint, wcfg.TLSNotAfter = tl.CertFile, tl.SelfSigned, tl.Fingerprint, tl.NotAfter
	}
	ui, err := web.New(st, en, box, wcfg, log)
	if err != nil {
		return err
	}
	if os.Getenv("ZIROX_UPDATE_CHECK") != "0" {
		ui.StartUpdateCheck(ctx)
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
	if tl != nil {
		srv.TLSConfig = tl.Config
	}
	errc := make(chan error, 1)
	go func() {
		if tl != nil {
			log.Info("listening with HTTPS", "addr", srv.Addr, "version", version, "self_signed", tl.SelfSigned,
				"cert", tl.CertFile, "sha256", tl.Fingerprint, "expires", tl.NotAfter.Format("2006-01-02"))
			errc <- srv.ListenAndServeTLS("", "") // the certificate comes from TLSConfig
		} else {
			log.Info("listening", "addr", srv.Addr, "version", version)
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
