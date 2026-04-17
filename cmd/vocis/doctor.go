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

	if cfg, _, err := config.Load(); err == nil && cfg.OpenAI.Backend == config.BackendLemonade {
		checkLemonadeModels(cfg)
	}

	return nil
}

// checkLemonadeModels hits the Lemonade REST /models endpoint and reports
// whether the configured transcribe + postprocess model IDs are present.
// Lemonade's realtime WS silently accepts any model name at session.update,
// so a typo in config surfaces only as "no transcript ever arrives". This
// check turns that into a boot-time diagnostic.
func checkLemonadeModels(cfg config.Config) {
	baseURL := strings.TrimRight(cfg.OpenAI.BaseURL, "/")
	if baseURL == "" {
		fmt.Printf("%-14s missing (openai.base_url is empty; set it to the Lemonade REST endpoint, e.g. http://localhost:13305/api/v1)\n", "lemonade")
		return
	}
	url := baseURL + "/models"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Printf("%-14s missing (build request: %v)\n", "lemonade", err)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("%-14s missing (GET %s: %v)\n", "lemonade", url, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("%-14s missing (GET %s: status %d)\n", "lemonade", url, resp.StatusCode)
		return
	}

	var payload struct {
		Data []struct {
			ID         string `json:"id"`
			Downloaded bool   `json:"downloaded"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		fmt.Printf("%-14s missing (decode models: %v)\n", "lemonade", err)
		return
	}

	available := make(map[string]bool, len(payload.Data))
	for _, m := range payload.Data {
		if m.Downloaded {
			available[m.ID] = true
		}
	}
	fmt.Printf("%-14s ok (%d downloaded model(s) at %s)\n", "lemonade", len(available), url)

	reportModel("lemonade-tx", cfg.OpenAI.Model, available)
	if cfg.PostProcess.Enabled {
		reportModel("lemonade-pp", cfg.PostProcess.Model, available)
	}
}

func reportModel(label, model string, available map[string]bool) {
	if model == "" {
		fmt.Printf("%-14s missing (model name is empty)\n", label)
		return
	}
	if available[model] {
		fmt.Printf("%-14s ok (%s)\n", label, model)
		return
	}
	names := make([]string, 0, len(available))
	for id := range available {
		names = append(names, id)
	}
	fmt.Printf("%-14s missing (%q not in downloaded models: %s)\n", label, model, strings.Join(names, ", "))
}

func isWaylandLikeSession() bool {
	if strings.EqualFold(os.Getenv("XDG_SESSION_TYPE"), "wayland") {
		return true
	}
	return os.Getenv("WAYLAND_DISPLAY") != ""
}
