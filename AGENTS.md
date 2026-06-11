# AGENTS.md

Self-hosted Qwen Web protocol gateway with OpenAI, Anthropic, and Gemini compatible APIs (Go 1.26 backend + React 19 WebUI).

## Setup commands

- Install deps (backend): `cd backend && go mod download`
- Install deps (frontend): `cd frontend && npm ci`
- Start dev (one-command): `go run start-all.go`  # spawns backend on :7860 and Vite dev server together
- Build (backend): `cd backend && go build -trimpath -ldflags="-s -w" -o ../bin/qwen2api-backend .`
- Build (frontend): `cd frontend && npm run build`
- Test (backend): `cd backend && go test ./...`
- Lint (frontend): `cd frontend && npm run lint`
- Typecheck (frontend): `cd frontend && npm run typecheck`  # runs automatically as part of `npm run build`

## Project layout

- `backend/` — Go 1.26 HTTP server (v2.0 mainline)
  - `backend/api/` — HTTP route handlers (`v1_chat.go`, `anthropic.go`, `gemini.go`, `responses.go`, `images.go`, `videos.go`, `files_api.go`, `embeddings.go`, `models.go`, `probes.go`, `admin.go`)
  - `backend/services/` — business logic (`qwen_client.go`, `account_pool`, `prompt_builder.go`, `tool_parser.go`, `model_catalog.go`, …)
  - `backend/adapter/`, `backend/core/`, `backend/runtime/`, `backend/toolcall/`, `backend/upstream/` — protocol adapters, core types, runtime, tool-call pipeline, upstream integration
  - `backend/main.go` — entry point; `backend/context_pipeline.go` — context/attachment pipeline
- `frontend/` — React 19 + Vite 6 + TypeScript 5.9 + Tailwind 4 WebUI
  - `frontend/src/pages/` — `Dashboard`, `AccountsPage`, `TokensPage`, `SettingsPage`, `TestPage`, `ImagePage`, `VideoPage`
  - `frontend/src/components/ui/` — shadcn-style primitives built on Radix UI
  - `frontend/src/layouts/`, `frontend/src/lib/` — layout shell + utilities
- `start-all.go` — one-command local startup (runs backend + frontend dev server together)
- `Dockerfile`, `docker-compose.yml`, `docker-compose.build.yml` — multi-arch container deployment
- `.github/workflows/docker-publish.yml` — CI: multi-arch (amd64/arm64) image build & push to GHCR + Docker Hub
- `README.md` (English default), `README_CN.md` (Simplified Chinese)

## Code style

- Go: standard `gofmt`; keep packages small and focused; reuse services from `backend/services/` rather than re-implementing.
- Frontend: TypeScript strict mode (`tsconfig.app.json: strict: true`, `noUnusedLocals`, `noUnusedParameters`, `erasableSyntaxOnly`).
- ESLint 9 flat config (`frontend/eslint.config.js`) — extends `@eslint/js` recommended, `typescript-eslint` recommended, `react-hooks` recommended, `react-refresh` (Vite).
- Tailwind 4 via `@tailwindcss/vite`; merge classnames with `tailwind-merge` and `clsx`; use `class-variance-authority` for variant styles.
- Run `cd frontend && npm run lint` and `cd frontend && npm run typecheck` before committing frontend changes.

## Testing instructions

- Backend unit tests: `cd backend && go test ./...` (Go `testing` package; add `*_test.go` next to the package under test).
- Frontend verification: `cd frontend && npm run build` (runs typecheck + production build); add Vitest / RTL tests alongside components when introducing non-trivial behavior.
- E2E / smoke: start the stack with `go run start-all.go` and probe `http://127.0.0.1:7860/healthz` and `/keepalive`.
- All tests and `npm run build` must pass before opening a PR.

## PR & commit conventions

### Branching model (this is a fork)

This repo (`rzrcazk/qwen2API`) is a **personal fork** of `YuJunZhiXue/qwen2API`. The workflow is intentionally non-standard:

- **`main`** — clean mirror of upstream. Pulled from `upstream/main` only. Never commit feature work here.
- **`dev`** — the user's own development line. All second-party features, refactors, experiments land here. Feature branches PR into `dev`.
- **Long-term plan** — when `dev` is considered stable, swap it to become the new `main` (i.e. dev → main promotion, not dev → main merge).
- Until then, `main` and `dev` are **two independent tracks**:
  - `main` always tracks upstream.
  - `dev` accumulates everything the user wants to ship under their own brand.

### Daily mechanics

- Upstream remote: `https://github.com/YuJunZhiXue/qwen2API.git` (configured as `upstream`). Fork remote: `https://github.com/rzrcazk/qwen2API.git` (configured as `origin`).
- Sync `main` with upstream: `git fetch upstream && git checkout main && git merge --ff-only upstream/main` (or `git reset --hard upstream/main` if no local-only commits — there shouldn't be any).
- Start a new feature: `git checkout dev && git pull --ff-only && git checkout -b feat/<short-name>`. PR target is **`dev`**, never `main`, never upstream.
- The user opens the PR manually on GitHub by changing the URL to `https://github.com/rzrcazk/qwen2API/compare/dev...feat/<short-name>` (GitHub defaults the compare-base to upstream when accessed from a fork, so the user has to switch it).

### Hard rules

- **Never reuse a branch that has already been merged via PR.** If you need follow-up commits on top of merged work, branch from the latest `origin/dev` and cherry-pick — don't force-push onto the merged branch.
- Commit message: conventional commits (`feat:` / `fix:` / `docs:` / `refactor:` / `chore:` / `test:` / `build:` / `ci:`).
- Open PR via `gh pr create` once CI is green. CI publishes Docker images on push to `main` and on `v*.*.*` tags — Docker-related changes must keep `Dockerfile`, `docker-compose.yml`, and `README.md` consistent.
- Keep Docker data paths container-internal as `/app/data` and `/app/logs`; control host paths through compose volume mappings, not hard-coded workspace paths.

## Security

- Never commit secrets: `.env`, real `accounts.json` / `api_keys.json`, Qwen tokens, cookies, passwords, or downstream API keys are git-ignored and must stay that way.
- Runtime-only secrets are injected via env (`ADMIN_KEY`, `QWEN_API_KEY`, `QWEN_ACCOUNT_N`, …) and are not persisted to disk.
- `ADMIN_KEY` gates the WebUI and `/api/admin/*`; downstream `QWEN_API_KEY[_N]` gate the public OpenAI / Anthropic / Gemini routes. Always validate keys at request boundary.
- If you find a security issue, follow the disclaimer in `README.md` — private disclosure first, no public secret leaks in issues or PRs.
- License: GPL-3.0 — any new dependency must be GPL-compatible.
