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
	"github.com/eswan18/bifrost/internal/github"
	"github.com/eswan18/bifrost/internal/kube"
	"github.com/eswan18/bifrost/internal/neon"
	"github.com/eswan18/bifrost/internal/preview"
	"github.com/eswan18/bifrost/internal/registry"
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

	// registry.yaml is embedded, so a parse failure here is a build-time
	// invariant violation (a bad commit to the registry itself), not a
	// runtime/environment condition — fail loudly rather than silently
	// disabling the preview control plane. Loaded unconditionally (not just
	// when GCPProject/preview deps are present) since it's also the source
	// of which services are previewable at all, used below.
	fleet, err := registry.Load()
	if err != nil {
		log.Fatalf("registry: %v", err)
	}
	// reg narrows the fleet-wide registry down to the previewable subset —
	// the shape the preview control plane below expects. Other fleet-wide
	// consumers of the full registry (build badges, service cards, ...) are
	// wired up right here in this file, off `fleet` directly.
	reg := preview.FromFleet(fleet)

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
	// previewTriggerIDs feeds the preview orchestrator: keyed by the FULL
	// "{service}-preview-build" trigger name (matching
	// preview.Orchestrator.TriggerIDs' own lookup key), not the bare service
	// name triggerIDs above uses. Both are resolved from the same trigger
	// list, just filtered differently.
	var previewTriggerIDs map[string]string
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
			triggerIDs = make(map[string]string, len(fleet))
			for _, svc := range fleet.Names() {
				if id, ok := names[svc+"-build"]; ok {
					triggerIDs[svc] = id
				}
			}
			previewTriggerIDs = make(map[string]string, len(reg))
			for _, svc := range reg.Names() {
				if id, ok := names[svc+"-preview-build"]; ok {
					previewTriggerIDs[svc+"-preview-build"] = id
				}
			}
		}
	}

	// GitHub/Neon clients back the preview orchestrator; each is optional
	// per the "empty disables" contract already used for GCPProject above —
	// an empty token means that dependency (and so the whole orchestrator)
	// is off, not misconfigured.
	var ghClient github.Client
	if cfg.GitHubToken != "" {
		ghClient = github.New(cfg.GitHubOrg, cfg.GitHubToken)
	}
	var neonClient neon.Client
	if cfg.NeonAPIKey != "" {
		neonClient = neon.New(cfg.NeonAPIKey)
	}

	sm := auth.NewSessionManager(cfg.SessionSecret, 12*time.Hour)
	authH := &auth.Handlers{OIDC: oidcClient, Session: sm, RenderError: renderError}
	webH := &web.Handlers{Cfg: cfg, Registry: fleet, Kube: kc, Builds: builds, TriggerIDs: triggerIDs, Renderer: rend}

	// The preview orchestrator needs every one of its dependencies present —
	// partial config (e.g. a GitHub token but no Neon key, or an empty
	// registry) leaves webH.Orch nil, and the mutating preview endpoints
	// answer 503 rather than running a half-wired flow.
	if kc != nil && builds != nil && ghClient != nil && neonClient != nil && len(reg) > 0 {
		webH.Orch = &preview.Orchestrator{
			Cfg:        cfg,
			Kube:       kc,
			GitHub:     ghClient,
			Neon:       neonClient,
			Builds:     builds,
			TriggerIDs: previewTriggerIDs,
			Registry:   reg,
			Fleet:      fleet,
		}
	}

	requireAuth := auth.RequireAuth(sm, cfg.AllowedEmail, "/auth/login")
	requirePreviewAuth := auth.RequireSessionOrBearer(sm, cfg.AllowedEmail, "/auth/login", cfg.PreviewAPIToken)

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
	mux.Handle("GET /previews", requireAuth(http.HandlerFunc(webH.Previews)))
	mux.Handle("GET /partial/overview", requireAuth(http.HandlerFunc(webH.OverviewFragment)))
	mux.Handle("GET /partial/apps", requireAuth(http.HandlerFunc(webH.AppsFragment)))
	mux.Handle("GET /partial/jobs", requireAuth(http.HandlerFunc(webH.JobsFragment)))
	mux.Handle("GET /partial/previews", requireAuth(http.HandlerFunc(webH.PreviewsFragment)))
	mux.Handle("GET /services/{name}/status", requireAuth(http.HandlerFunc(webH.StatusJSON)))
	mux.Handle("POST /services/{name}/promote", requireAuth(http.HandlerFunc(webH.Promote)))
	mux.Handle("POST /services/{name}/rollback", requireAuth(http.HandlerFunc(webH.Rollback)))
	mux.Handle("GET /api/previews", requirePreviewAuth(http.HandlerFunc(webH.PreviewsListJSON)))
	mux.Handle("GET /api/previews/{tag}", requirePreviewAuth(http.HandlerFunc(webH.PreviewJSON)))
	mux.Handle("POST /api/previews", requirePreviewAuth(http.HandlerFunc(webH.CreatePreviewJSON)))
	mux.Handle("DELETE /api/previews/{tag}", requirePreviewAuth(http.HandlerFunc(webH.DeletePreviewJSON)))

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
