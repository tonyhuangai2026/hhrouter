# Agent Router Platform

A self-hostable **LLM routing gateway** (inspired by new-api). It exposes a single,
unified inbound surface in **both OpenAI and Anthropic Messages formats**, then routes
each request to an OpenAI-compatible or AWS Bedrock upstream — applying multi-dimensional
routing rules, weighted load balancing with failover, per-key + per-user quotas, and full
request logging with an analytics dashboard.

- **Backend** — Go + Gin + GORM (`/server`)
- **Frontend** — React + Vite + Semi Design (`/web`)
  - 界面语言：中/英，自动检测可手动切换 / UI language: Chinese & English, auto-detected and switchable
- **Data** — PostgreSQL 16 (durable) + Redis 7 (quota counters, model cache)
- **Deploy** — `docker compose up -d` (four services, one command)

---

## Architecture

```
                       ┌──────────── Browser (React + Semi) ────────────┐
                       │  Login/Register · Channels · Routing Rules ·    │
                       │  API Keys · Dashboard · Users                   │
                       └───────────────┬─────────────────────────────────┘
                                       │  HTTP :8080  (nginx)
  Downstream clients  ── sk-xxxx ──┐   │  /         → SPA (static dist)
  (OpenAI / Anthropic SDK)         │   │  /api/*    → backend (admin, JWT)
                                   │   │  /v1/*     → backend (relay, sk- key)
                                   ▼   ▼
   ┌──────────────────── frontend (nginx) reverse proxy ────────────────────┐
   └───────────────────────────────┬────────────────────────────────────────┘
                                    │  backend:3000
                ┌───────────────────▼───────────────────────────┐
                │            Go + Gin server (/server)           │
                │  Admin API /api (JWT)   Relay API /v1 (sk-)    │
                │  middleware: auth · quota · log                │
                │                  │                             │
                │            Router Engine                       │
                │     rules → candidates → LB → failover         │
                │          │                  │                  │
                │   OpenAI Adapter      Bedrock Adapter          │
                └──────────┼──────────────────┼──────────────────┘
                           ▼                  ▼
                 OpenAI-compatible      bedrock-runtime.<region>
                 upstreams (/v1/...)    .amazonaws.com/model/<id>/converse
                           │                  │
            ┌──────────────┴──────────────────┴──────────────┐
            ▼                                                 ▼
      PostgreSQL 16  (users, channels, tokens, rules, logs)   Redis 7
                                                         (quota, model cache)
```

Services in `docker-compose.yml`:

| Service    | Image / build         | Role                                              |
|------------|-----------------------|---------------------------------------------------|
| `postgres` | `postgres:16-alpine`  | Durable store (named volume `pgdata`, healthcheck)|
| `redis`    | `redis:7-alpine`      | Quota counters + model cache (healthcheck)        |
| `backend`  | build `./server`      | Go server; waits for db+redis healthy             |
| `frontend` | build `./web`         | nginx: serves SPA, proxies `/api` + `/v1`         |

The frontend container hosts the built SPA and reverse-proxies `/api` and `/v1` to the
backend, so the browser only ever talks to one origin (`http://localhost:8080`).

---

## Quickstart (Docker)

Requires Docker with the Compose plugin (`docker compose`).

```bash
# 1. Configure
cp .env.example .env
#    Edit .env and set strong JWT_SECRET and SECRET_KEY values.

# 2. Build and start everything
docker compose up -d --build

# 3. Wait for health, then verify
docker compose ps                      # all four services should be healthy
curl -fsS http://localhost:8080/api/ping   # {"message":"pong","db":true,"redis":true}

# Open the UI
open http://localhost:8080             # (or just visit it in your browser)
```

Tear down (keep data) / wipe everything:

```bash
docker compose down            # stop containers, keep volumes
docker compose down -v         # also delete pgdata + redisdata (full reset)
```

> **Ports:** the UI/API are published on `HTTP_PORT` (default `8080`). The backend,
> postgres, and redis are internal to the compose network and not published on the host.

---

## End-to-end walkthrough

This is the full happy path, from a fresh deploy to calling the relay with an OpenAI and
an Anthropic client.

### 1. Register the first account (becomes admin)

Open `http://localhost:8080`. On first visit the setup flow detects there are no users
yet and prompts you to register — **the very first account created becomes an `admin`**.
(Alternatively, set `ADMIN_USERNAME`/`ADMIN_PASSWORD` in `.env` before first start to
seed an admin automatically.)

Via API:

```bash
curl -s http://localhost:8080/api/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin12345","display_name":"Admin"}'
# → 201 { "token": "<jwt>", "user": { "role": "admin", ... } }
```

### 2. Log in

The UI logs you in automatically after registration. To log in again later, or via API:

```bash
TOKEN=$(curl -s http://localhost:8080/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin12345"}' | jq -r .token)
```

### 3. Configure a channel (upstream)

In the UI go to **Channels → New**. Pick `type=openai`, set the upstream `base_url`
(e.g. `https://api.openai.com`), paste the upstream API key, and set a `group`
(default `default`). The key is stored AES-256-GCM encrypted and only ever shown masked.

Via API:

```bash
curl -s http://localhost:8080/api/channels \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name":"openai-main",
        "type":"openai",
        "base_url":"https://api.openai.com",
        "key":"sk-REPLACE-WITH-UPSTREAM-KEY",
        "group":"default",
        "models":["gpt-4o-mini"],
        "status":"enabled"
      }'
```

### 4. Fetch models

Click **Auto-fetch models** on the channel (or `POST /api/channels/:id/fetch-models`).
For OpenAI channels this calls the upstream `GET {base_url}/v1/models` and stores the
returned model ids (cached in Redis for 10 minutes). For Bedrock channels a built-in
model id list is offered. You can also edit the model list by hand.

```bash
curl -s -X POST http://localhost:8080/api/channels/1/fetch-models \
  -H "Authorization: Bearer $TOKEN"
```

### 5. (Optional) Create a routing rule

In **Routing Rules → New** you can express *"requests matching these groups / models /
token-size land on these channels"*. With a single channel you can skip this — when no
rule matches, the engine falls back to any enabled channel that can serve the requested
model.

```bash
curl -s http://localhost:8080/api/rules \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name":"default-route",
        "enabled":true,
        "priority":0,
        "match":{"groups":["default"]},
        "target_group":"default"
      }'
```

### 6. Generate an API key

In **API Keys → New**. The full `sk-...` key is shown **once** — copy it now; afterwards
only a masked form is stored/displayed.

```bash
SK=$(curl -s http://localhost:8080/api/tokens \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"my-key","group":"default","quota":-1}' | jq -r .key)
echo "$SK"   # sk-...   (quota -1 = unlimited)
```

### 6b. (Optional) Try a channel in the Playground

Before wiring up a downstream client you can talk to any channel directly from the admin UI.
Open **Channels → Playground** for a **ChatGPT-style chat window**: a multi-turn conversation
with Markdown + code-block rendering, streaming with a typing cursor, per-message **copy** and
**regenerate**, an optional **system prompt**, multi-line input (Enter sends / Shift+Enter
newlines), and image upload/paste for multimodal models. It calls `POST /api/channels/:id/test-chat`
against the one chosen channel — it does **not** consume quota and is **not** keyed by an API
key. Each test-chat is still written to the request log tagged `is_test=true` (with `token_id`
NULL) so it is auditable, yet the **Dashboard** summary/timeseries default to production-only
traffic so test runs never skew your metrics. In the Dashboard **logs** table use the
**type** filter (All / Production / Test) to view test-chat rows; upstream error text is
captured in the log's `error_message`.

### 7. Call the relay

The gateway accepts both inbound formats and routes them to the matching upstream.

**OpenAI client** (`/v1/chat/completions`):

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="sk-...")  # your sk- key
resp = client.chat.completions.create(
    model="gpt-4o-mini",
    messages=[{"role": "user", "content": "Hello!"}],
)
print(resp.choices[0].message.content)
```

or with curl:

```bash
curl -s http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $SK" -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello!"}]}'
```

**Anthropic client** (`/v1/messages`):

```python
import anthropic
client = anthropic.Anthropic(base_url="http://localhost:8080", api_key="sk-...")
msg = client.messages.create(
    model="claude-3-5-sonnet-20240620",
    max_tokens=256,
    messages=[{"role": "user", "content": "Hello!"}],
)
print(msg.content[0].text)
```

or with curl:

```bash
curl -s http://localhost:8080/v1/messages \
  -H "x-api-key: $SK" -H 'anthropic-version: 2023-06-01' -H 'Content-Type: application/json' \
  -d '{"model":"claude-3-5-sonnet-20240620","max_tokens":256,
       "messages":[{"role":"user","content":"Hello!"}]}'
```

Both endpoints support `"stream": true` (SSE). Every call is metered against the key's
and the user's quota and written to the request log; view it under **Dashboard**.

> **Quota note:** relay calls are gated by `min(token quota, user quota)`. A brand-new
> user starts with the `DefaultUserQuota` option (which ships at `0`), so calls return
> HTTP `402 insufficient_quota` until the user is given budget. As admin, raise it under
> **Users** in the UI (or `PUT /api/users/:id` with `{"quota": 1000000}`), and/or bump the
> `DefaultUserQuota` system option. A token `quota` of `-1` means "unlimited for the token"
> but the user-level quota still applies.

---

## Configuration reference

All variables are documented in [`.env.example`](./.env.example). Key ones:

| Variable                          | Required | Default        | Purpose                                            |
|-----------------------------------|----------|----------------|----------------------------------------------------|
| `HTTP_PORT`                       | no       | `8080`         | Host port the UI/API are published on              |
| `POSTGRES_USER/PASSWORD/DB`       | no       | `postgres/.../agent_router` | Database credentials (shared by db + backend) |
| `POSTGRES_SSLMODE`                | no       | `disable`      | Postgres SSL mode                                  |
| `REDIS_PASSWORD` / `REDIS_DB`     | no       | empty / `0`    | Redis auth / db index                              |
| `JWT_SECRET`                      | **yes**  | —              | HS256 secret signing admin JWTs                    |
| `SECRET_KEY`                      | **yes**  | —              | Encrypts stored channel keys (AES-256-GCM)         |
| `GIN_MODE`                        | no       | `release`      | `release` / `debug` / `test`                       |
| `ADMIN_USERNAME` / `ADMIN_PASSWORD` | no     | empty          | If both set, seed an admin on first start          |

> `SECRET_KEY` may be any length: the server derives a 32-byte AES-256 key from it with
> SHA-256. Use a long random value in production. Changing it later makes previously
> stored channel keys undecryptable, so set it once before configuring channels.

Inside the compose network the backend reaches the database at `postgres:5432` and Redis
at `redis:6379` — these hostnames are the compose service names and are wired in
`docker-compose.yml`; you do not set `DB_DSN`/`REDIS_ADDR` yourself.

---

## Local development (without Docker)

You still need PostgreSQL and Redis reachable. The quickest way is to run just those two
via compose and develop the apps natively:

```bash
docker compose up -d postgres redis
```

### Backend (`/server`)

```bash
cd server
export PORT=3000
export POSTGRES_HOST=localhost POSTGRES_PORT=5432 \
       POSTGRES_USER=postgres POSTGRES_PASSWORD=postgres POSTGRES_DB=agent_router
export REDIS_ADDR=localhost:6379
export JWT_SECRET=dev-jwt-secret
export SECRET_KEY=dev-secret-key
export GIN_MODE=debug
go run .          # serves http://localhost:3000  (GET /api/ping → pong)
```

Run the test suite:

```bash
cd server && go build ./... && go test ./...
```

### Frontend (`/web`)

```bash
cd web
npm install
npm run dev       # Vite dev server (default http://localhost:5173)
```

`vite.config.js` proxies `/api` and `/v1` to the backend (`http://localhost:3000`) during
development, so the SPA talks to your local `go run` server. Build the production bundle
with `npm run build` (outputs `web/dist`, which the nginx container serves in production).

---

## Project layout

```
/server            Go + Gin backend (Dockerfile = multi-stage build → alpine)
  main.go          entrypoint: load config → connect DB/Redis → migrate → serve
  config/          env parsing (PORT, POSTGRES_*/DB_DSN, REDIS_ADDR, JWT_SECRET, SECRET_KEY)
  internal/        model, db, middleware, service, router engine, adapters, relay, controllers
/web               React + Vite + Semi Design SPA (Dockerfile = build → nginx)
  nginx.conf       serves dist + reverse-proxies /api and /v1 to backend:3000
docker-compose.yml four services: postgres / redis / backend / frontend
.env.example       all configuration variables with defaults
```

For the full technical design see the project's Tech Design document (Agent Router
Platform MVP) — §2 layout, §10 deployment, §11 non-functional/security.
```
