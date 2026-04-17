package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"vocis/internal/config"
	"vocis/internal/securestore"
)

// Lemonade protocol defaults used when probing /health fails or is unreachable.
// These match the defaults documented in config.example.yaml.
const (
	lemonadeDefaultBaseURL     = "http://localhost:13305/api/v1"
	lemonadeDefaultRealtimeURL = "ws://localhost:9000"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage vocis configuration",
}

var configInitForce bool

var configInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Create default config file",
	Long: `Create a default config file. If a config already exists, writes the new
defaults to config.new.yaml and opens Neovim in diff mode so you can merge.
Use --force to overwrite without diffing.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runConfigInit(configInitForce)
	},
}

var configBackendCmd = &cobra.Command{
	Use:   "backend",
	Short: "Pick the transcription backend (openai or lemonade)",
	Long: `Interactively pick the backend and rewrite openai.backend plus the URL
fields. Selecting lemonade probes http://localhost:13305/api/v1/health for the
websocket_port and sets realtime_url accordingly; falls back to ws://localhost:9000
when the probe fails.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runConfigBackend()
	},
}

var configModelsCmd = &cobra.Command{
	Use:   "models",
	Short: "List models from the configured backend and pick transcription + postprocess models",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runConfigModels()
	},
}

func init() {
	configInitCmd.Flags().BoolVar(&configInitForce, "force", false, "overwrite existing config with defaults")
	configCmd.AddCommand(configInitCmd)
	configCmd.AddCommand(configBackendCmd)
	configCmd.AddCommand(configModelsCmd)
}

// runConfigInit is the former top-level `vocis init`, moved under `vocis config init`.
func runConfigInit(force bool) error {
	path, err := config.Path()
	if err != nil {
		return err
	}

	if force {
		if err := config.Save(path, config.Default()); err != nil {
			return err
		}
		fmt.Printf("wrote %s (forced)\n", path)
		return nil
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := config.Save(path, config.Default()); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", path)
		return nil
	}

	newPath := path + ".new"
	if err := config.Save(newPath, config.Default()); err != nil {
		return err
	}
	fmt.Printf("wrote new defaults to %s\n", newPath)
	fmt.Printf("opening diff: %s (current) vs %s (new defaults)\n", path, newPath)

	cmd := exec.Command("nvim", "-d", "-c", "set diffopt+=iwhite", path, newPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nvim: %w", err)
	}

	if err := os.Remove(newPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cleanup %s: %w", newPath, err)
	}
	fmt.Println("cleaned up", newPath)
	return nil
}

func runConfigBackend() error {
	cfg, path, err := config.Load()
	if err != nil {
		return err
	}

	fmt.Printf("Current backend: %s\n\n", cfg.OpenAI.Backend)
	fmt.Println("Available backends:")
	fmt.Println("  1) openai    — hosted OpenAI realtime API (requires API key)")
	fmt.Println("  2) lemonade  — local Lemonade Server (no auth, autodetected on localhost)")
	fmt.Print("\nPick [1-2]: ")

	choice, err := readLine()
	if err != nil {
		return err
	}

	switch strings.ToLower(strings.TrimSpace(choice)) {
	case "1", "openai":
		cfg.OpenAI.Backend = config.BackendOpenAI
		cfg.OpenAI.BaseURL = "https://api.openai.com/v1"
		cfg.OpenAI.RealtimeURL = ""
		fmt.Printf("\nSet backend=openai\n  base_url=%s\n  realtime_url=(empty)\n", cfg.OpenAI.BaseURL)
	case "2", "lemonade":
		cfg.OpenAI.Backend = config.BackendLemonade
		base, ws, detected := detectLemonade()
		cfg.OpenAI.BaseURL = base
		cfg.OpenAI.RealtimeURL = ws
		status := "used defaults (no server responded)"
		if detected {
			status = "detected running Lemonade Server"
		}
		fmt.Printf("\nSet backend=lemonade (%s)\n  base_url=%s\n  realtime_url=%s\n", status, base, ws)
	default:
		return fmt.Errorf("invalid choice: %q", strings.TrimSpace(choice))
	}

	if err := config.Save(path, cfg); err != nil {
		return err
	}
	fmt.Printf("\nWrote %s\n", path)
	return nil
}

// detectLemonade probes the default Lemonade REST endpoint and returns
// (base_url, realtime_url, detected). When /health exposes websocket_port,
// that port is used for the realtime URL; otherwise we fall back to
// ws://localhost:9000.
func detectLemonade() (string, string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, lemonadeDefaultBaseURL+"/health", nil)
	if err != nil {
		return lemonadeDefaultBaseURL, lemonadeDefaultRealtimeURL, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return lemonadeDefaultBaseURL, lemonadeDefaultRealtimeURL, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return lemonadeDefaultBaseURL, lemonadeDefaultRealtimeURL, false
	}

	var payload struct {
		WebsocketPort int `json:"websocket_port"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err == nil && payload.WebsocketPort > 0 {
		return lemonadeDefaultBaseURL, fmt.Sprintf("ws://localhost:%d", payload.WebsocketPort), true
	}
	return lemonadeDefaultBaseURL, lemonadeDefaultRealtimeURL, true
}

type modelChoice struct {
	ID   string
	Note string
}

func runConfigModels() error {
	cfg, path, err := config.Load()
	if err != nil {
		return err
	}

	txModels, ppModels, err := fetchModels(cfg)
	if err != nil {
		return err
	}
	if len(txModels) == 0 {
		return fmt.Errorf("no transcription-capable models returned from backend %q", cfg.OpenAI.Backend)
	}
	if len(ppModels) == 0 {
		return fmt.Errorf("no chat-capable models returned from backend %q", cfg.OpenAI.Backend)
	}

	fmt.Printf("Backend: %s\n\n", cfg.OpenAI.Backend)

	fmt.Printf("Transcription model (current: %s)\n", cfg.OpenAI.Model)
	newTx, err := pickModel(txModels, cfg.OpenAI.Model)
	if err != nil {
		return err
	}

	fmt.Printf("\nPost-processing model (current: %s)\n", cfg.PostProcess.Model)
	newPP, err := pickModel(ppModels, cfg.PostProcess.Model)
	if err != nil {
		return err
	}

	cfg.OpenAI.Model = newTx
	cfg.PostProcess.Model = newPP

	if err := config.Save(path, cfg); err != nil {
		return err
	}
	fmt.Printf("\nWrote %s\n  openai.model=%s\n  postprocess.model=%s\n",
		path, cfg.OpenAI.Model, cfg.PostProcess.Model)
	return nil
}

func fetchModels(cfg config.Config) (tx, pp []modelChoice, err error) {
	switch cfg.OpenAI.Backend {
	case config.BackendLemonade:
		return fetchLemonadeModels(cfg)
	case config.BackendOpenAI, "":
		return fetchOpenAIModels(cfg)
	default:
		return nil, nil, fmt.Errorf("unknown backend %q", cfg.OpenAI.Backend)
	}
}

func fetchOpenAIModels(cfg config.Config) (tx, pp []modelChoice, err error) {
	store := securestore.New()
	key, err := store.APIKey()
	if err != nil {
		return nil, nil, err
	}

	baseURL := strings.TrimRight(cfg.OpenAI.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if org := strings.TrimSpace(cfg.OpenAI.Organization); org != "" {
		req.Header.Set("OpenAI-Organization", org)
	}
	if proj := strings.TrimSpace(cfg.OpenAI.Project); proj != "" {
		req.Header.Set("OpenAI-Project", proj)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("GET %s/models: %w", baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("GET %s/models: status %d", baseURL, resp.StatusCode)
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, nil, fmt.Errorf("decode models: %w", err)
	}

	ids := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)

	for _, id := range ids {
		if looksLikeOpenAITXModel(id) {
			tx = append(tx, modelChoice{ID: id})
		}
		if looksLikeOpenAIPPModel(id) {
			pp = append(pp, modelChoice{ID: id})
		}
	}
	return tx, pp, nil
}

// looksLikeOpenAITXModel tags OpenAI realtime-transcription-capable models.
// The /v1/models endpoint doesn't carry capability metadata so we apply
// name-based heuristics: anything with "transcribe" or "whisper" in the id.
func looksLikeOpenAITXModel(id string) bool {
	lower := strings.ToLower(id)
	return strings.Contains(lower, "transcribe") || strings.Contains(lower, "whisper")
}

// looksLikeOpenAIPPModel tags chat-completion-capable models. Again,
// capability metadata isn't in the list response — we keep gpt-* and o[0-9]*
// families and drop obviously non-chat suffixes (transcribe, tts, embedding,
// image, moderation, search, realtime, audio).
func looksLikeOpenAIPPModel(id string) bool {
	lower := strings.ToLower(id)
	for _, bad := range []string{"transcribe", "tts", "embedding", "image", "moderation", "search", "realtime", "audio", "dall-e"} {
		if strings.Contains(lower, bad) {
			return false
		}
	}
	if strings.HasPrefix(lower, "gpt-") {
		return true
	}
	if len(lower) >= 2 && lower[0] == 'o' && lower[1] >= '0' && lower[1] <= '9' {
		return true
	}
	return false
}

func fetchLemonadeModels(cfg config.Config) (tx, pp []modelChoice, err error) {
	baseURL := strings.TrimRight(cfg.OpenAI.BaseURL, "/")
	if baseURL == "" {
		return nil, nil, errors.New("openai.base_url is empty; run `vocis config backend` and pick lemonade first")
	}

	// show_all=true returns Lemonade's full registry (not just downloaded),
	// so the picker can pull a new model without touching the CLI.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models?show_all=true", nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("GET %s/models: %w", baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("GET %s/models: status %d", baseURL, resp.StatusCode)
	}

	var payload struct {
		Data []struct {
			ID         string   `json:"id"`
			Downloaded bool     `json:"downloaded"`
			Labels     []string `json:"labels"`
			Recipe     string   `json:"recipe"`
			Size       float64  `json:"size"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, nil, fmt.Errorf("decode models: %w", err)
	}

	for _, m := range payload.Data {
		// Skip Lemonade "experience" entries — those are curated bundles,
		// not runnable models, and they have no size.
		if m.Recipe == "experience" {
			continue
		}
		labels := labelSet(m.Labels)
		note := lemonadeNote(m.Size, m.Recipe, m.Downloaded)
		choice := modelChoice{ID: m.ID, Note: note}
		if labels["transcription"] {
			tx = append(tx, choice)
		}
		if isLemonadePP(labels) {
			pp = append(pp, choice)
		}
	}
	sort.Slice(tx, lemonadeLess(tx))
	sort.Slice(pp, lemonadeLess(pp))
	return tx, pp, nil
}

// isLemonadePP returns true for chat models suitable for post-processing.
// We exclude reasoning models because their streaming response emits
// reasoning_content before content, which trips the first-token timeout
// in PostProcess. The other exclusions drop non-chat modalities.
func isLemonadePP(labels map[string]bool) bool {
	for _, bad := range []string{"tts", "transcription", "embeddings", "reranking", "image", "esrgan", "vision", "speech", "reasoning"} {
		if labels[bad] {
			return false
		}
	}
	return true
}

func labelSet(ls []string) map[string]bool {
	m := make(map[string]bool, len(ls))
	for _, l := range ls {
		m[strings.ToLower(l)] = true
	}
	return m
}

func lemonadeNote(sizeGB float64, recipe string, downloaded bool) string {
	status := "available"
	if downloaded {
		status = "downloaded"
	}
	if sizeGB > 0 {
		return fmt.Sprintf("%.2fGB %s %s", sizeGB, recipe, status)
	}
	return fmt.Sprintf("%s %s", recipe, status)
}

// lemonadeLess sorts downloaded models first, then alpha by id.
func lemonadeLess(s []modelChoice) func(i, j int) bool {
	return func(i, j int) bool {
		di := strings.Contains(s[i].Note, "downloaded")
		dj := strings.Contains(s[j].Note, "downloaded")
		if di != dj {
			return di
		}
		return s[i].ID < s[j].ID
	}
}

// pickModel prints a numbered list of choices and reads a 1-based selection from
// stdin. An empty reply keeps the current model.
func pickModel(choices []modelChoice, current string) (string, error) {
	currentIdx := -1
	for i, c := range choices {
		if c.ID == current {
			currentIdx = i
		}
		marker := " "
		if c.ID == current {
			marker = "*"
		}
		if c.Note != "" {
			fmt.Printf("  %s %2d) %s  (%s)\n", marker, i+1, c.ID, c.Note)
		} else {
			fmt.Printf("  %s %2d) %s\n", marker, i+1, c.ID)
		}
	}

	prompt := "Pick [1-%d]"
	if currentIdx >= 0 {
		prompt += fmt.Sprintf(", Enter keeps %s", current)
	}
	fmt.Printf(prompt+": ", len(choices))

	line, err := readLine()
	if err != nil {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		if currentIdx < 0 {
			return "", errors.New("no current model and no choice provided")
		}
		return current, nil
	}
	n, err := strconv.Atoi(line)
	if err != nil {
		return "", fmt.Errorf("invalid choice %q", line)
	}
	if n < 1 || n > len(choices) {
		return "", fmt.Errorf("choice %d out of range [1-%d]", n, len(choices))
	}
	return choices[n-1].ID, nil
}

// stdinReader is shared across all readLine calls so that bytes buffered
// past a newline on one call are still visible to the next call. Creating
// a fresh bufio.Reader each call caused later reads to hit EOF when the
// test harness piped multiple lines on stdin.
var stdinReader = bufio.NewReader(os.Stdin)

// readLine reads a single line from stdin. Unlike fmt.Scanln this tolerates
// empty lines (used to keep the current selection).
func readLine() (string, error) {
	line, err := stdinReader.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
