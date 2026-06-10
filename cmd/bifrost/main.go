package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/eswan18/bifrost/internal/auth"
	"github.com/eswan18/bifrost/internal/config"
	"github.com/eswan18/bifrost/internal/kube"
	"github.com/eswan18/bifrost/internal/web"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	tmplDir := os.Getenv("TEMPLATES_DIR")
	if tmplDir == "" {
		tmplDir = "templates"
	}
	staticDir := os.Getenv("STATIC_DIR")
	if staticDir == "" {
		staticDir = "static"
	}

	rend, err := web.LoadTemplates(tmplDir)
	if err != nil {
		log.Fatalf("templates: %v", err)
	}

	kc, err := kube.New(cfg.ArgoCDNamespace)
	if err != nil {
		log.Fatalf("kube: %v", err)
	}

	oidcCtx, oidcCancel := context.WithTimeout(context.Background(), 15*time.Second)
	oidcClient, err := auth.NewOIDC(oidcCtx,
		cfg.OIDCIssuerExternal, cfg.OIDCIssuerInternal,
		cfg.OIDCClientID, cfg.OIDCClientSecret,
		cfg.BaseURL+"/auth/callback",
	)
	oidcCancel()
	if err != nil {
		log.Fatalf("oidc: %v", err)
	}

	sm := auth.NewSessionManager(cfg.SessionSecret, 12*time.Hour)
	authH := &auth.Handlers{OIDC: oidcClient, Session: sm}
	webH := &web.Handlers{Cfg: cfg, Kube: kc, Renderer: rend}

	requireAuth := auth.RequireAuth(sm, cfg.AllowedEmail, "/auth/login")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /auth/login", authH.Login)
	mux.HandleFunc("GET /auth/callback", authH.Callback)
	mux.HandleFunc("POST /auth/logout", authH.Logout)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir(staticDir))))
	mux.Handle("GET /", requireAuth(http.HandlerFunc(webH.Status)))
	mux.Handle("POST /services/{name}/promote", requireAuth(http.HandlerFunc(webH.Promote)))

	srv := &http.Server{
		Addr:              cfg.HTTPAddress,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Staging restarts on every auto-deploy and bifrost promotes itself in
	// prod — drain in-flight requests on SIGTERM instead of dropping them.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("bifrost (%s) listening on %s", cfg.Env, cfg.HTTPAddress)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	case <-ctx.Done():
		log.Printf("signal received, shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}
}
