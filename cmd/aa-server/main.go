// aa-server runs the native sessions service against PostgreSQL.
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
	"strings"
	"syscall"
	"time"

	"aa-server/internal/cryptocompat"
	"aa-server/internal/deployment"
	"aa-server/internal/httpserver"
	"aa-server/internal/postgres"
)

func enabled(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes":
		return true
	}
	return false
}
func run() error {
	_ = flag.Bool("experimental", false, "deprecated compatibility flag")
	address := flag.String("listen", "127.0.0.1:8082", "HTTP listen address")
	static := flag.String("static", "static", "existing static file directory")
	initialize := flag.Bool("init-schema", false, "explicitly initialize the PostgreSQL schema and admin hash")
	flag.Parse()
	if os.Getenv("AAID_JWKS_URL") == "" || os.Getenv("AAID_PUBLIC_URL") == "" {
		return errors.New("AAID_JWKS_URL and AAID_PUBLIC_URL are required; anonymous API access is disabled")
	}
	publicURL, err := deployment.PublicURL(os.Getenv("PUBLIC_URL"), deployment.SiteURL)
	if err != nil {
		return fmt.Errorf("PUBLIC_URL: %w", err)
	}
	identityURL, err := deployment.PublicURL(os.Getenv("AAID_PUBLIC_URL"), "")
	if err != nil {
		return fmt.Errorf("AAID_PUBLIC_URL: %w", err)
	}
	keyPath := os.Getenv("MASTER_KEY_FILE")
	if keyPath == "" {
		return errors.New("MASTER_KEY_FILE must point to the preserved master.key")
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return errors.New("could not read MASTER_KEY_FILE")
	}
	if _, err = cryptocompat.NewFernet(string(key)); err != nil {
		return errors.New("MASTER_KEY_FILE does not contain a valid preserved Fernet key")
	}
	maximum := int64(16)
	if raw := os.Getenv("DATABASE_MAX_CONNECTIONS"); raw != "" {
		maximum, err = strconv.ParseInt(raw, 10, 32)
		if err != nil || maximum < 2 || maximum > 1024 {
			return errors.New("DATABASE_MAX_CONNECTIONS must be between 2 and 1024")
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	store, err := postgres.Open(ctx, os.Getenv("DATABASE_URL"), int32(maximum))
	if err != nil {
		return err
	}
	defer store.Close()
	if *initialize {
		if err = store.Initialize(ctx); err != nil {
			return err
		}
	}
	vault, err := postgres.NewVault(store, string(key))
	if err != nil {
		return err
	}
	if *initialize {
		if err = vault.InitializeAdmin(ctx, os.Getenv("ADMIN_PASSWORD")); err != nil {
			return err
		}
	}
	var admin bool
	if err = store.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM aa.config WHERE key='admin_password')").Scan(&admin); err != nil {
		return errors.New("PostgreSQL schema is missing or incompatible")
	}
	if !admin {
		return errors.New("preserved admin credentials are missing; import data or explicitly initialize the empty development database")
	}
	cfg := httpserver.Config{Vault: vault, MasterKey: string(key), AAIDURL: identityURL, PublicURL: publicURL, StaticDir: *static, EnforceAAID: true, PanelLocalOnly: enabled(os.Getenv("PANEL_LOCAL_ONLY"))}
	cfg.RequireIdentityState = true
	cfg.MetricsFile = os.Getenv("METRICS_FILE")
	if endpoint := os.Getenv("AAID_JWKS_URL"); endpoint != "" {
		verifier := cryptocompat.NewVerifier(endpoint, nil)
		defer verifier.Close()
		if issuer := os.Getenv("AAID_ISSUER"); issuer != "" {
			verifier.Issuer = issuer
		}
		if audience := os.Getenv("AAID_AUDIENCE"); audience != "" {
			verifier.Audience = audience
		}
		cfg.Verifier = verifier
	}
	refreshSeconds, refreshErr := strconv.Atoi(os.Getenv("REFRESH_SECONDS"))
	if refreshErr != nil {
		refreshSeconds = 180
	}
	if refreshSeconds < 60 {
		refreshSeconds = 60
	}
	cfg.RefreshEvery = time.Duration(refreshSeconds) * time.Second
	for name, target := range map[string]*time.Duration{"RENEW_BEFORE_SECONDS": &cfg.RenewBefore, "RENEW_INTERVAL_SECONDS": &cfg.RenewInterval, "RENEW_BACKOFF_SECONDS": &cfg.RenewBackoff} {
		if value, err := strconv.Atoi(os.Getenv(name)); err == nil {
			if value < 60 {
				value = 60
			}
			*target = time.Duration(value) * time.Second
		}
	}
	app, err := httpserver.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer app.Close()
	server := &http.Server{Addr: *address, Handler: app, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 180 * time.Second, IdleTimeout: 75 * time.Second, MaxHeaderBytes: 1 << 20}
	errorsChannel := make(chan error, 1)
	go func() { errorsChannel <- server.ListenAndServe() }()
	fmt.Fprintf(os.Stderr, "Go sessions service listening on %s\n", *address)
	select {
	case err := <-errorsChannel:
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
