package transcribe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"vocis/internal/config"
)

func TestEnsureTranscribeModelLoaded_NoopWhenAlreadyLoaded(t *testing.T) {
	t.Parallel()

	stub := newLemonadeStub(t, stubSpec{loaded: []string{"whisper-v3-turbo-FLM"}})
	defer stub.Close()

	onLoadingCount := 0
	err := EnsureTranscribeModelLoaded(context.Background(), config.TranscriptionConfig{
		Backend: config.BackendLemonadeChat,
		Model:   "whisper-v3-turbo-FLM",
		BaseURL: stub.URL + "/api/v1",
	}, func(string) { onLoadingCount++ })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if onLoadingCount != 0 {
		t.Fatalf("onLoading fired %d times, want 0 when model already loaded", onLoadingCount)
	}
	if got := atomic.LoadInt32(&stub.loadRequests); got != 0 {
		t.Fatalf("/load count = %d, want 0 when model already loaded", got)
	}
}

func TestEnsureTranscribeModelLoaded_LoadsWhenMissing(t *testing.T) {
	t.Parallel()

	stub := newLemonadeStub(t, stubSpec{loaded: []string{"gemma4-it-e2b-FLM"}})
	defer stub.Close()

	var gotModel string
	onLoadingCount := 0
	err := EnsureTranscribeModelLoaded(context.Background(), config.TranscriptionConfig{
		Backend: config.BackendLemonadeChat,
		Model:   "whisper-v3-turbo-FLM",
		BaseURL: stub.URL + "/api/v1",
	}, func(m string) {
		onLoadingCount++
		gotModel = m
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if onLoadingCount != 1 {
		t.Fatalf("onLoading fired %d times, want 1", onLoadingCount)
	}
	if gotModel != "whisper-v3-turbo-FLM" {
		t.Fatalf("onLoading model = %q, want whisper-v3-turbo-FLM", gotModel)
	}
	if got := atomic.LoadInt32(&stub.loadRequests); got != 1 {
		t.Fatalf("/load count = %d, want 1", got)
	}
	if got := stub.lastLoadModel(); got != "whisper-v3-turbo-FLM" {
		t.Fatalf("/load received model_name=%q, want whisper-v3-turbo-FLM", got)
	}
}

func TestEnsureTranscribeModelLoaded_PropagatesLoadError(t *testing.T) {
	t.Parallel()

	stub := newLemonadeStub(t, stubSpec{
		loaded:     []string{"gemma4-it-e2b-FLM"},
		loadStatus: http.StatusInternalServerError,
		loadBody:   "oom: cannot fit whisper-v3-turbo",
	})
	defer stub.Close()

	err := EnsureTranscribeModelLoaded(context.Background(), config.TranscriptionConfig{
		Backend: config.BackendLemonadeChat,
		Model:   "whisper-v3-turbo-FLM",
		BaseURL: stub.URL + "/api/v1",
	}, nil)
	if err == nil {
		t.Fatalf("want error from /load failure, got nil")
	}
	if !strings.Contains(err.Error(), "whisper-v3-turbo") {
		t.Fatalf("error %q should mention the model name", err.Error())
	}
}

// TestEnsureTranscribeModelLoaded_ChatAudioUsesLoadEndpoint verifies
// the chat-audio backend hits /load (not /audio/transcriptions, which
// would 4xx for type=llm models like gemma-FLM).
func TestEnsureTranscribeModelLoaded_ChatAudioUsesLoadEndpoint(t *testing.T) {
	t.Parallel()

	stub := newLemonadeStub(t, stubSpec{loaded: []string{"some-other-model"}})
	defer stub.Close()

	onLoadingCount := 0
	err := EnsureTranscribeModelLoaded(context.Background(), config.TranscriptionConfig{
		Backend: config.BackendLemonadeChat,
		Model:   "gemma4-it-e2b-FLM",
		BaseURL: stub.URL + "/api/v1",
	}, func(string) { onLoadingCount++ })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if onLoadingCount != 1 {
		t.Fatalf("onLoading fired %d times, want 1", onLoadingCount)
	}
	if got := atomic.LoadInt32(&stub.loadRequests); got != 1 {
		t.Fatalf("loadRequests=%d, want 1", got)
	}
	if got := stub.lastLoadModel(); got != "gemma4-it-e2b-FLM" {
		t.Fatalf("/load received model_name=%q, want gemma4-it-e2b-FLM", got)
	}
}

type stubSpec struct {
	loaded     []string
	loadStatus int
	loadBody   string
}

type lemonadeStub struct {
	*httptest.Server
	loadRequests int32
	lastLoadName atomic.Value
}

func (s *lemonadeStub) lastLoadModel() string {
	v := s.lastLoadName.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}

// newLemonadeStub fakes the subset of the Lemonade REST API the
// preflight touches: GET /api/v1/health for the loaded-models list
// and POST /api/v1/load to force a model into a slot.
func newLemonadeStub(t *testing.T, spec stubSpec) *lemonadeStub {
	t.Helper()

	state := &lemonadeStub{}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		loaded := make([]LemonadeLoadedModel, 0, len(spec.loaded))
		for _, name := range spec.loaded {
			loaded = append(loaded, LemonadeLoadedModel{Name: name, Type: "audio"})
		}
		body := map[string]any{
			"status":            "ok",
			"all_models_loaded": loaded,
			"max_models":        map[string]int{"audio": 1},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/api/v1/load", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&state.loadRequests, 1)
		var body struct {
			ModelName string `json:"model_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		state.lastLoadName.Store(body.ModelName)
		if spec.loadStatus != 0 && spec.loadStatus != http.StatusOK {
			w.WriteHeader(spec.loadStatus)
			_, _ = w.Write([]byte(spec.loadBody))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success"}`))
	})
	state.Server = httptest.NewServer(mux)
	return state
}
