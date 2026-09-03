package server

import (
	_ "embed"
	"net/http"
)

//go:embed web/dashboard.html
var dashboardHTML string

//go:embed web/dashboard.css
var dashboardCSS string

//go:embed web/dashboard.js
var dashboardJS string

func (s *srv) adminDashboard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(dashboardHTML))
}

func (s *srv) adminDashboardCSS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(dashboardCSS))
}

func (s *srv) adminDashboardJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(dashboardJS))
}
