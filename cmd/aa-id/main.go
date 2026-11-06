// aa-id runs the native identity issuer against PostgreSQL.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"aa-server/internal/browser"
	"aa-server/internal/deployment"
	"aa-server/internal/identityserver"
	"aa-server/internal/postgres"
)

func number(name string, fallback, minimum int64) int64 {
	value, err := strconv.ParseInt(os.Getenv(name), 10, 64)
	if err != nil {
		return fallback
	}
	if value < minimum {
		return minimum
	}
	return value
}
func run() error {
	_ = flag.Bool("experimental", false, "deprecated compatibility flag")
	address := flag.String("listen", "127.0.0.1:8093", "HTTP listen address")
	static := flag.String("static", "services/aa-id/aaid/static", "existing AA ID static file directory")
	initialize := flag.Bool("init-schema", false, "explicitly initialize the PostgreSQL schema")
	flag.Parse()
	publicURL, err := deployment.PublicURL(os.Getenv("AAID_PUBLIC_URL"), deployment.IdentityURL)
	if err != nil {
		return fmt.Errorf("AAID_PUBLIC_URL: %w", err)
	}
	siteURL, err := deployment.PublicURL(os.Getenv("AA_SITE_URL"), "")
	if err != nil {
		return fmt.Errorf("AA_SITE_URL: %w", err)
	}
	path := os.Getenv("SIGNING_KEY_FILE")
	if path == "" {
		return errors.New("SIGNING_KEY_FILE must point to the preserved raw signing.key")
	}
	seed, err := os.ReadFile(path)
	if err != nil || len(seed) != 32 {
		return errors.New("SIGNING_KEY_FILE must contain the preserved 32-byte Ed25519 seed")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	connections := number("DATABASE_MAX_CONNECTIONS", 16, 2)
	if connections > 1024 {
		return errors.New("DATABASE_MAX_CONNECTIONS cannot exceed 1024")
	}
	store, err := postgres.Open(ctx, os.Getenv("DATABASE_URL"), int32(connections))
	if err != nil {
		return err
	}
	defer store.Close()
	if *initialize {
		if err = store.Initialize(ctx); err != nil {
			return err
		}
	}
	var schema bool
	if err = store.Pool.QueryRow(ctx, "SELECT to_regclass('aaid.users') IS NOT NULL").Scan(&schema); err != nil || !schema {
		return errors.New("PostgreSQL schema is missing; import data or explicitly initialize an empty development database")
	}
	engine := browser.New(ctx, browser.Config{DriverDirectory: os.Getenv("PLAYWRIGHT_DRIVER_PATH"), Identity: true, LoginSlots: int(number("LOGIN_SLOTS", 4, 1))})
	defer engine.Close()
	jobs := browser.NewJobs(ctx, engine)
	defer jobs.Close()
	jobs.MaxActive = int(number("MAX_ACTIVE_LOGINS", 16, 1))
	jobs.TTL = time.Duration(number("LOGIN_TTL_SECONDS", 600, 120)) * time.Second
	app, err := identityserver.New(ctx, identityserver.Config{Store: postgres.NewIdentity(store), Seed: seed, Issuer: os.Getenv("AAID_ISSUER"), Audience: os.Getenv("AAID_AUDIENCE"), AdminToken: os.Getenv("ADMIN_TOKEN"), SiteURL: siteURL, PublicURL: publicURL, SessionsURL: os.Getenv("SESSIONS_URL"), StaticDir: *static, TrustProxy: os.Getenv("TRUST_PROXY_IP") == "1", AccessTTL: number("ACCESS_TTL_SECONDS", 600, 60), SessionTTL: number("SESSION_TTL_SECONDS", 60*86400, 3600), WebTTL: number("WEB_SESSION_TTL_SECONDS", 30*86400, 600), MaxSessions: int(number("MAX_SESSIONS", 2, 1)), Logins: jobs})
	if err != nil {
		return err
	}
	defer app.Close()
	server := &http.Server{Addr: *address, Handler: app, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 75 * time.Second, MaxHeaderBytes: 1 << 20}
	failure := make(chan error, 1)
	go func() { failure <- server.ListenAndServe() }()
	fmt.Fprintf(os.Stderr, "Go AA ID listening on %s\n", *address)
	select {
	case err := <-failure:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
