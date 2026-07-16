package web

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eswan18/bifrost/internal/auth"
	"github.com/eswan18/bifrost/internal/config"
	"github.com/eswan18/bifrost/internal/gcb"
	"github.com/eswan18/bifrost/internal/kube"
)

// TestPagesRenderRichScenario exercises every full page against a fleet that
// hits each derivation branch (crash, drift, deploying/degraded, in-sync,
// jobs, builds), and guards against a nil field reference silently rendering
// "<no value>". It's the broad complement to the targeted handler tests.
func TestPagesRenderRichScenario(t *testing.T) {
	orig := nextRun
	nextRun = func(schedule, tz string, after time.Time) (time.Time, error) {
		return time.Now().Add(3 * time.Hour), nil
	}
	defer func() { nextRun = orig }()

	cfg := &config.Config{
		Services:        []string{"api-gateway", "billing-worker", "auth-service", "search-indexer"},
		SessionSecret:   []byte("12345678901234567890123456789012"),
		ArgoCDNamespace: "argocd",
		GitHubOrg:       "acme",
		DisplayLocation: time.UTC,
		StagingURLs:     map[string]string{"api-gateway": "https://api-staging.example"},
		ProdURLs:        map[string]string{"api-gateway": "https://api.example"},
		GCPProject:      "ethans-services",
	}
	rend, err := LoadTemplates("../../templates")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Hour)
	k := &fakeKube{
		pods: map[string][]kube.PodInfo{
			"api-gateway-staging": {{Name: "s", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/api-gateway:b82e4d1", Ready: true}}}},
			"api-gateway-prod": {{Name: "p", Phase: "Running", Containers: []kube.ContainerInfo{
				{Image: "reg/api-gateway:9f3a1c2", Ready: false, WaitingReason: "CrashLoopBackOff", RestartCount: 7},
			}}},
			"billing-worker-staging": {{Name: "s", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/billing-worker:4c7d9e2", Ready: true}}}},
			"billing-worker-prod":    {{Name: "p", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/billing-worker:4c7d9e2", Ready: true}}}},
			// One container not ready → deploying (partial readiness).
			"auth-service-staging": {{Name: "s", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/auth-service:e1f2a3b", Ready: true}, {Image: "reg/auth-service:e1f2a3b", Ready: false}}}},
			"auth-service-prod":    {{Name: "p", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/auth-service:c4d0e88", Ready: true}}}},
			// A failed job pod must NOT drag the env health to degraded.
			"search-indexer-staging": {
				{Name: "s", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/search-indexer:7d2c5f8", Ready: true}}},
				{Name: "reindex-1-pod", Phase: "Failed", OwnerKind: "Job", OwnerName: "reindex-1", Containers: []kube.ContainerInfo{{Image: "reg/search-indexer:7d2c5f8", ExitCode: i32(137)}}},
			},
			"search-indexer-prod": {{Name: "p", Phase: "Running", Containers: []kube.ContainerInfo{{Image: "reg/search-indexer:7d2c5f8", Ready: true}}}},
		},
		rsets: map[string][]kube.ReplicaSetInfo{
			"api-gateway-prod":    {{Revision: 2, Image: "reg/api-gateway:9f3a1c2"}, {Revision: 1, Image: "reg/api-gateway:c8b7a61"}},
			"api-gateway-staging": {{Revision: 2, Image: "reg/api-gateway:b82e4d1"}, {Revision: 1, Image: "reg/api-gateway:7c9d0e1"}},
		},
		cronjobs: map[string][]kube.CronJobInfo{
			"billing-worker-staging": {{Name: "invoice-sync", Schedule: "0 14 * * *", Image: "reg/billing-worker:4c7d9e2"}},
			"search-indexer-staging": {{Name: "reindex-nightly", Schedule: "0 3 * * *", Image: "reg/search-indexer:7d2c5f8"}},
		},
		jobs: map[string][]kube.JobInfo{
			"billing-worker-staging": {{Name: "invoice-sync-9", OwnerCron: "invoice-sync", Image: "reg/billing-worker:4c7d9e2", StartTime: start, CompletionTime: start.Add(72 * time.Second), Succeeded: true}},
			"search-indexer-staging": {{Name: "reindex-1", OwnerCron: "reindex-nightly", Image: "reg/search-indexer:7d2c5f8", StartTime: start, Failed: true, FailReason: "BackoffLimitExceeded"}},
		},
	}
	h := &Handlers{Cfg: cfg, Kube: k, Renderer: rend, Builds: &fakeBuilds{builds: map[string]gcb.BuildStatus{
		"api-gateway":    {Status: "SUCCESS", SHA: "b82e4d1", FinishTime: time.Now().Add(-2 * time.Hour), LogURL: "https://cb/1"},
		"auth-service":   {Status: "WORKING", SHA: "e1f2a3b", StartTime: time.Now().Add(-2 * time.Minute), LogURL: "https://cb/2"},
		"search-indexer": {Status: "FAILURE", SHA: "7d2c5f8", FinishTime: time.Now().Add(-30 * time.Minute), LogURL: "https://cb/3"},
	}}}
	sess := &auth.Session{Email: "me@example.com", ID: "sid1", IssuedAt: time.Now()}

	pages := map[string]func(*httptest.ResponseRecorder){
		"overview": func(w *httptest.ResponseRecorder) { h.Overview(w, authed(t, "GET", "/", "", sess)) },
		"apps":     func(w *httptest.ResponseRecorder) { h.Apps(w, authed(t, "GET", "/apps", "", sess)) },
		"jobs":     func(w *httptest.ResponseRecorder) { h.Jobs(w, authed(t, "GET", "/jobs", "", sess)) },
	}
	for name, fn := range pages {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			fn(rec)
			if rec.Code != 200 {
				t.Fatalf("code = %d", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "<no value>") {
				t.Error("template rendered '<no value>' (a nil field reference)")
			}
		})
	}
}
