package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/config"
)

func dashboardHTMLForTest(t *testing.T) string {
	t.Helper()
	h, _ := adminSetup(t)
	rec := adminReq(t, h, http.MethodGet, "/admin/", "", testAdminKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard: %d", rec.Code)
	}
	return rec.Body.String()
}

func TestDashboardShellLoadsWithoutAdminKey(t *testing.T) {
	h, _ := adminSetup(t)
	rec := adminReq(t, h, http.MethodGet, "/admin/", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard shell status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `id="key"`) {
		t.Fatal("dashboard shell missing admin-key input")
	}

	// The shell is static; data remains protected until the entered key is sent
	// through X-Admin-Key by dashboard.js.
	rec = adminReq(t, h, http.MethodGet, "/admin/keys", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated data API status = %d, want 401", rec.Code)
	}
}

func TestDashboardAssetsEmbedded(t *testing.T) {
	h, _ := adminSetup(t)
	for _, path := range []string{"/admin/", "/admin/assets/dashboard.css", "/admin/assets/dashboard.js"} {
		rec := adminReq(t, h, http.MethodGet, path, "", testAdminKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, rec.Code)
		}
	}
}

func TestDashboardAssetsUnauthenticated(t *testing.T) {
	h, _ := adminSetup(t)
	// Static assets carry no config/private payload: served without admin key.
	for _, path := range []string{"/admin/assets/dashboard.css", "/admin/assets/dashboard.js"} {
		rec := adminReq(t, h, http.MethodGet, path, "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s unauthenticated: %d", path, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s Cache-Control %q, want no-store", path, got)
		}
	}
	// Data APIs stay authenticated.
	for _, path := range []string{"/admin/keys", "/admin/usage", "/admin/providers"} {
		rec := adminReq(t, h, http.MethodGet, path, "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s unauthenticated: %d, want 401", path, rec.Code)
		}
	}
}

func TestDashboardAssetsContentType(t *testing.T) {
	h, _ := adminSetup(t)
	cases := map[string]string{
		"/admin/":                     "text/html; charset=utf-8",
		"/admin/assets/dashboard.css": "text/css; charset=utf-8",
		"/admin/assets/dashboard.js":  "application/javascript; charset=utf-8",
	}
	for path, want := range cases {
		rec := adminReq(t, h, http.MethodGet, path, "", testAdminKey)
		if got := rec.Header().Get("Content-Type"); got != want {
			t.Errorf("%s Content-Type %q, want %q", path, got, want)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s Cache-Control %q, want no-store", path, got)
		}
	}
}

func TestDashboardKeepsOperationsSections(t *testing.T) {
	body := dashboardHTMLForTest(t)
	for _, id := range []string{"s-overview", "s-keys", "s-providers", "s-logs"} {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Errorf("missing %s", id)
		}
	}
}

func TestDashboardReferencesAssets(t *testing.T) {
	body := dashboardHTMLForTest(t)
	if !strings.Contains(body, `href="/admin/assets/dashboard.css"`) {
		t.Error("dashboard.html missing stylesheet link")
	}
	if !strings.Contains(body, `src="/admin/assets/dashboard.js"`) {
		t.Error("dashboard.html missing script src")
	}
	if strings.Contains(body, "<style>") || strings.Contains(body, "<script>") {
		t.Error("dashboard.html still contains inline style/script block")
	}
}

func dashboardAssetForTest(t *testing.T, path string) string {
	t.Helper()
	h, _ := adminSetup(t)
	rec := adminReq(t, h, http.MethodGet, path, "", testAdminKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: %d", path, rec.Code)
	}
	return rec.Body.String()
}

// TestDashboardSettings asserts the Settings section shell: top-level nav,
// s-settings section, sub-navigation, search, action controls, and the
// modal/tab ARIA hooks (Task 9, Step 1).
func TestDashboardSettings(t *testing.T) {
	html := dashboardHTMLForTest(t)
	js := dashboardAssetForTest(t, "/admin/assets/dashboard.js")
	css := dashboardAssetForTest(t, "/admin/assets/dashboard.css")

	for _, want := range []string{
		// top-level nav entry + section
		`data-s="settings"`, `id="s-settings"`,
		// sub-navigation IDs
		`id="subnav"`,
		`data-sub="general"`, `data-sub="providers"`, `data-sub="routes"`,
		`data-sub="pricing"`, `data-sub="resilience"`, `data-sub="search"`,
		`data-sub="advanced"`, `data-sub="raw"`,
		// controls
		`id="settings-search"`, `id="settings-add"`, `id="settings-duplicate"`,
		`id="settings-move-up"`, `id="settings-move-down"`, `id="settings-delete"`,
		`id="settings-discard"`, `id="settings-validate"`,
		// modal + tabs ARIA
		`role="dialog"`, `aria-modal="true"`, `role="tablist"`, `role="tab"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard.html missing %s", want)
		}
	}

	for _, want := range []string{
		"async function loadSettings(",
		"function renderSettingsSection(",
		"function renderField(",
		"function renderObject(",
		"function renderList(",
		"function renderMap(",
		"function setDraftValue(",
		"function markDirty(",
		"function filterSettings(",
		"const configState",
		"dirtyPaths",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("dashboard.js missing %s", want)
		}
	}

	// visual system: breakpoints + reduced motion + 44px targets
	for _, want := range []string{
		"@media (min-width:768px)", "@media (min-width:1024px)",
		"prefers-reduced-motion", "min-height:44px",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("dashboard.css missing %s", want)
		}
	}
}

// TestDashboardConfigWorkflowMarkers asserts the Task 10 save workflow:
// exact request methods, revision binding, dialog hook, and state functions.
func TestDashboardConfigWorkflowMarkers(t *testing.T) {
	js := dashboardAssetForTest(t, "/admin/assets/dashboard.js")
	html := dashboardHTMLForTest(t)
	for _, marker := range []string{
		`api("/admin/config/validate", {method:"POST"`,
		`api("/admin/config", {method:"PUT"`,
		"candidate_revision",
		"expected_revision",
		"restart_required_paths",
		"config-diff-dialog",
		"async function validateConfig(",
		"function openDiffDialog(",
		"async function confirmConfigSave(",
		"function switchEditorMode(",
		"function showFieldErrors(",
		"function handleConfigConflict(",
		"function discardConfigDraft(",
	} {
		if !strings.Contains(js, marker) {
			t.Errorf("dashboard.js missing %q", marker)
		}
	}
	if !strings.Contains(html, `id="config-diff-dialog"`) {
		t.Error("dashboard.html missing config-diff-dialog")
	}
	if !strings.Contains(html, "<dialog") {
		t.Error("config diff must use a native <dialog> element")
	}
}

// TestDashboardMobileLayout (Task 12 fix round 2): at 375px the page must not
// overflow horizontally. Pins the mobile breakpoint contract: single-column
// #app, non-sticky horizontal-scroll sidebar nav, wrapping header with a
// flexible admin-key input, and contained settings chrome. Desktop rules
// (min-width:768px/1024px) are pinned by TestDashboardSettings.
//
// Task 12 fix round 3: the sole 1fr track was still expanded by grid items'
// intrinsic min-content (horizontal aside nav / #subnav / long inputs), so
// scrollWidth was 586 at clientWidth 375. The track must be minmax(0,1fr)
// and every grid child must allow shrink below min-content (min-width:0)
// and be capped at the track width (max-width:100%).
func TestDashboardMobileLayout(t *testing.T) {
	css := dashboardAssetForTest(t, "/admin/assets/dashboard.css")

	idx := strings.Index(css, "@media (max-width:767px)")
	if idx < 0 {
		t.Fatal("dashboard.css missing @media (max-width:767px) mobile block")
	}
	mobile := css[idx:]
	for _, want := range []string{
		"position:static",    // aside not sticky on mobile
		"flex-direction:row", // nav becomes horizontal
		"overflow-x:auto",    // nav scrolls instead of overflowing
		"flex-wrap:wrap",     // header wraps
		"min-width:0",        // password input can shrink
	} {
		if !strings.Contains(mobile, want) {
			t.Errorf("mobile block missing %q", want)
		}
	}
	// Round 3 containment contract: the 1fr track must not be sized by grid
	// items' min-content, and grid children must be allowed to shrink to the
	// track width.
	if !strings.Contains(mobile, "#app{display:grid;grid-template-columns:minmax(0,1fr)") {
		t.Error("mobile block missing minmax(0,1fr) on #app track")
	}
	for _, sel := range []string{"aside", "main", "header", ".settings-shell", ".settings-main", "#subnav"} {
		// each selector must carry min-width:0 (shrink below min-content)
		i := strings.Index(mobile, sel)
		if i < 0 {
			t.Errorf("mobile block missing selector %q", sel)
			continue
		}
		end := strings.Index(mobile[i:], "}")
		if end < 0 || !strings.Contains(mobile[i:i+end], "min-width:0") {
			t.Errorf("mobile %s rule missing min-width:0", sel)
		}
	}
}

// TestDashboardKeyFields (Task 11, Step 1): every one of the eleven
// POST /admin/keys fields must appear in the create form and in the JS
// payload construction.
func TestDashboardKeyFields(t *testing.T) {
	html := dashboardHTMLForTest(t)
	js := dashboardAssetForTest(t, "/admin/assets/dashboard.js")

	var keyFields = []string{
		"name", "rpm", "tpm", "model_rpm", "limit_by_header", "daily_quota",
		"quota_tokens", "budget_usd", "allowed_models", "groups", "expires_at",
	}
	for _, f := range keyFields {
		if !strings.Contains(js, f+":") && !strings.Contains(js, f+" =") {
			t.Errorf("dashboard.js payload missing field %q", f)
		}
	}
	// Advanced disclosure for the non-common fields.
	if !strings.Contains(html, "<details") || !strings.Contains(html, "Advanced") {
		t.Error("dashboard.html missing Advanced <details> disclosure for extra key fields")
	}
	for _, id := range []string{"f-model-rpm", "f-limit-header", "f-daily-quota", "f-budget", "f-models", "f-groups", "f-expires"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("dashboard.html missing advanced input #%s", id)
		}
	}
	// datetime-local input converted to RFC3339.
	if !strings.Contains(html, `type="datetime-local"`) {
		t.Error("expires_at must use a datetime-local input")
	}
	if !strings.Contains(js, "toISOString") && !strings.Contains(js, "RFC3339") {
		t.Error("dashboard.js must convert datetime-local to RFC3339")
	}
	// Comma-separated models/groups parsed into arrays.
	if !strings.Contains(js, `split(",")`) {
		t.Error("dashboard.js must split comma-separated allowed_models/groups")
	}
	// Compact rows with expandable details.
	if !strings.Contains(js, "key-details") {
		t.Error("key rows must be compact with expandable details (key-details)")
	}
	// No inline handler attributes: DOM-safe event delegation.
	if strings.Contains(js, "onclick='") || strings.Contains(js, `onclick="`) || strings.Contains(js, "onchange='") {
		t.Error("dashboard.js must not emit inline onclick/onchange handlers; use event delegation")
	}
}

// TestDashboardProviderActions (Task 11, Step 1): provider rows expose
// disabled/balance_low independent of circuit, and the four actions.
func TestDashboardProviderActions(t *testing.T) {
	js := dashboardAssetForTest(t, "/admin/assets/dashboard.js")
	html := dashboardHTMLForTest(t)

	for _, marker := range []string{
		"p.disabled", "p.balance_low",
		`/admin/providers/" + encodeURIComponent(name) + "/disable`,
		`/admin/providers/" + encodeURIComponent(name) + "/enable`,
		"/admin/usage/export?format=csv",
	} {
		if !strings.Contains(js, marker) {
			t.Errorf("dashboard.js missing %q", marker)
		}
	}
	// CSV export via Blob fetch (keeps X-Admin-Key header), not navigation.
	if !strings.Contains(js, "blob(") && !strings.Contains(js, "Blob") {
		t.Error("CSV export must fetch as Blob to preserve the admin header")
	}
	// Edit configuration control navigates to Settings providers tab.
	if !strings.Contains(js, "editProvider") {
		t.Error("dashboard.js missing editProvider (Edit configuration)")
	}
	if !strings.Contains(html, "Export CSV") {
		t.Error("Logs section missing Export CSV button")
	}
}

// TestAdminMutationAuth (Task 11, Steps 1+5): every mutating admin route
// rejects ?key= query auth and requires the X-Admin-Key header; the
// dashboard document itself keeps query auth.
func TestAdminMutationAuth(t *testing.T) {
	h, _ := adminSetup(t)

	mutations := []struct{ method, path string }{
		{http.MethodPost, "/admin/keys"},
		{http.MethodPost, "/admin/keys/1/disable"},
		{http.MethodPost, "/admin/keys/1/enable"},
		{http.MethodDelete, "/admin/keys/1"},
		{http.MethodPost, "/admin/providers/fake/test"},
		{http.MethodPost, "/admin/providers/fake/circuit/reset"},
		{http.MethodPost, "/admin/providers/fake/disable"},
		{http.MethodPost, "/admin/providers/fake/enable"},
	}
	for _, m := range mutations {
		rec := adminReq(t, h, m.method, m.path+"?key="+testAdminKey, "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with ?key=: status %d, want 401 (mutations are header-only)",
				m.method, m.path, rec.Code)
		}
	}
	// Dashboard document keeps query auth.
	rec := adminReq(t, h, http.MethodGet, "/admin/?key="+testAdminKey, "", "")
	if rec.Code != http.StatusOK {
		t.Errorf("GET /admin/?key=: status %d, want 200 (query auth preserved)", rec.Code)
	}
}

// TestDashboardSchemaKinds compares every leaf kind in config.FormSchema()
// against the renderer's supported kinds; any new kind fails the build here.
func TestDashboardSchemaKinds(t *testing.T) {
	js := dashboardAssetForTest(t, "/admin/assets/dashboard.js")
	// renderer dispatch table: SUPPORTED_KINDS lists every kind renderField handles
	const marker = "SUPPORTED_KINDS"
	i := strings.Index(js, marker)
	if i < 0 {
		t.Fatalf("dashboard.js missing %s", marker)
	}
	supported := map[string]bool{}
	for _, k := range []string{"string", "number", "bool", "list", "map", "group"} {
		if strings.Contains(js[i:i+200], `"`+k+`"`) {
			supported[k] = true
		}
	}
	var walk func(f *config.FieldSchema)
	walk = func(f *config.FieldSchema) {
		if f == nil {
			return
		}
		if !supported[f.Kind] {
			t.Errorf("schema path %s has unsupported kind %q", f.Path, f.Kind)
		}
		walk(f.Item)
		for _, c := range f.Children {
			walk(c)
		}
	}
	for _, c := range config.FormSchema().Children {
		walk(c)
	}
}
