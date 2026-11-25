package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

// releaseGPU asks one app to free its GPU memory through its release_path. It contacts only
// that endpoint: it never wakes the host and never goes through the public upstream handler.
// The optional GPU_CONTROL_TOKEN is sent as a bearer token.
func (a *admin) releaseGPU(w http.ResponseWriter, r *http.Request) {
	if !a.bridge.Allows(audiencePrivate) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "bridge is off"})
		return
	}
	var app *Upstream
	for _, u := range a.ups {
		if u.cfg.Name == r.FormValue("name") && u.cfg.ReleasePath != "" {
			app = u
			break
		}
	}
	if app == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "app has no GPU release control"})
		return
	}
	target := *app.target
	target.Path, target.RawPath, target.RawQuery, target.Fragment = app.cfg.ReleasePath, "", "", ""
	ctx, cancel := context.WithTimeout(r.Context(), 65*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewBufferString("{}"))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "invalid release endpoint"})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// This is an internal request. Apps may allow localhost while refusing IP Host headers.
	req.Host = "localhost"
	if token := os.Getenv("GPU_CONTROL_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Transport: app.client.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "could not release GPU; the app may be offline"})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var detail struct {
			Error  string `json:"error"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(body, &detail)
		message := detail.Error
		if message == "" {
			message = detail.Detail
		}
		if message == "" {
			message = "app refused GPU release"
		}
		code := http.StatusBadGateway
		if resp.StatusCode == http.StatusConflict {
			code = http.StatusConflict
		}
		writeJSON(w, code, map[string]any{"error": message})
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	log.Printf("%s: GPU release completed", app.cfg.Name)
	a.reply(w, r, map[string]any{"released": true, "app": app.cfg.Name})
}
