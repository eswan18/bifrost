# Bifrost

Web UI for staging→prod promotions on the ethans-services GKE cluster. Mirrors the `ib` CLI tool.

## Development

```bash
make css-watch   # in one terminal
make run         # in another
```

Visit http://localhost:8080.

## Deploy

Push to `main`. Cloud Build → Artifact Registry → ArgoCD Image Updater (staging only).
Promote to prod via the app itself (eventually) or `ib promote bifrost`.
