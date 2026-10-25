// Package web serves the plugin's control panel (unauthenticated static
// resources) and its JSON API (Management API routes, which CLIProxyAPI
// authenticates with the management key before they reach the plugin).
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Danielw412/cpa-custom-router/internal/engine"
)

//go:embed assets/ui.html assets/ui.css assets/ui.js
var assets embed.FS

const (
	PluginID     = "custom-quota-router"
	APIBase      = "/v0/management/" + PluginID
	ResourceBase = "/v0/resource/plugins/" + PluginID
)

// Backend is what the API needs from the running plugin.
type Backend interface {
	Engine() *engine.Engine
	RequestPoll()
	WakeStarter()
	LastError() string
	// WakeHeld makes held requests re-check a changed pause or slow down,
	// Waiting counts them, and Persist saves state right away.
	WakeHeld()
	Waiting() int
	Persist()
}

// Response mirrors pluginapi.ManagementResponse.
type Response struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// Route is a management or resource route declaration.
type Route struct {
	Method      string `json:"Method,omitempty"`
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

// ManagementRoutes are authenticated JSON endpoints.
func ManagementRoutes() []Route {
	return []Route{
		{Method: "GET", Path: APIBase + "/state"},
		{Method: "POST", Path: APIBase + "/mode"},
		{Method: "POST", Path: APIBase + "/select"},
		{Method: "POST", Path: APIBase + "/refresh"},
		{Method: "POST", Path: APIBase + "/windows/start"},
		{Method: "POST", Path: APIBase + "/windows/schedule"},
		{Method: "POST", Path: APIBase + "/windows/cancel"},
		{Method: "POST", Path: APIBase + "/pause"},
		{Method: "POST", Path: APIBase + "/resume"},
		{Method: "POST", Path: APIBase + "/slowdown"},
	}
}

// ResourceRoutes are the static panel files; "/ui" appears in the dashboard menu.
func ResourceRoutes() []Route {
	return []Route{
		{Path: ResourceBase + "/ui", Menu: "Quota Router", Description: "Quota-aware Codex account routing: mode, current account, pause and slow down, preferences and switch history."},
		{Path: ResourceBase + "/ui.css"},
		{Path: ResourceBase + "/ui.js"},
	}
}

// Handle dispatches one request by exact path.
func Handle(b Backend, version, method, path string, query url.Values, body []byte) Response {
	path = strings.TrimRight(path, "/")
	switch {
	case strings.HasPrefix(path, ResourceBase+"/"):
		if method != http.MethodGet {
			return jsonError(http.StatusMethodNotAllowed, "method not allowed")
		}
		return serveAsset(strings.TrimPrefix(path, ResourceBase+"/"), version)
	case path == APIBase+"/state" && method == http.MethodGet:
		limit := 50
		if v, err := strconv.Atoi(query.Get("history")); err == nil && v > 0 && v <= 1000 {
			limit = v
		}
		return stateResponse(b, version, limit)
	case path == APIBase+"/mode" && method == http.MethodPost:
		var req struct {
			Mode    string `json:"mode"`
			Account string `json:"account"`
		}
		if err := decode(body, &req); err != nil {
			return jsonError(http.StatusBadRequest, err.Error())
		}
		switch strings.ToLower(strings.TrimSpace(req.Mode)) {
		case engine.ModeAuto:
			b.Engine().SetAuto()
		case engine.ModeManual:
			if strings.TrimSpace(req.Account) == "" {
				return jsonError(http.StatusBadRequest, "manual mode needs an account to pin")
			}
			if err := b.Engine().Pin(strings.TrimSpace(req.Account)); err != nil {
				return accountError(err)
			}
		default:
			return jsonError(http.StatusBadRequest, `mode must be "auto" or "manual"`)
		}
		return stateResponse(b, version, 200)
	case path == APIBase+"/select" && method == http.MethodPost:
		var req struct {
			Account string `json:"account"`
		}
		if err := decode(body, &req); err != nil {
			return jsonError(http.StatusBadRequest, err.Error())
		}
		if err := b.Engine().Select(strings.TrimSpace(req.Account)); err != nil {
			return accountError(err)
		}
		return stateResponse(b, version, 200)
	case path == APIBase+"/refresh" && method == http.MethodPost:
		b.RequestPoll()
		return stateResponse(b, version, 200)
	case path == APIBase+"/windows/start" && method == http.MethodPost:
		var req struct {
			Account        string `json:"account"`
			DelayMinutes   int    `json:"delay_minutes"`
			StaggerMinutes int    `json:"stagger_minutes"`
		}
		if err := decode(body, &req); err != nil {
			return jsonError(http.StatusBadRequest, err.Error())
		}
		if req.DelayMinutes < 0 || req.DelayMinutes > 1440 || req.StaggerMinutes < 0 || req.StaggerMinutes > 720 {
			return jsonError(http.StatusBadRequest, "delay must be 0-1440 minutes and stagger 0-720 minutes")
		}
		_, err := b.Engine().PlanStarts(strings.TrimSpace(req.Account),
			time.Duration(req.DelayMinutes)*time.Minute, time.Duration(req.StaggerMinutes)*time.Minute)
		if err != nil {
			return accountError(err)
		}
		b.WakeStarter()
		return stateResponse(b, version, 200)
	case path == APIBase+"/windows/schedule" && method == http.MethodPost:
		var req struct {
			Account string    `json:"account"`
			At      time.Time `json:"at"`
			Daily   bool      `json:"daily"`
		}
		if err := decode(body, &req); err != nil {
			return jsonError(http.StatusBadRequest, err.Error())
		}
		if _, err := b.Engine().PlanManual(strings.TrimSpace(req.Account), req.At, req.Daily); err != nil {
			if errors.Is(err, engine.ErrBadSchedule) {
				return jsonError(http.StatusBadRequest, "pick a time within the next 7 days")
			}
			return accountError(err)
		}
		b.WakeStarter()
		return stateResponse(b, version, 200)
	case path == APIBase+"/windows/cancel" && method == http.MethodPost:
		var req struct {
			ID      string `json:"id"`
			Account string `json:"account"`
		}
		if err := decode(body, &req); err != nil {
			return jsonError(http.StatusBadRequest, err.Error())
		}
		if _, err := b.Engine().CancelStarts(strings.TrimSpace(req.ID), strings.TrimSpace(req.Account)); err != nil {
			return jsonError(http.StatusNotFound, "that scheduled start no longer exists")
		}
		b.WakeStarter()
		return stateResponse(b, version, 200)
	case path == APIBase+"/pause" && method == http.MethodPost:
		var req struct {
			Minutes           int `json:"minutes"`
			AutoResumeMinutes int `json:"auto_resume_minutes"`
		}
		if err := decode(body, &req); err != nil {
			return jsonError(http.StatusBadRequest, err.Error())
		}
		if err := b.Engine().Pause(time.Duration(req.Minutes)*time.Minute, req.AutoResumeMinutes); err != nil {
			return jsonError(http.StatusBadRequest, fmt.Sprintf("pause for 0 (until resumed) to %d minutes, with auto-resume 0 (off) to %d minutes",
				int(engine.MaxPause/time.Minute), engine.MaxAutoResumeMinutes))
		}
		return changed(b, version)
	case path == APIBase+"/resume" && method == http.MethodPost:
		if err := decode(body, &struct{}{}); err != nil {
			return jsonError(http.StatusBadRequest, err.Error())
		}
		b.Engine().Resume()
		return changed(b, version)
	case path == APIBase+"/slowdown" && method == http.MethodPost:
		var req struct {
			Seconds int `json:"seconds"`
		}
		if err := decode(body, &req); err != nil {
			return jsonError(http.StatusBadRequest, err.Error())
		}
		if err := b.Engine().SetSlowDown(req.Seconds); err != nil {
			return jsonError(http.StatusBadRequest, fmt.Sprintf("slow down must be 0 (off) to %d seconds", engine.MaxSlowDownSeconds))
		}
		return changed(b, version)
	}
	return jsonError(http.StatusNotFound, "not found")
}

// changed applies a pause or slow-down change to held requests at once and
// saves it, so a reload right after keeps it.
func changed(b Backend, version string) Response {
	b.WakeHeld()
	b.Persist()
	return stateResponse(b, version, 200)
}

type stateBody struct {
	engine.View
	Version   string `json:"version"`
	LastError string `json:"last_error,omitempty"`
	// Waiting counts requests held by a pause or slow down right now.
	Waiting int `json:"waiting"`
}

func stateResponse(b Backend, version string, historyLimit int) Response {
	body, err := json.Marshal(stateBody{View: b.Engine().View(historyLimit), Version: version, LastError: b.LastError(), Waiting: b.Waiting()})
	if err != nil {
		return jsonError(http.StatusInternalServerError, "encode state failed")
	}
	return Response{StatusCode: http.StatusOK, Headers: apiHeaders(), Body: body}
}

func decode(body []byte, v any) error {
	if len(body) > 64<<10 {
		return errors.New("request body too large")
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid JSON body")
	}
	return nil
}

func accountError(err error) Response {
	if errors.Is(err, engine.ErrUnknownAccount) {
		return jsonError(http.StatusNotFound, "unknown account")
	}
	return jsonError(http.StatusBadRequest, err.Error())
}

func apiHeaders() http.Header {
	return http.Header{
		"Content-Type":           {"application/json; charset=utf-8"},
		"Cache-Control":          {"no-store"},
		"X-Content-Type-Options": {"nosniff"},
	}
}

func jsonError(status int, msg string) Response {
	body, _ := json.Marshal(map[string]string{"error": msg})
	return Response{StatusCode: status, Headers: apiHeaders(), Body: body}
}

func serveAsset(name, version string) Response {
	var file, ctype string
	switch name {
	case "ui":
		file, ctype = "assets/ui.html", "text/html; charset=utf-8"
	case "ui.css":
		file, ctype = "assets/ui.css", "text/css; charset=utf-8"
	case "ui.js":
		file, ctype = "assets/ui.js", "text/javascript; charset=utf-8"
	default:
		return jsonError(http.StatusNotFound, "not found")
	}
	data, err := assets.ReadFile(file)
	if err != nil {
		return jsonError(http.StatusNotFound, "not found")
	}
	if name == "ui" {
		data = []byte(strings.ReplaceAll(string(data), "{{VERSION}}", url.QueryEscape(version)))
	}
	return Response{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":            {ctype},
			"Cache-Control":           {"no-cache"},
			"X-Content-Type-Options":  {"nosniff"},
			"Referrer-Policy":         {"no-referrer"},
			"Content-Security-Policy": {"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'self'; base-uri 'none'; form-action 'none'"},
		},
		Body: data,
	}
}
