package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Embed the IANA timezone database: the alpine runtime image ships no
	// tzdata, so DISPLAY_TIMEZONE's time.LoadLocation would fail at startup
	// (crashloop, July 2026).
	_ "time/tzdata"

	"github.com/eswan18/bifrost/internal/auth"
	"github.com/eswan18/bifrost/internal/config"
	"github.com/eswan18/bifrost/internal/gcb"
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
	rend.SetCSSVersion(web.CSSVersion(staticDir))

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

	// renderError renders the themed error page; auth handlers use it to show
	// sign-in failures (e.g. an error redirect from the IdP) as a real page
	// instead of a bare 502 that Cloudflare would mask as "Bad Gateway".
	renderError := func(w http.ResponseWriter, status int, message string) {
		var buf bytes.Buffer
		if err := rend.Render(&buf, "error", map[string]any{"Title": "Bifrost", "Theme": "light", "Message": message}); err != nil {
			log.Printf("render error page failed: %v", err)
			http.Error(w, message, status)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write(buf.Bytes())
	}

	// Build badges and pipeline links are an optional enhancement: no project
	// configured or no credentials shouldn't keep the rest of the app from
	// starting.
	var builds gcb.Client
	var triggerIDs map[string]string
	if cfg.GCPProject != "" {
		builds, err = gcb.New(context.Background(), cfg.GCPProject)
		if err != nil {
			log.Printf("cloud build client unavailable, build badges disabled: %v", err)
			builds = nil
		}
		// Trigger IDs are static infra; resolve them once so each service card
		// can link to its build pipeline. Keyed by the "{service}-build"
		// convention all triggers follow.
		if names, err := gcb.TriggerIDs(context.Background(), cfg.GCPProject); err != nil {
			log.Printf("cloud build triggers unavailable, pipeline links disabled: %v", err)
		} else {
			triggerIDs = make(map[string]string, len(cfg.Services))
			for _, svc := range cfg.Services {
				if id, ok := names[svc+"-build"]; ok {
					triggerIDs[svc] = id
				}
			}
		}
	}

	sm := auth.NewSessionManager(cfg.SessionSecret, 12*time.Hour)
	authH := &auth.Handlers{OIDC: oidcClient, Session: sm, RenderError: renderError}
	webH := &web.Handlers{Cfg: cfg, Kube: kc, Builds: builds, TriggerIDs: triggerIDs, Renderer: rend}

	requireAuth := auth.RequireAuth(sm, cfg.AllowedEmail, "/auth/login")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /auth/login", authH.Login)
	mux.HandleFunc("GET /auth/callback", authH.Callback)
	mux.HandleFunc("POST /auth/logout", authH.Logout)
	// no-cache = revalidate before reuse (304 when unchanged). Without it,
	// browsers heuristically cache /static/style.css and serve a stale
	// stylesheet after deploys that add new CSS classes.
	staticFiles := http.StripPrefix("/static/", http.FileServer(http.Dir(staticDir)))
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		staticFiles.ServeHTTP(w, r)
	}))
	// Tabs are real routes; each has a polling fragment endpoint that renders
	// just the swappable tab body.
	mux.Handle("GET /", requireAuth(http.HandlerFunc(webH.Overview)))
	mux.Handle("GET /apps", requireAuth(http.HandlerFunc(webH.Apps)))
	mux.Handle("GET /jobs", requireAuth(http.HandlerFunc(webH.Jobs)))
	mux.Handle("GET /partial/overview", requireAuth(http.HandlerFunc(webH.OverviewFragment)))
	mux.Handle("GET /partial/apps", requireAuth(http.HandlerFunc(webH.AppsFragment)))
	mux.Handle("GET /partial/jobs", requireAuth(http.HandlerFunc(webH.JobsFragment)))
	mux.Handle("GET /services/{name}/status", requireAuth(http.HandlerFunc(webH.StatusJSON)))
	mux.Handle("POST /services/{name}/promote", requireAuth(http.HandlerFunc(webH.Promote)))
	mux.Handle("POST /services/{name}/rollback", requireAuth(http.HandlerFunc(webH.Rollback)))

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
