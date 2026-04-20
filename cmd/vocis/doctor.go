package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"vocis/internal/config"
	"vocis/internal/platform/gnome"
	"vocis/internal/recorder"
	"vocis/internal/securestore"
	"vocis/internal/sessionlog"
	"vocis/internal/transcribe"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check system dependencies",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDoctor()
	},
}

func runDoctor() error {
	cfgPath, err := config.Path()
	if err != nil {
		return err
	}
	logDir, err := sessionlog.Dir()
	if err != nil {
		return err
	}

	checks := []struct {
		label string
		value string
		ok    bool
	}{
		{label: "display", value: os.Getenv("DISPLAY"), ok: os.Getenv("DISPLAY") != ""},
	}

	for _, check := range checks {
		status := "ok"
		if !check.ok {
			status = "missing"
		}
		fmt.Printf("%-14s %s (%s)\n", check.label, status, check.value)
	}

	for _, cmd := range []string{"xdotool", "xclip"} {
		path, ok := findExecutable(cmd)
		status := "ok"
		if !ok {
			status = "missing"
		}
		fmt.Printf("%-14s %s (%s)\n", cmd, status, path)
	}
	audioStatus := "ok"
	audioValue := "pulse server"
	if err := recorder.Check(); err != nil {
		audioStatus = "missing"
		audioValue = err.Error()
	}
	fmt.Printf("%-14s %s (%s)\n", "audio", audioStatus, audioValue)

	if _, err := os.Stat(cfgPath); err == nil {
		fmt.Printf("%-14s ok (%s)\n", "config", cfgPath)
	} else {
		fmt.Printf("%-14s missing (%s)\n", "config", cfgPath)
	}
	fmt.Printf("%-14s ok (%s)\n", "log-dir", logDir)

	store := securestore.New()
	if _, err := store.APIKey(); err == nil {
		fmt.Printf("%-14s ok (keyring or env)\n", "openai-key")
	} else {
		fmt.Printf("%-14s missing (%v)\n", "openai-key", err)
	}

	if isWaylandLikeSession() {
		if gnome.Available() {
			fmt.Printf("%-14s ok (vocis-gnome extension responding on %s)\n", "wayland-hk", gnome.BusName)
		} else {
			fmt.Printf("%-14s missing (install + enable extensions/vocis-gnome, then log out/in)\n", "wayland-hk")
		}
	}

	if cfg, _, err := config.Load(); err == nil && cfg.Transcription.Backend == config.BackendLemonade {
		checkLemonadeModels(cfg)
	}

	return nil
}

// checkLemonadeModels hits /models (is it downloaded?) and /health
// (is it currently resident?) and reports both for the configured
// transcribe + postprocess model IDs. "Downloaded" means the model
// is on disk; "loaded" means Lemonade has it in memory right now
// and will respond to a request without a 5-10s load stall. The WS
// realtime stream silently accepts any model name at session.update,
// so a typo surfaces only as "no transcript ever arrives" — this
// check turns that into a boot-time diagnostic.
func checkLemonadeModels(cfg config.Config) {
	baseURL := strings.TrimRight(cfg.Transcription.BaseURL, "/")
	if baseURL == "" {
		fmt.Printf("%-14s missing (transcription.base_url is empty; set it to the Lemonade REST endpoint, e.g. http://localhost:13305/api/v1)\n", "lemonade")
		return
	}

	downloaded, err := fetchDownloadedModels(baseURL)
	if err != nil {
		fmt.Printf("%-14s missing (%v)\n", "lemonade", err)
		return
	}
	fmt.Printf("%-14s ok (%d downloaded at %s/models)\n", "lemonade", len(downloaded), baseURL)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	health, healthErr := transcribe.FetchLemonadeHealth(ctx, baseURL)
	loaded := map[string]bool{}
	if healthErr != nil {
		fmt.Printf("%-14s missing (%v)\n", "lemonade-hp", healthErr)
	} else {
		for _, name := range health.LoadedNames() {
			loaded[name] = true
		}
		fmt.Printf("%-14s ok (%d resident, version %s)\n", "lemonade-hp", len(loaded), health.Version)
	}

	reportModel("lemonade-tx", cfg.Transcription.Model, downloaded, loaded)
	if cfg.PostProcess.Enabled {
		reportModel("lemonade-pp", cfg.PostProcess.Model, downloaded, loaded)
	}
}

func fetchDownloadedModels(baseURL string) (map[string]bool, error) {
	url := baseURL + "/models"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID         string `json:"id"`
			Downloaded bool   `json:"downloaded"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	out := make(map[string]bool, len(payload.Data))
	for _, m := range payload.Data {
		if m.Downloaded {
			out[m.ID] = true
		}
	}
	return out, nil
}

func reportModel(label, model string, downloaded, loaded map[string]bool) {
	if model == "" {
		fmt.Printf("%-14s missing (model name is empty)\n", label)
		return
	}
	if !downloaded[model] {
		names := make([]string, 0, len(downloaded))
		for id := range downloaded {
			names = append(names, id)
		}
		fmt.Printf("%-14s missing (%q not downloaded: %s)\n", label, model, strings.Join(names, ", "))
		return
	}
	if loaded[model] {
		fmt.Printf("%-14s ok (%s, loaded)\n", label, model)
		return
	}
	fmt.Printf("%-14s warn (%s, downloaded but not loaded — first request will trigger a 5-10s load)\n", label, model)
}

func isWaylandLikeSession() bool {
	if strings.EqualFold(os.Getenv("XDG_SESSION_TYPE"), "wayland") {
		return true
	}
	return os.Getenv("WAYLAND_DISPLAY") != ""
}
