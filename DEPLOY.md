# Deploying webshooter (Render.com)

Render's free tier needs **no credit card**. It builds from the `Dockerfile` and
serves over HTTPS with WebSocket support. The free plan is single-instance,
which suits this game (state lives in memory in one process — never run more
than one instance).

> Free services **sleep after ~15 min idle** and cold-start (~30–60s) on the
> next request. They stay awake while players are connected.

## One-time setup

1. Push this repo to GitHub (or GitLab).
2. Go to <https://dashboard.render.com> and sign up (GitHub/Google/email — no card).
3. **New → Blueprint**, pick this repo. Render reads `render.yaml` and creates
   the `webshooter` web service (Docker runtime, free plan).
4. Click **Apply / Deploy**. First build takes a few minutes.

Your URL will be `https://<service-name>.onrender.com`.

## Lock down the WebSocket origin

After the first deploy, once you know the URL, set the origin allowlist so only
your own site can open game connections:

- Render dashboard → the service → **Environment** → add:
  - `ALLOWED_ORIGIN = https://<service-name>.onrender.com`
- Save — Render redeploys automatically.

Leaving `ALLOWED_ORIGIN` unset allows any origin (fine for local dev, not prod).

## Updates

`autoDeploy` is on, so pushing to the connected branch redeploys automatically.

## Local dev (unchanged)

```bash
go run .
# http://localhost:8080  (PORT defaults to 8080; Render injects its own $PORT)
```
