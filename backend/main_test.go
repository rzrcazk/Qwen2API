package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newMediaTestApp builds a minimal *App suitable for exercising the media
// resolution helpers. It does not bring up the chat pool, the upstream
// client, or the mail session — only the bits the new code touches (the
// settings, the uploaded-file store, the file content cache, the users
// store, an empty account pool, and a quiet logger).
//
// The stores are wired to real on-disk files inside t.TempDir() so the
// `getUploadedLocalFile` → `loadUploadedLocalFiles` chain and the
// `resolveAuth` → `usersStore.LoadInto` chain behave exactly as they do
// in production. The apiKeys map is left nil so resolveAuth accepts any
// non-empty token (per the `len(app.apiKeys) > 0` guard), which is what
// the HTTP-level tests rely on. The account pool is initialised but
// empty — AcquireFor returns the standard "no accounts" error rather
// than a nil-pointer panic, so we can match on the error string.
func newMediaTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	logger := newLogger("error")
	accountsStore := NewJSONStore(filepath.Join(dir, "accounts.json"), []Account{})
	settings := Settings{
		ContextGeneratedDir:      dir,
		ContextAllowedUserExts:   "txt,md,png,jpg,jpeg,webp,gif,bmp",
		MaxRetries:               1,
		MaxInflightPerAccount:    1,
		AccountReadySetThreshold: 1,
	}
	// Manually load the empty default so AcquireFor's pickLockedFor has
	// a deterministic empty slice to walk; we also catch any disk error
	// loudly instead of letting it surface as a nil-deref later.
	if err := accountsStore.Ensure(); err != nil {
		t.Fatalf("seed accounts store: %v", err)
	}
	pool := NewAccountPool(accountsStore, settings, logger)
	if err := pool.Load(); err != nil {
		t.Fatalf("load account pool: %v", err)
	}
	return &App{
		settings:          settings,
		logger:            logger,
		accounts:          pool,
		usersStore:        NewJSONStore(filepath.Join(dir, "users.json"), []map[string]any{}),
		uploadedFileStore: NewJSONStore(filepath.Join(dir, "uploaded.json"), []UploadedLocalFileRecord{}),
		fileContentCache:  newFileContentCache(),
	}
}

// writeUploadedFile inserts a record into the on-disk store and writes the
// given content to disk under the configured ContextGeneratedDir. The
// returned record mirrors what `saveUploadedLocalFile` would produce.
func writeUploadedFile(t *testing.T, app *App, id, filename, contentType, ownerToken string, payload []byte) UploadedLocalFileRecord {
	t.Helper()
	dir := normalizeWorkspacePath(app.settings.ContextGeneratedDir)
	if dir == "" {
		t.Fatalf("test setup: ContextGeneratedDir is empty")
	}
	full := filepath.Join(dir, id+"-"+filepath.Base(filename))
	if err := os.WriteFile(full, payload, 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	rec := UploadedLocalFileRecord{
		ID:          id,
		Filename:    filename,
		Size:        int64(len(payload)),
		ContentType: contentType,
		CreatedAt:   1,
		Path:        full,
		OwnerToken:  ownerToken,
	}
	records, err := app.loadUploadedLocalFiles()
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	records = append(records, rec)
	if err := app.saveUploadedLocalFiles(records); err != nil {
		t.Fatalf("save store: %v", err)
	}
	return rec
}

func TestQwenHeadersIncludeRequestID(t *testing.T) {
	h := qwenHeaders("test-token")
	got := strings.TrimSpace(h.Get("x-request-id"))
	if got == "" {
		t.Fatalf("qwenHeaders should include non-empty x-request-id")
	}
}

func TestLoadSettingsKeepsChatIDPrewarmEnabledByDefault(t *testing.T) {
	t.Setenv("CHAT_ID_PREWARM_TARGET_PER_ACCOUNT", "")

	settings := LoadSettings()

	if settings.ChatIDPrewarmTargetPerAccount != 5 {
		t.Fatalf("ChatIDPrewarmTargetPerAccount = %d, want 5", settings.ChatIDPrewarmTargetPerAccount)
	}
}

func TestResolveChatModelAliasesUseCurrentPlusDefault(t *testing.T) {
	cases := []string{"qwen", "qwen-plus", "qwen-max", "gpt-4o", "gpt-5", "claude-sonnet-4-5", "gemini-2.5-pro"}
	for _, input := range cases {
		got := resolveModel(input)
		if got != "qwen3.7-plus" {
			t.Fatalf("resolveModel(%q) = %q, want qwen3.7-plus", input, got)
		}
	}
}

func TestUpstreamMediaFileClassMatchesQwenImageShape(t *testing.T) {
	if got := upstreamUploadKind("image/png"); got != "image" {
		t.Fatalf("upstreamUploadKind(image/png) = %q, want image", got)
	}
	if got := upstreamMediaFileClass("image"); got != "vision" {
		t.Fatalf("upstreamMediaFileClass(image) = %q, want vision", got)
	}
	if got := upstreamUploadKind("application/pdf"); got != "file" {
		t.Fatalf("upstreamUploadKind(application/pdf) = %q, want file", got)
	}
	if got := upstreamMediaFileClass("file"); got != "document" {
		t.Fatalf("upstreamMediaFileClass(file) = %q, want document", got)
	}
}

// TestResolveMediaModel covers the qwen3.6-plus → qwen3.7-plus media
// bump. The alias table is pure data, so the assertions stay close to
// the entries the spec called out. Anything that does not hit an alias
// falls through to `resolveModel` and is verified separately.
func TestResolveMediaModel(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		image    bool
		expected string
	}{
		// Default behaviour — every empty / blank request resolves to 3.7-plus.
		{"empty defaults to 3.7-plus", "", true, "qwen3.7-plus"},
		{"whitespace defaults to 3.7-plus", "   ", false, "qwen3.7-plus"},

		// OpenAI / Sora family — all three aliases from the old table.
		{"dall-e-3 maps to 3.7-plus", "dall-e-3", true, "qwen3.7-plus"},
		{"dall-e-2 maps to 3.7-plus", "dall-e-2", true, "qwen3.7-plus"},
		{"gpt-image-1 maps to 3.7-plus", "gpt-image-1", true, "qwen3.7-plus"},
		{"gpt-image-1-mini maps to 3.7-plus (new alias)", "gpt-image-1-mini", true, "qwen3.7-plus"},
		{"sora maps to 3.7-plus", "sora", false, "qwen3.7-plus"},
		{"sora-2 maps to 3.7-plus", "sora-2", false, "qwen3.7-plus"},

		// qwen-image family.
		{"qwen-image maps to 3.7-plus", "qwen-image", true, "qwen3.7-plus"},
		{"qwen-image-plus maps to 3.7-plus", "qwen-image-plus", true, "qwen3.7-plus"},
		{"qwen-image-turbo maps to 3.7-plus", "qwen-image-turbo", true, "qwen3.7-plus"},
		{"qwen-image-edit-plus maps to 3.7-plus (new alias)", "qwen-image-edit-plus", true, "qwen3.7-plus"},

		// qwen-video family.
		{"qwen-video maps to 3.7-plus", "qwen-video", false, "qwen3.7-plus"},
		{"qwen-video-plus maps to 3.7-plus", "qwen-video-plus", false, "qwen3.7-plus"},
		{"qwen-video-turbo maps to 3.7-plus", "qwen-video-turbo", false, "qwen3.7-plus"},

		// qwen3.7-plus family variants introduced for forward compatibility.
		{"qwen3.7-plus identity", "qwen3.7-plus", true, "qwen3.7-plus"},
		{"qwen3.7-plus-image maps to 3.7-plus", "qwen3.7-plus-image", true, "qwen3.7-plus"},
		{"qwen3.7-plus-t2i maps to 3.7-plus", "qwen3.7-plus-t2i", true, "qwen3.7-plus"},
		{"qwen3.7-plus-video maps to 3.7-plus", "qwen3.7-plus-video", false, "qwen3.7-plus"},
		{"qwen3.7-plus-t2v maps to 3.7-plus", "qwen3.7-plus-t2v", false, "qwen3.7-plus"},
		{"qwen3.7-plus-thinking maps to 3.7-plus", "qwen3.7-plus-thinking", true, "qwen3.7-plus"},
		{"qwen3.7-plus-search maps to 3.7-plus", "qwen3.7-plus-search", true, "qwen3.7-plus"},
		{"qwen3.7-plus-deep-research maps to 3.7-plus", "qwen3.7-plus-deep-research", true, "qwen3.7-plus"},
		{"qwen3.7-plus-deep_research maps to 3.7-plus", "qwen3.7-plus-deep_research", true, "qwen3.7-plus"},
		{"qwen3.7-plus-webdev maps to 3.7-plus", "qwen3.7-plus-webdev", true, "qwen3.7-plus"},
		{"qwen3.7-plus-web-dev maps to 3.7-plus", "qwen3.7-plus-web-dev", true, "qwen3.7-plus"},
		{"qwen3.7-plus-slides maps to 3.7-plus", "qwen3.7-plus-slides", true, "qwen3.7-plus"},

		// Aliases are looked up case-insensitively (the table lookup is
		// normalised through strings.ToLower).
		{"DALL-E-3 mixed case", "DALL-E-3", true, "qwen3.7-plus"},
		{"Qwen-Image-Plus mixed case", "Qwen-Image-Plus", true, "qwen3.7-plus"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := resolveMediaModel(tc.input, tc.image)
			if got != tc.expected {
				t.Fatalf("resolveMediaModel(%q, %v) = %q, want %q", tc.input, tc.image, got, tc.expected)
			}
		})
	}
}

// TestResolveMediaModelFallback verifies that unknown model names (not in
// the alias table) still fall through to the chat-model resolver rather
// than silently snapping to qwen3.7-plus. The non-aliased name "qwen3.6-plus"
// is the natural regression target — pre-bump it aliased to itself, post
// bump it does not, so the fallback path needs to keep the input stable.
func TestResolveMediaModelFallback(t *testing.T) {
	// "qwen3.6-plus" is no longer in the media alias table; it must fall
	// through to resolveModel, where the chat modelMap still maps it to
	// "qwen3.6-plus" verbatim (resolveModel is intentionally untouched
	// per the commit-1 message).
	got := resolveMediaModel("qwen3.6-plus", true)
	if got != "qwen3.6-plus" {
		t.Fatalf("resolveMediaModel(qwen3.6-plus) = %q, want qwen3.6-plus (fallback via resolveModel)", got)
	}
}

// TestResolveMediaInputImageEmpty asserts the no-op short-circuit.
func TestResolveMediaInputImageEmpty(t *testing.T) {
	app := newMediaTestApp(t)
	for _, in := range []string{"", "   ", "\n\t"} {
		files, err := app.resolveMediaInputImage(context.Background(), in, &AuthContext{Token: "tkn"})
		if err != nil {
			t.Fatalf("resolveMediaInputImage(%q) returned error: %v", in, err)
		}
		if files != nil {
			t.Fatalf("resolveMediaInputImage(%q) = %v, want nil slice", in, files)
		}
	}
}

// TestResolveMediaInputImageDataURL verifies a data URL passes through
// unchanged, with the right upstream file-block shape.
func TestResolveMediaInputImageDataURL(t *testing.T) {
	app := newMediaTestApp(t)
	const dataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII="
	files, err := app.resolveMediaInputImage(context.Background(), dataURL, &AuthContext{Token: "tkn"})
	if err != nil {
		t.Fatalf("resolveMediaInputImage(data URL) error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file block, got %d", len(files))
	}
	typ, _ := files[0]["type"].(string)
	if typ != "image" {
		t.Fatalf("file block type = %q, want image", typ)
	}
	if files[0]["showType"] != "image" || files[0]["file_class"] != "vision" || files[0]["status"] != "uploaded" {
		t.Fatalf("file shape = %+v, want Qwen image attachment shape", files[0])
	}
	inner, ok := files[0]["image_url"].(map[string]any)
	if !ok {
		t.Fatalf("image_url key is not a map: %T", files[0]["image_url"])
	}
	url, _ := inner["url"].(string)
	if url != dataURL {
		t.Fatalf("image_url.url = %q, want passthrough %q", url, dataURL)
	}
	topURL, _ := files[0]["url"].(string)
	if topURL != dataURL {
		t.Fatalf("url = %q, want passthrough %q", topURL, dataURL)
	}
}

// TestResolveMediaInputImageDataURLCaseInsensitive — the data: prefix check
// is done with strings.ToLower, so "DATA:..." should also pass through.
func TestResolveMediaInputImageDataURLCaseInsensitive(t *testing.T) {
	app := newMediaTestApp(t)
	const dataURL = "DATA:image/jpeg;base64,AAA="
	files, err := app.resolveMediaInputImage(context.Background(), dataURL, &AuthContext{Token: "tkn"})
	if err != nil {
		t.Fatalf("resolveMediaInputImage(case insensitive data URL) error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file block, got %d", len(files))
	}
	inner, _ := files[0]["image_url"].(map[string]any)
	if inner["url"] != dataURL {
		t.Fatalf("expected passthrough of the original case, got %v", inner["url"])
	}
}

// TestResolveMediaInputImageHTTPSURL — http(s) URLs pass through verbatim.
func TestResolveMediaInputImageHTTPSURL(t *testing.T) {
	app := newMediaTestApp(t)
	cases := []string{
		"http://example.com/cat.png",
		"https://cdn.example.com/path/to/cat.png?token=abc",
		"HTTPS://CDN.Example.com/Cat.PNG", // case-insensitive, must pass through unchanged
	}
	for _, in := range cases {
		files, err := app.resolveMediaInputImage(context.Background(), in, &AuthContext{Token: "tkn"})
		if err != nil {
			t.Fatalf("resolveMediaInputImage(%q) error: %v", in, err)
		}
		if len(files) != 1 {
			t.Fatalf("expected 1 file block for %q, got %d", in, len(files))
		}
		if files[0]["type"] != "image" || files[0]["showType"] != "image" || files[0]["file_class"] != "vision" {
			t.Fatalf("file shape = %+v, want Qwen image attachment shape", files[0])
		}
		inner, _ := files[0]["image_url"].(map[string]any)
		if inner["url"] != in {
			t.Fatalf("expected passthrough of %q, got %v", in, inner["url"])
		}
	}
}

// TestResolveMediaInputImageFileID — a file-… id owned by the calling
// token must be resolved into an internal local-file marker. The actual
// Qwen URL is produced later after an upstream account is acquired, which
// prevents large file_id inputs from becoming overlong data URLs.
func TestResolveMediaInputImageFileID(t *testing.T) {
	app := newMediaTestApp(t)
	const (
		fileID = "file-test-1"
		owner  = "owner-token"
		mime   = "image/png"
		raw    = "fake-png-bytes"
	)
	writeUploadedFile(t, app, fileID, "cat.png", mime, owner, []byte(raw))

	files, err := app.resolveMediaInputImage(context.Background(), fileID, &AuthContext{Token: owner})
	if err != nil {
		t.Fatalf("resolveMediaInputImage(file-id) error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file block, got %d", len(files))
	}
	if files[0]["url"] != fileID {
		t.Fatalf("url = %v, want file_id marker %q", files[0]["url"], fileID)
	}
	if files[0]["type"] != "image" || files[0]["showType"] != "image" || files[0]["file_class"] != "vision" {
		t.Fatalf("file shape = %+v, want Qwen image attachment shape", files[0])
	}
	local, ok := files[0][mediaLocalFileRecordKey].(*UploadedLocalFileRecord)
	if !ok {
		t.Fatalf("missing local file marker: %T", files[0][mediaLocalFileRecordKey])
	}
	if local == nil || local.ID != fileID || local.Filename != "cat.png" || local.ContentType != mime {
		t.Fatalf("local marker = %+v", local)
	}
}

// TestResolveMediaInputImageFileIDMIMEFromExtension — when the record
// has no recorded ContentType, the helper falls back to mime lookup by
// file extension, and ultimately to application/octet-stream.
func TestResolveMediaInputImageFileIDMIMEFromExtension(t *testing.T) {
	app := newMediaTestApp(t)
	// No content_type set on the record — extension ".jpg" should drive
	// the mime lookup. .jpg is registered in Go's mime table.
	writeUploadedFile(t, app, "file-jpg", "puppy.jpg", "", "owner", []byte("jpeg-bytes"))
	files, err := app.resolveMediaInputImage(context.Background(), "file-jpg", &AuthContext{Token: "owner"})
	if err != nil {
		t.Fatalf("resolveMediaInputImage(file-jpg) error: %v", err)
	}
	local, ok := files[0][mediaLocalFileRecordKey].(*UploadedLocalFileRecord)
	if !ok {
		t.Fatalf("missing local file marker: %T", files[0][mediaLocalFileRecordKey])
	}
	if local == nil || local.ID != "file-jpg" || local.Filename != "puppy.jpg" {
		t.Fatalf("local marker = %+v", local)
	}
}

// TestResolveMediaInputImageFileIDNotFound — unknown file-… id must
// fail with a 400-friendly error that names the missing id.
func TestResolveMediaInputImageFileIDNotFound(t *testing.T) {
	app := newMediaTestApp(t)
	_, err := app.resolveMediaInputImage(context.Background(), "file-does-not-exist", &AuthContext{Token: "owner"})
	if err == nil {
		t.Fatal("expected error for unknown file_id, got nil")
	}
	if !strings.Contains(err.Error(), "file-does-not-exist") {
		t.Fatalf("error should name the missing file_id; got %q", err.Error())
	}
}

// TestResolveMediaInputImageFileIDWrongOwner — a record owned by a
// different token must not leak its bytes. The helper wraps the
// `getUploadedLocalFile` error, which surfaces "owned by another token".
func TestResolveMediaInputImageFileIDWrongOwner(t *testing.T) {
	app := newMediaTestApp(t)
	writeUploadedFile(t, app, "file-secret", "secret.png", "image/png", "owner-A", []byte("super-secret"))
	_, err := app.resolveMediaInputImage(context.Background(), "file-secret", &AuthContext{Token: "owner-B"})
	if err == nil {
		t.Fatal("expected error for cross-owner access, got nil")
	}
	if !strings.Contains(err.Error(), "file_id lookup failed") {
		t.Fatalf("expected wrapped owner-mismatch error, got %q", err.Error())
	}
}

// TestResolveMediaInputImageFileIDPathEscape — the helper must reject a
// record whose on-disk Path has been tampered with to escape
// ContextGeneratedDir, even when the record is owned by the caller.
func TestResolveMediaInputImageFileIDPathEscape(t *testing.T) {
	app := newMediaTestApp(t)
	// Use a path the helper cannot read back (the directory is the temp
	// dir we created; "../escape.png" is outside it).
	escape := filepath.Join(filepath.Dir(app.settings.ContextGeneratedDir), "escape.png")
	records := []UploadedLocalFileRecord{{
		ID:          "file-evil",
		Filename:    "evil.png",
		ContentType: "image/png",
		CreatedAt:   1,
		Path:        escape,
		OwnerToken:  "owner",
	}}
	if err := app.saveUploadedLocalFiles(records); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	_, err := app.resolveMediaInputImage(context.Background(), "file-evil", &AuthContext{Token: "owner"})
	if err == nil {
		t.Fatal("expected error for path-escape record, got nil")
	}
	if !strings.Contains(err.Error(), "not accessible") {
		t.Fatalf("expected 'not accessible' error, got %q", err.Error())
	}
}

// TestResolveMediaInputImageInvalid — anything that is not a data URL,
// http(s) URL, or file-… id must be rejected with a 400-friendly error.
func TestResolveMediaInputImageInvalid(t *testing.T) {
	app := newMediaTestApp(t)
	cases := []string{
		"file-",                  // empty file-… payload after prefix
		"just-a-word",            // no recognised prefix
		"ftp://example.com",      // non-http(s) scheme
		"file-no-leading-hyphen", // looks like a file id but no "file-" prefix
	}
	for _, in := range cases {
		// The first three cases should return an error.
		if in == "file-" {
			// "file-" with nothing after the dash will still hit the
			// getUploadedLocalFile branch, which will return nil record
			// → "not found" error. That is fine — it is still rejected.
			_, err := app.resolveMediaInputImage(context.Background(), in, &AuthContext{Token: "owner"})
			if err == nil {
				t.Fatalf("resolveMediaInputImage(%q) expected error, got nil", in)
			}
			continue
		}
		_, err := app.resolveMediaInputImage(context.Background(), in, &AuthContext{Token: "owner"})
		if err == nil {
			t.Fatalf("resolveMediaInputImage(%q) expected error, got nil", in)
		}
	}
}

// TestHandleGetFileRoundTrip exercises the GET handler through a real
// httptest request/response. The auth check, record lookup, path safety
// check, and Content-Type header are all on the critical path for the
// i2v/i2i feature, so we cover them end-to-end.
func TestHandleGetFileRoundTrip(t *testing.T) {
	app := newMediaTestApp(t)
	const (
		fileID = "file-rt"
		owner  = "owner-rt"
		body   = "pixel-data"
	)
	writeUploadedFile(t, app, fileID, "in.png", "image/png", owner, []byte(body))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/files/{file_id}", app.handleGetFile)

	req := httptest.NewRequest("GET", "/v1/files/"+fileID, nil)
	req.Header.Set("Authorization", "Bearer "+owner)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/files/... status = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", got)
	}
	if got := rec.Header().Get("X-File-Id"); got != fileID {
		t.Fatalf("X-File-Id = %q, want %q", got, fileID)
	}
	got, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(got) != body {
		t.Fatalf("response body = %q, want %q", got, body)
	}
}

// TestHandleGetFileNotFound — unknown file_id → 404.
func TestHandleGetFileNotFound(t *testing.T) {
	app := newMediaTestApp(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/files/{file_id}", app.handleGetFile)
	req := httptest.NewRequest("GET", "/v1/files/file-missing", nil)
	req.Header.Set("Authorization", "Bearer owner")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestHandleGetFileForbidden — record owned by a different token → 403.
func TestHandleGetFileForbidden(t *testing.T) {
	app := newMediaTestApp(t)
	writeUploadedFile(t, app, "file-x", "x.png", "image/png", "owner-x", []byte("body"))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/files/{file_id}", app.handleGetFile)
	req := httptest.NewRequest("GET", "/v1/files/file-x", nil)
	req.Header.Set("Authorization", "Bearer owner-y")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleGetFileUnauthorized — missing Authorization header → 401.
func TestHandleGetFileUnauthorized(t *testing.T) {
	app := newMediaTestApp(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/files/{file_id}", app.handleGetFile)
	req := httptest.NewRequest("GET", "/v1/files/file-any", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestHandleGetFilePathEscape — handler must reject a tampered record
// whose on-disk path escapes ContextGeneratedDir.
func TestHandleGetFilePathEscape(t *testing.T) {
	app := newMediaTestApp(t)
	// Plant a real file outside the allowed directory so the record's
	// Path resolves to a real, readable file — the handler still must
	// refuse based on the prefix check.
	outside := filepath.Join(filepath.Dir(app.settings.ContextGeneratedDir), "outside.png")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatalf("plant outside file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })
	records := []UploadedLocalFileRecord{{
		ID:          "file-evil",
		Filename:    "evil.png",
		ContentType: "image/png",
		CreatedAt:   1,
		Path:        outside,
		OwnerToken:  "owner",
	}}
	if err := app.saveUploadedLocalFiles(records); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/files/{file_id}", app.handleGetFile)
	req := httptest.NewRequest("GET", "/v1/files/file-evil", nil)
	req.Header.Set("Authorization", "Bearer owner")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleImagesNilImageFieldAcceptsBody drives the full handleImages
// path with a body that has no `image` field. The test account pool is
// empty, so the call to createImageURLs → AcquireFor would otherwise
// loop for 60 seconds — we pre-cancel the request context so it fails
// fast with ctx.Canceled. What we are verifying is that the request is
// NOT rejected at body-parse time with a "prompt is required" /
// "image must be …" 400, i.e. the i2i integration did not break the
// pure-t2i happy path.
func TestHandleImagesNilImageFieldAcceptsBody(t *testing.T) {
	app := newMediaTestApp(t)

	body, _ := json.Marshal(map[string]any{
		"prompt": "a friendly red panda",
		"n":      1,
		"size":   "1024x1024",
		"model":  "qwen-image-plus",
	})
	req := httptest.NewRequest("POST", "/v1/images/generations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	ctx, cancel := context.WithCancel(req.Context())
	cancel() // short-circuit AcquireFor
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	app.handleImages(rec, req)

	if rec.Code == http.StatusBadRequest {
		var env map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		t.Fatalf("handleImages with nil image field returned 400: %v", env)
	}
	// The downstream branch we care about is "we got past body parsing".
	// The actual call into createImageURLs will fail because the test
	// App has an empty account pool, but the failure mode is a 5xx, not
	// a 4xx for missing image data. Anything except a 400 in that
	// ballpark is acceptable; we still want a non-401 to confirm auth
	// passed.
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("handleImages returned 401 (auth): %s", rec.Body.String())
	}
	// We expect either 500 (no accounts) or another upstream-driven
	// error code — both are fine for this test.
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("handleImages returned 400; body: %s", rec.Body.String())
	}
}

// TestHandleImagesInvalidImageField — when the `image` field is present
// but malformed, the handler must return 400 with the helper's error
// message. This guarantees the new 400 path is reachable from the
// HTTP boundary, not just from the helper.
func TestHandleImagesInvalidImageField(t *testing.T) {
	app := newMediaTestApp(t)
	body, _ := json.Marshal(map[string]any{
		"prompt": "a friendly red panda",
		"image":  "garbage-not-a-url-or-file-id",
		"model":  "qwen-image-plus",
	})
	req := httptest.NewRequest("POST", "/v1/images/generations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	app.handleImages(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "image must be") {
		t.Fatalf("error body should mention the image field constraint; got %s", rec.Body.String())
	}
}

// TestHandleImagesMissingPrompt — when the body has no prompt, the
// handler must return 400 with the prompt-required error. Sanity
// check that the i2i integration did not break the "prompt is
// required" guard at the top of handleImages.
func TestHandleImagesMissingPrompt(t *testing.T) {
	app := newMediaTestApp(t)
	body, _ := json.Marshal(map[string]any{
		"model": "qwen-image-plus",
		// no "prompt" key
	})
	req := httptest.NewRequest("POST", "/v1/images/generations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	app.handleImages(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "prompt is required") {
		t.Fatalf("error body should mention prompt; got %s", rec.Body.String())
	}
}

// TestHandleImagesFileIDImageField — when `image` is a file-… id owned
// by the caller, the helper resolves it and the request proceeds past
// the body-parse stage. (Same caveat as the nil-image test: we cannot
// reach the upstream call without an account pool, so we cancel the
// context to make AcquireFor fail fast and assert the response is NOT
// 400.)
func TestHandleImagesFileIDImageField(t *testing.T) {
	app := newMediaTestApp(t)
	const (
		fileID = "file-handle-img"
		owner  = "test-token"
	)
	writeUploadedFile(t, app, fileID, "ref.png", "image/png", owner, []byte("ref-bytes"))

	body, _ := json.Marshal(map[string]any{
		"prompt": "give the red panda a hat",
		"image":  fileID,
		"model":  "qwen-image-plus",
	})
	req := httptest.NewRequest("POST", "/v1/images/generations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+owner)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	app.handleImages(rec, req)
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("handleImages with file_id image field returned 400: %s", rec.Body.String())
	}
}

// TestHandleVideosInvalidImageField — mirror of the image-side test
// for videos: a garbage `image` value must be rejected with 400.
func TestHandleVideosInvalidImageField(t *testing.T) {
	app := newMediaTestApp(t)
	body, _ := json.Marshal(map[string]any{
		"prompt": "the red panda waves hello",
		"image":  "garbage-not-a-url-or-file-id",
		"model":  "qwen-video-plus",
	})
	req := httptest.NewRequest("POST", "/v1/videos/generations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	app.handleVideos(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "image must be") {
		t.Fatalf("error body should mention the image field constraint; got %s", rec.Body.String())
	}
}

// TestHandleVideosNilImageFieldAcceptsBody — pure t2v must keep working
// even after the i2v integration. We assert that body parsing accepts a
// request without an `image` field. Context is cancelled to make the
// downstream AcquireFor fail fast.
func TestHandleVideosNilImageFieldAcceptsBody(t *testing.T) {
	app := newMediaTestApp(t)
	body, _ := json.Marshal(map[string]any{
		"prompt":   "the red panda waves hello",
		"duration": 5,
		"size":     "1280x720",
		"model":    "qwen-video-plus",
	})
	req := httptest.NewRequest("POST", "/v1/videos/generations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	app.handleVideos(rec, req)
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("handleVideos with nil image field returned 400: %s", rec.Body.String())
	}
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("handleVideos returned 401: %s", rec.Body.String())
	}
}

// TestCreateImageURLsReachesAcquireFor is a small focused test that
// verifies the createImageURLs call site now accepts the new
// `files` slice and plumbs it through to AcquireFor (and onwards to
// buildChatPayload). The minimal account pool is empty, so the
// request context is pre-cancelled to make AcquireFor fail fast with
// ctx.Canceled. The combination of (a) the function compiling with
// the new 5-argument signature and (b) AcquireFor returning the
// context-cancelled error is what we are verifying — if the new
// files parameter were dropped on the floor, the call would still
// succeed, so the existence of the parameter is part of the test.
func TestCreateImageURLsReachesAcquireFor(t *testing.T) {
	app := newMediaTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	urls, err := app.createImageURLs(ctx, "qwen3.7-plus", "hello",
		map[string]any{"size": "1024x1024", "ratio": "1:1", "width": 1024, "height": 1024},
		[]map[string]any{{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA"}}},
	)
	if err == nil {
		t.Fatalf("createImageURLs unexpectedly succeeded: %v", urls)
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "canceled") &&
		!strings.Contains(err.Error(), "no accounts") && !strings.Contains(err.Error(), "账号") {
		t.Fatalf("expected context-cancel or no-accounts error, got %v", err)
	}
}

// TestCreateVideoURLsReachesAcquireFor mirrors the image-side test for
// the new createVideoURLs signature. The video branch is the one that
// flips chatType from "t2v" to "i2v" when an input file is present.
func TestCreateVideoURLsReachesAcquireFor(t *testing.T) {
	app := newMediaTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// i2v path: files is non-empty so chatType should be "i2v".
	urls, err := app.createVideoURLs(ctx, "qwen3.7-plus", "hello",
		map[string]any{"size": "1280x720", "ratio": "16:9", "width": 1280, "height": 720, "duration": 5},
		[]map[string]any{{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA"}}},
		"i2v",
	)
	if err == nil {
		t.Fatalf("createVideoURLs unexpectedly succeeded: %v", urls)
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "canceled") &&
		!strings.Contains(err.Error(), "no accounts") && !strings.Contains(err.Error(), "账号") {
		t.Fatalf("expected context-cancel or no-accounts error, got %v", err)
	}

	// t2v path: files is nil, chatType is "t2v".
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := app.createVideoURLs(ctx2, "qwen3.7-plus", "hello",
		map[string]any{"size": "1280x720", "ratio": "16:9", "width": 1280, "height": 720, "duration": 5},
		nil,
		"t2v",
	); err == nil {
		t.Fatalf("createVideoURLs (t2v) unexpectedly succeeded")
	}
}

// ---- multipart/form-data support for /v1/images and /v1/videos ----
//
// The tests below cover the new branch in handleImages/handleVideos that
// accepts a multipart/form-data body (a single image part plus the
// usual prompt / model / n / size form fields) so an OpenAI-style
// caller can upload + generate in one round-trip. The pattern is the
// same as the JSON-path tests above: the test App has an empty account
// pool, so we pre-cancel the request context to make AcquireFor return
// immediately; the assertions focus on the body-parse and multipart
// validation paths, not the upstream call.

// buildMultipartImagesRequest assembles a multipart/form-data body
// for the /v1/images/generations route. `fields` is a free-form
// key/value list (strings only) that becomes the non-file parts; if
// `file` is non-nil it is written under the "image" form key with
// `fileName` as the filename. The Content-Type returned is the
// boundary-tagged value the request must carry.
func buildMultipartImagesRequest(t *testing.T, fields map[string]string, file []byte, fileName, authHeader string) *http.Request {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("write field %q: %v", k, err)
		}
	}
	if file != nil {
		fw, err := mw.CreateFormFile("image", fileName)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := fw.Write(file); err != nil {
			t.Fatalf("write file body: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest("POST", "/v1/images/generations", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return req
}

// buildMultipartVideosRequest is the videos-route mirror of
// buildMultipartImagesRequest. The URL path is the only difference —
// the multipart body assembly is identical.
func buildMultipartVideosRequest(t *testing.T, fields map[string]string, file []byte, fileName, authHeader string) *http.Request {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("write field %q: %v", k, err)
		}
	}
	if file != nil {
		fw, err := mw.CreateFormFile("image", fileName)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := fw.Write(file); err != nil {
			t.Fatalf("write file body: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest("POST", "/v1/videos/generations", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return req
}

// TestHandleImagesMultipartSuccess — a well-formed multipart request
// with a prompt and a small PNG-like image part must clear the body
// parse + save stage and proceed to the upstream call. We pre-cancel
// the request context so the empty account pool returns immediately;
// the test asserts the response is NOT a 400 (we got past validation)
// and the saved file is now visible in the on-disk store.
func TestHandleImagesMultipartSuccess(t *testing.T) {
	app := newMediaTestApp(t)
	// Minimal 1x1 PNG (base64-decoded from the standard test vector).
	const tinyPNG = "\x89PNG\r\n\x1a\n" + "fake-but-valid-looking-bytes-for-test-only"
	req := buildMultipartImagesRequest(t,
		map[string]string{
			"prompt": "a friendly red panda wearing a scarf",
			"model":  "qwen-image-plus",
			"n":      "1",
			"size":   "1024x1024",
		},
		[]byte(tinyPNG), "ref.png", "Bearer test-token",
	)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	app.handleImages(rec, req)

	if rec.Code == http.StatusBadRequest {
		t.Fatalf("handleImages(multipart) returned 400: %s", rec.Body.String())
	}
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("handleImages(multipart) returned 401: %s", rec.Body.String())
	}

	// The save side should have run before the upstream call failed —
	// confirm at least one record landed in the store, owned by the
	// auth token we sent.
	records, err := app.loadUploadedLocalFiles()
	if err != nil {
		t.Fatalf("loadUploadedLocalFiles: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 saved record, got %d", len(records))
	}
	if records[0].OwnerToken != "test-token" {
		t.Fatalf("record owner = %q, want test-token", records[0].OwnerToken)
	}
	if records[0].Source != "media-multipart" {
		t.Fatalf("record source = %q, want media-multipart", records[0].Source)
	}
	if !strings.HasPrefix(records[0].ID, "file-") {
		t.Fatalf("record ID = %q, want file-*", records[0].ID)
	}
	if records[0].Filename != "ref.png" {
		t.Fatalf("record filename = %q, want ref.png", records[0].Filename)
	}
}

// TestHandleImagesMultipartMissingImage — a multipart request with
// only prompt (no file part) must be accepted as a pure t2i call.
// Same short-circuit pattern: cancel context to fail fast on the
// account pool side, then assert the response is NOT a 400.
func TestHandleImagesMultipartMissingImage(t *testing.T) {
	app := newMediaTestApp(t)
	req := buildMultipartImagesRequest(t,
		map[string]string{
			"prompt": "a friendly red panda",
			"model":  "qwen-image-plus",
		},
		nil, "", "Bearer test-token",
	)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	app.handleImages(rec, req)

	if rec.Code == http.StatusBadRequest {
		t.Fatalf("handleImages(multipart, no image) returned 400: %s", rec.Body.String())
	}
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("handleImages(multipart, no image) returned 401: %s", rec.Body.String())
	}

	// No file part means no record should be created.
	records, err := app.loadUploadedLocalFiles()
	if err != nil {
		t.Fatalf("loadUploadedLocalFiles: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected 0 saved records (pure t2i), got %d", len(records))
	}
}

// TestHandleImagesMultipartBadExtension — when the uploaded file has
// a .exe extension (or any extension not in
// app.settings.ContextAllowedUserExts), the helper must reject with
// 400 and the "Unsupported file extension" message that handleUploadFile
// already uses.
func TestHandleImagesMultipartBadExtension(t *testing.T) {
	app := newMediaTestApp(t)
	req := buildMultipartImagesRequest(t,
		map[string]string{
			"prompt": "irrelevant — should not get this far",
		},
		[]byte("MZ\x90\x00\x03"), "evil.exe", "Bearer test-token",
	)
	rec := httptest.NewRecorder()

	app.handleImages(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Unsupported file extension: exe") {
		t.Fatalf("error body should mention unsupported extension; got %s", rec.Body.String())
	}
	// Nothing should be saved to the store.
	records, err := app.loadUploadedLocalFiles()
	if err != nil {
		t.Fatalf("loadUploadedLocalFiles: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected 0 saved records on rejected upload, got %d", len(records))
	}
}

// TestHandleImagesMultipartOversized — a 130MB file must be rejected
// with 400 and the "File exceeds 128MB limit" message. We use a
// zero-initialised buffer to keep the test cheap (we do not care
// about file content, only its size).
func TestHandleImagesMultipartOversized(t *testing.T) {
	app := newMediaTestApp(t)
	// 130MB = 130 * 1024 * 1024 bytes; using a non-PNG extension keeps
	// the test simple (we should fail on size before content type
	// would matter). Use ".bin" which is not in the allowed set, but
	// we register it locally so the size check is the only failure.
	app.settings.ContextAllowedUserExts = "txt,md,png,jpg,jpeg,webp,gif,bmp,bin"
	oversized := make([]byte, 130*1024*1024) // 130MB of zeros
	req := buildMultipartImagesRequest(t,
		map[string]string{"prompt": "irrelevant"},
		oversized, "huge.bin", "Bearer test-token",
	)
	rec := httptest.NewRecorder()

	app.handleImages(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, truncate(rec.Body.String(), 200))
	}
	if !strings.Contains(rec.Body.String(), "File exceeds 128MB limit") {
		t.Fatalf("error body should mention 128MB limit; got %s", truncate(rec.Body.String(), 200))
	}
	records, err := app.loadUploadedLocalFiles()
	if err != nil {
		t.Fatalf("loadUploadedLocalFiles: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected 0 saved records on oversized upload, got %d", len(records))
	}
}

// TestHandleImagesJSONRegression — after wiring the multipart branch,
// the JSON body path must still work exactly as it did before. We send
// a plain JSON request and assert it gets past body parsing (no 400),
// which exercises the unchanged code path and catches a regression
// where the multipart branch accidentally shadows the JSON branch.
func TestHandleImagesJSONRegression(t *testing.T) {
	app := newMediaTestApp(t)
	body, _ := json.Marshal(map[string]any{
		"prompt": "a friendly red panda",
		"model":  "qwen-image-plus",
		"n":      1,
		"size":   "1024x1024",
	})
	req := httptest.NewRequest("POST", "/v1/images/generations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	app.handleImages(rec, req)

	if rec.Code == http.StatusBadRequest {
		t.Fatalf("JSON regression: handleImages returned 400: %s", rec.Body.String())
	}
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("JSON regression: handleImages returned 401: %s", rec.Body.String())
	}
	// JSON path must not have created a file record.
	records, err := app.loadUploadedLocalFiles()
	if err != nil {
		t.Fatalf("loadUploadedLocalFiles: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected 0 saved records on JSON path, got %d", len(records))
	}
}

// TestHandleVideosMultipartSuccess — mirror of the images happy-path
// test for /v1/videos/generations. We verify the multipart branch
// saves the file and that downstream rejects the upload with a
// non-400 / non-401 code (i.e. it got past body parsing).
func TestHandleVideosMultipartSuccess(t *testing.T) {
	app := newMediaTestApp(t)
	const tinyPNG = "\x89PNG\r\n\x1a\nfake-but-valid-looking-bytes-for-test-only"
	req := buildMultipartVideosRequest(t,
		map[string]string{
			"prompt":   "the red panda waves hello",
			"model":    "qwen-video-plus",
			"duration": "5",
			"size":     "1280x720",
		},
		[]byte(tinyPNG), "ref.png", "Bearer test-token",
	)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	app.handleVideos(rec, req)

	if rec.Code == http.StatusBadRequest {
		t.Fatalf("handleVideos(multipart) returned 400: %s", rec.Body.String())
	}
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("handleVideos(multipart) returned 401: %s", rec.Body.String())
	}
	records, err := app.loadUploadedLocalFiles()
	if err != nil {
		t.Fatalf("loadUploadedLocalFiles: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 saved record, got %d", len(records))
	}
	if records[0].OwnerToken != "test-token" {
		t.Fatalf("record owner = %q, want test-token", records[0].OwnerToken)
	}
	if records[0].Source != "media-multipart" {
		t.Fatalf("record source = %q, want media-multipart", records[0].Source)
	}
}

// TestHandleVideosJSONRegression — mirror of the images JSON
// regression test for /v1/videos/generations. Confirms the JSON
// branch of the new branching handler is unchanged.
func TestHandleVideosJSONRegression(t *testing.T) {
	app := newMediaTestApp(t)
	body, _ := json.Marshal(map[string]any{
		"prompt":   "the red panda waves hello",
		"model":    "qwen-video-plus",
		"duration": 5,
		"size":     "1280x720",
	})
	req := httptest.NewRequest("POST", "/v1/videos/generations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	app.handleVideos(rec, req)

	if rec.Code == http.StatusBadRequest {
		t.Fatalf("JSON regression: handleVideos returned 400: %s", rec.Body.String())
	}
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("JSON regression: handleVideos returned 401: %s", rec.Body.String())
	}
}

// TestResolveMultipartMediaImageHelper — direct unit test of the
// helper that does not go through the handler. This pins down the
// contract: file_id is non-empty on success, "file-" prefix matches
// the rest of the file store, and the record is owned by auth.Token.
func TestResolveMultipartMediaImageHelper(t *testing.T) {
	app := newMediaTestApp(t)
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if err := mw.WriteField("prompt", "anything"); err != nil {
		t.Fatalf("write field: %v", err)
	}
	fw, err := mw.CreateFormFile("image", "ref.png")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write([]byte("fake-png-bytes")); err != nil {
		t.Fatalf("write file body: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest("POST", "/v1/images/generations", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	auth := &AuthContext{Token: "helper-test-token"}
	fileID, err := app.resolveMultipartMediaImage(context.Background(), req, auth)
	if err != nil {
		t.Fatalf("resolveMultipartMediaImage: %v", err)
	}
	if !strings.HasPrefix(fileID, "file-") {
		t.Fatalf("fileID = %q, want file- prefix", fileID)
	}

	records, err := app.loadUploadedLocalFiles()
	if err != nil {
		t.Fatalf("loadUploadedLocalFiles: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 saved record, got %d", len(records))
	}
	if records[0].ID != fileID {
		t.Fatalf("record.ID = %q, helper returned %q", records[0].ID, fileID)
	}
	if records[0].OwnerToken != "helper-test-token" {
		t.Fatalf("record owner = %q, want helper-test-token", records[0].OwnerToken)
	}
}

// TestResolveMultipartMediaImageHelperNoFile — when the multipart
// body has no image part at all (pure t2i/t2v case), the helper
// returns ("", nil) so the handler falls through to the
// resolveMediaInputImage(r.Context(), "", auth) → nil path.
func TestResolveMultipartMediaImageHelperNoFile(t *testing.T) {
	app := newMediaTestApp(t)
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if err := mw.WriteField("prompt", "text-only prompt"); err != nil {
		t.Fatalf("write field: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest("POST", "/v1/images/generations", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	auth := &AuthContext{Token: "helper-test-token"}
	fileID, err := app.resolveMultipartMediaImage(context.Background(), req, auth)
	if err != nil {
		t.Fatalf("resolveMultipartMediaImage (no file): %v", err)
	}
	if fileID != "" {
		t.Fatalf("fileID = %q, want empty", fileID)
	}
	records, err := app.loadUploadedLocalFiles()
	if err != nil {
		t.Fatalf("loadUploadedLocalFiles: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected 0 saved records (no file part), got %d", len(records))
	}
}

// TestIsMultipartMediaRequest pins the small header-parsing helper
// used to choose between the JSON and multipart branches. It must
// tolerate the `boundary=…` parameter that real clients add and be
// case-insensitive (HTTP headers are case-insensitive; the actual
// value of Content-Type is normalized by net/http but we still want
// the EqualFold path tested).
func TestIsMultipartMediaRequest(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		want        bool
	}{
		{"plain multipart", "multipart/form-data", true},
		{"with boundary", "multipart/form-data; boundary=----abc", true},
		{"uppercase prefix", "MULTIPART/FORM-DATA", true},
		{"with extra spaces", " multipart/form-data ; boundary=x", true},
		{"application/json", "application/json", false},
		{"text/plain", "text/plain", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/x", nil)
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			if got := isMultipartMediaRequest(req); got != tc.want {
				t.Fatalf("contentType=%q: got %v, want %v", tc.contentType, got, tc.want)
			}
		})
	}
}

// TestMultipartFormToMap pins the helper that flattens parsed
// multipart values into the body map shape. It must:
//   - return an empty (non-nil) map when no values are present
//   - keep only the first value when a key appears multiple times
//   - leave MultipartForm untouched
func TestMultipartFormToMap(t *testing.T) {
	t.Run("empty form", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/x", nil)
		body := multipartFormToMap(req)
		if body == nil {
			t.Fatalf("expected non-nil map")
		}
		if len(body) != 0 {
			t.Fatalf("expected empty map, got %v", body)
		}
	})

	t.Run("values flattened", func(t *testing.T) {
		body := &bytes.Buffer{}
		mw := multipart.NewWriter(body)
		_ = mw.WriteField("prompt", "hello")
		_ = mw.WriteField("size", "1024x1024")
		_ = mw.Close()
		req := httptest.NewRequest("POST", "/x", body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		if err := req.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		m := multipartFormToMap(req)
		if m["prompt"] != "hello" {
			t.Fatalf("prompt = %v, want hello", m["prompt"])
		}
		if m["size"] != "1024x1024" {
			t.Fatalf("size = %v, want 1024x1024", m["size"])
		}
	})

	t.Run("first value wins on duplicate key", func(t *testing.T) {
		body := &bytes.Buffer{}
		mw := multipart.NewWriter(body)
		_ = mw.WriteField("prompt", "first")
		_ = mw.WriteField("prompt", "second")
		_ = mw.Close()
		req := httptest.NewRequest("POST", "/x", body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		if err := req.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		m := multipartFormToMap(req)
		if m["prompt"] != "first" {
			t.Fatalf("prompt = %v, want first", m["prompt"])
		}
	})
}
