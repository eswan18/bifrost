package main

import (
	"context"
	"log"
	"net/http"
	"os"
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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	oidcClient, err := auth.NewOIDC(ctx,
		cfg.OIDCIssuerExternal, cfg.OIDCIssuerInternal,
		cfg.OIDCClientID, cfg.OIDCClientSecret,
		cfg.BaseURL+"/auth/callback",
	)
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

	log.Printf("bifrost (%s) listening on %s", cfg.Env, cfg.HTTPAddress)
	if err := http.ListenAndServe(cfg.HTTPAddress, mux); err != nil {
		log.Fatal(err)
	}
}
