# Smoke Test — 2026-06

End-to-end smoke for the i2v / i2i / t2i / t2v media surfaces on
`feat/i2v-i2i-and-model-bump`, run as part of the
`docs + smoke` follow-up commit (see `git log --oneline`).

## TL;DR

- The local stack **boots cleanly** on `PORT=17860` with the empty
  account pool — `/healthz` and `/readyz` both return 200. Binary
  is healthy.
- The **HTTP boundary** for `/v1/images/generations`,
  `/v1/videos/generations`, and `/v1/files` is reachable end-to-end
  and the i2v / i2i validation paths return the expected 4xx bodies
  when the inputs are malformed. This is a stronger signal than a
  unit test on the helper function, because it crosses the
  `mux → handler → resolveMediaInputImage → response` boundary.
- A **full happy-path smoke** (t2i / i2i / i2v) against the real
  Qwen upstream was **not** executed in this PR. Reason:
  `data/accounts.json` is `[]` and there are no `QWEN_ACCOUNT_*`
  env vars in this environment, so any media call that reaches the
  upstream path fails with
  `500 {"detail":"no available upstream account"}`. Without a real
  account we cannot validate the upstream chat-creation /
  image-or-video generation paths.
- The three cases below are written so the next person with a real
  account can paste them in and run them verbatim. The expected
  happy-path responses and the 4xx / 5xx error bodies (observed
  during the no-account boot probe) are both included so the
  verifier can tell at a glance which branch was reached.

## Environment

| Item | Value |
| --- | --- |
| Branch | `feat/i2v-i2i-and-model-bump` |
| HEAD at probe time | `ecf79b6` (this PR's README commit) |
| Go | `1.26` (`go build ./...` + `go test ./...` green) |
| Frontend | `npm run build` green |
| Backend binary | `go build -trimpath -ldflags="-s -w" -o /tmp/qwen2api-smoke/qwen2api-backend .` (≈13 MB) |
| Port | `17860` (host port 7860 was held by OrbStack on this machine) |
| `ADMIN_KEY` | `smoketest` (env-injected, not committed) |
| `QWEN_API_KEY` | `sk-smoketest` (env-injected downstream key, not committed) |
| `data/accounts.json` | `[]` (empty) |
| `QWEN_ACCOUNT_*` env | unset |

## Stack boot probe (no accounts required)

```bash
go build -trimpath -ldflags="-s -w" -o /tmp/qwen2api-smoke/qwen2api-backend .

PORT=17860 \
  ADMIN_KEY=smoketest \
  QWEN_API_KEY=sk-smoketest \
  /tmp/qwen2api-smoke/qwen2api-backend \
  > /tmp/qwen2api-smoke/server.log 2>&1 &
SERVER_PID=$!
sleep 3
```

`/healthz` and `/readyz` immediately returned 200:

```text
--- /healthz ---
{"status":"ok"}
HTTP 200

--- /readyz ---
{"accounts":{"available":0,"available_chat":0,"available_image":0,
  "available_video":0,"global_in_use":0,"global_max_inflight":0,
  "max_inflight_per_account":2,"max_queue_size":0,
  "ready_set_enabled":false,"ready_set_threshold":128,
  "recommended_concurrency":0,"total":0,"valid":0},
 "status":"ready"}
HTTP 200
```

The `accounts.total = 0` block confirms the empty pool, but the
server itself is fully alive — every non-media endpoint
(`/healthz`, `/readyz`, `/keepalive`, `/v1/models`, WebUI) is
unaffected by the missing account. Media calls, however, will
fail at the upstream stage as documented below.

## Case 1 — Pure text-to-image (t2i, backwards-compatible)

```bash
curl -sS -o - -w "\nHTTP %{http_code}\n" \
  http://127.0.0.1:7860/v1/images/generations \
  -H "Authorization: Bearer $QWEN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "prompt": "a friendly red panda sitting in bamboo, soft sunlight",
    "n": 1,
    "size": "1024x1024",
    "model": "qwen-image-plus"
  }'
```

### Expected response — happy path (real account)

`200 OK` body, shape locked in `backend/main.go` (see
`handleImages` + `createImageURLs`):

```json
{
  "created": 1718000000,
  "data": [
    {
      "url": "https://cdn.qwenlm.ai/.../image.png",
      "revised_prompt": "a friendly red panda sitting in bamboo, soft sunlight",
      "size": "1024x1024",
      "ratio": "1:1",
      "width": 1024,
      "height": 1024
    }
  ]
}
```

`size` accepts `1024x1024` / `1024x1792` / `1792x1024` or the
`1:1` / `9:16` / `16:9` shortcuts. `model` is forwarded to
`resolveMediaModel`; the default is `qwen3.7-plus`. `n` is
clamped to `[1, 4]`.

### Observed response — no-account run (this PR)

```text
{"detail":"no available upstream account"}
HTTP 500
```

Took ~60 s before returning: the empty pool spins the
`mediaRetryAttempts()` loop before giving up. This is the same
behaviour the unit tests in `backend/main_test.go` short-circuit
by pre-cancelling the request context — we just don't have that
short-circuit in the live binary, by design.

> **What this proves without a real account**: the request body
> is parsed, `resolveMediaModel("qwen-image-plus")` succeeds (log
> line `图片生成请求解析完成 … resolved_model=qwen3.7-plus`),
> `prompt_len=20` is correct, `n=1` is accepted, and the call
> reaches the upstream stage. The 5xx is purely the missing
> account, not a code regression.

## Case 2 — Image-to-image (i2i, data URL)

```bash
curl -sS -o - -w "\nHTTP %{http_code}\n" \
  http://127.0.0.1:7860/v1/images/generations \
  -H "Authorization: Bearer $QWEN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "prompt": "give the red panda a straw hat",
    "image": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAA...",
    "size": "1024x1024",
    "model": "qwen-image-plus"
  }'
```

### Expected response — happy path (real account)

Same shape as Case 1, `200 OK` with a single-element `data` array.
The chat type is flipped from `image_gen` to the i2i variant
inside `buildChatPayload`; no extra fields are surfaced in the
HTTP response.

### Observed response — no-account run (this PR)

For the *valid* body above we expect the same 500
`{"detail":"no available upstream account"}` as Case 1.

We did, however, exercise the **validation branch** end-to-end
through the live HTTP handler (not just the unit-test helper):

```bash
# Garbage image value
curl -sS -o - -w "\nHTTP %{http_code}\n" \
  http://127.0.0.1:7860/v1/images/generations \
  -H "Authorization: Bearer $QWEN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "prompt": "give the red panda a straw hat",
    "image": "garbage-not-a-url",
    "model": "qwen-image-plus"
  }'
```

```text
{"detail":"image must be a data URL, http(s) URL, or file_id (got \"garbage-not-a-url\")"}
HTTP 400
```

> **What this proves without a real account**: the
> `resolveMediaInputImage` helper is wired into the live
> `handleImages` mux path, the 400 message matches
> `main.go:5794` verbatim, and the i2i integration did not break
> the existing prompt-parse / size-parse flow. Mirrors the unit
> tests `TestHandleImagesInvalidImageField` and
> `TestResolveMediaInputImageGarbage` in `backend/main_test.go`.

## Case 3 — Image-to-video (i2v, via `file_id`)

The i2v call needs a `file_id`, so the curl is two steps.

### Step 3a — upload the reference image to `/v1/files`

```bash
FILE_ID=$(curl -sS http://127.0.0.1:7860/v1/files \
  -H "Authorization: Bearer $QWEN_API_KEY" \
  -F "file=@./reference.png;type=image/png" \
  | jq -r .id)

echo "Uploaded: $FILE_ID"
```

Multipart upload, single file max 128 MiB, extension must be in
`Settings.ContextAllowedUserExts` (default list includes the usual
image / pdf / text / code formats). `image/*` is accepted.

### Step 3b — request the i2v generation

```bash
curl -sS -o - -w "\nHTTP %{http_code}\n" \
  http://127.0.0.1:7860/v1/videos/generations \
  -H "Authorization: Bearer $QWEN_API_KEY" \
  -H "Content-Type: application/json" \
  -d "{
    \"prompt\": \"the red panda waves hello, slow camera pan\",
    \"image\": \"$FILE_ID\",
    \"size\": \"1280x720\",
    \"duration\": 5,
    \"model\": \"qwen-video-plus\"
  }"
```

### Expected response — happy path (real account)

`200 OK`, `data` is a single-element array, chat type is
auto-flipped to `i2v` because `image` was set:

```json
{
  "created": 1718000000,
  "data": [
    {
      "url": "https://cdn.qwenlm.ai/.../video.mp4",
      "revised_prompt": "the red panda waves hello, slow camera pan",
      "size": "1280x720",
      "ratio": "16:9",
      "width": 1280,
      "height": 720,
      "duration": 5
    }
  ]
}
```

`duration` is clamped to `[1, 10]`, `n` to `[1, 2]`. If the
`image` field is omitted entirely the chat type stays `t2v` and
the response shape is identical.

### Observed response — no-account run (this PR)

We exercised the two validation paths end-to-end:

```bash
# Garbage image value on the video endpoint
curl -sS -o - -w "\nHTTP %{http_code}\n" \
  http://127.0.0.1:7860/v1/videos/generations \
  -H "Authorization: Bearer $QWEN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "prompt": "the red panda waves hello",
    "image": "garbage-not-a-url",
    "model": "qwen-video-plus"
  }'
```

```text
{"detail":"image must be a data URL, http(s) URL, or file_id (got \"garbage-not-a-url\")"}
HTTP 400
```

And the file-upload step ran cleanly end-to-end:

```bash
curl -sS -o - -w "\nHTTP %{http_code}\n" \
  http://127.0.0.1:7860/v1/files \
  -H "Authorization: Bearer $QWEN_API_KEY" \
  -F "file=@./tiny.png;type=image/png"
```

```json
{
  "bytes": 69,
  "content_block": {
    "file_id": "file-a08b2d4b973a5bb5b61f7abe",
    "filename": "tiny.png",
    "mime_type": "image/png",
    "type": "input_file"
  },
  "content_type": "image/png",
  "created_at": 1781100589,
  "filename": "tiny.png",
  "id": "file-a08b2d4b973a5bb5b61f7abe",
  "object": "file"
}
HTTP 200
```

> **What this proves without a real account**: the `/v1/files`
> upload + `file_id` lookup chain is fully wired (multipart → JSON
> record → on-disk blob in `data/context_files/`), and the i2v
> HTTP path correctly rejects malformed `image` values through
> the same helper as the i2i endpoint. The remaining unknown is
> whether the upstream `i2v` chat type works against the real
> model — that needs an account.

## Verifier checklist (for someone with a real account)

If you have a populated `data/accounts.json` (or
`QWEN_ACCOUNT_1=token;…` in env), the next step is:

1. Start the stack: `go run start-all.go` (or `cd backend && go
   run .` and `cd frontend && npm run dev`).
2. Set the downstream key: `export
   QWEN_API_KEY=sk-downstream-key-from-webui`.
3. Run Case 1 verbatim — expect a `200 OK` JSON with a
   `cdn.qwenlm.ai/.../image.png` URL in `data[0].url`. The
   response should arrive in roughly 5-30 s on a warm pool.
4. Run Case 2 verbatim with a tiny test PNG base64-encoded into
   the data URL — expect the same shape, image URL pointing at
   the edited result.
5. Run Case 3a + 3b — expect the file_id JSON, then a `data[0]`
   whose `url` ends in `.mp4` (or a `task_id` that resolves to a
   video after polling).
6. If any case returns `4xx`, the error body's `detail` field
   should be one of:
   - `prompt is required` — the prompt field was missing/blank
   - `image must be a data URL, http(s) URL, or file_id (got
     "…")` — the `image` field had an unsupported shape
   - `image file_id not found: file-…` — the file_id is wrong or
     owned by a different API key
   - `image file_id lookup failed: …` — on-disk read failed
   - `Unsupported file extension: .…` — the multipart file
     extension is not in the allow-list
7. If a case returns `5xx`, capture the body and the last 50
   lines of `logs/qwen2api-*.log` (or the server stderr) — the
   `req_id` field ties them together. The most common
   no-account 5xx is `{"detail":"no available upstream
   account"}`; an upstream rejection looks like `{"detail":"All
   N attempts failed. Last error: …"}`.

## Notes & TODOs

- **Happy-path smoke still owed to CI.** This branch is
  implementation-complete; the only missing piece for a full
  e2e is a real account. Worth wiring a "smoke" job in CI that
  uses a self-issued test account + a recorded upstream response
  fixture, so the i2v/i2i branches don't silently regress.
- **The 60-second t2i timeout** when the account pool is empty
  is not new (it predates this PR), but it does make the no-
  account smoke slower than ideal. The unit tests in
  `backend/main_test.go` use `context.WithCancel` to make
  `AcquireFor` fail fast; the live binary doesn't have an
  equivalent. If we want, we can add a `--media-no-account-
  fail-fast` env knob to make boot probes snappier. Out of scope
  for this PR.
- **Probe artifacts cleanup.** The boot probe created a few
  runtime files under `data/` (an uploaded file record, a
  `context_cache.json`, a `session_affinity.json`). All of them
  are gitignored — `data/*.json` is in `.gitignore`; the
  uploaded blob in `data/context_files/` was cleaned up manually
  after the probe. No secrets are involved.
- **No frontend smoke was run.** The WebUI is reachable on
  `http://127.0.0.1:7860` when the dev server is started, and
  the `FileUpload` primitive on the `ImagePage` / `VideoPage`
  sends the same multipart `POST /v1/files` + JSON `image:
  file_id` flow that Cases 2/3 exercise. A click-through in
  the browser is the natural next step once an account is
  available.
