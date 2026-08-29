package fixtures_test

import (
	"io"
	"net/http"
	"testing"

	"github.com/boop-dev/boop/tests/fixtures"
)

func TestOllamaTags(t *testing.T) {
	srv := fixtures.NewServer(t)

	var out struct {
		Models []struct {
			Name       string `json:"name"`
			Model      string `json:"model"`
			Size       int64  `json:"size"`
			Digest     string `json:"digest"`
			ModifiedAt string `json:"modified_at"`
			Details    struct {
				Format            string   `json:"format"`
				Family            string   `json:"family"`
				Families          []string `json:"families"`
				ParameterSize     string   `json:"parameter_size"`
				QuantizationLevel string   `json:"quantization_level"`
				ContextLength     int      `json:"context_length"`
				EmbeddingLength   int      `json:"embedding_length"`
			} `json:"details"`
			Capabilities []string `json:"capabilities"`
		} `json:"models"`
	}
	decode(t, get(t, srv, "/api/tags"), &out)

	if len(out.Models) != 2 {
		t.Fatalf("models = %d, want 2", len(out.Models))
	}
	m := out.Models[0]
	if m.Name != "boop-test-model" || m.Model != m.Name {
		t.Errorf("name/model = %q/%q", m.Name, m.Model)
	}
	if m.Size == 0 || m.Digest == "" || m.ModifiedAt == "" {
		t.Errorf("model metadata incomplete: %+v", m)
	}
	// Capability detection depends on these two living in different places.
	if m.Details.ContextLength != 8192 {
		t.Errorf("details.context_length = %d, want 8192", m.Details.ContextLength)
	}
	if m.Details.EmbeddingLength != 4096 {
		t.Errorf("details.embedding_length = %d", m.Details.EmbeddingLength)
	}
	if len(m.Capabilities) != 2 || m.Capabilities[1] != "tools" {
		t.Errorf("top-level capabilities = %v", m.Capabilities)
	}
	if m.Details.Format != "gguf" || m.Details.Family != "llama" ||
		len(m.Details.Families) != 1 || m.Details.ParameterSize != "8.0B" ||
		m.Details.QuantizationLevel != "Q4_K_M" {
		t.Errorf("details = %+v", m.Details)
	}
}

func TestOllamaShow(t *testing.T) {
	srv := fixtures.NewServer(t)

	var out struct {
		Modelfile string `json:"modelfile"`
		Details   struct {
			Family        string `json:"family"`
			ContextLength int    `json:"context_length"`
		} `json:"details"`
		ModelInfo    map[string]any `json:"model_info"`
		Capabilities []string       `json:"capabilities"`
	}
	decode(t, post(t, srv, "/api/show", `{"model":"boop-test-vision"}`), &out)

	if out.Details.Family != "llama" || out.Details.ContextLength != 32768 {
		t.Errorf("details = %+v", out.Details)
	}
	if got, ok := out.ModelInfo["llama.context_length"]; !ok || got.(float64) != 32768 {
		t.Errorf("model_info = %+v", out.ModelInfo)
	}
	if len(out.Capabilities) != 3 || out.Capabilities[2] != "vision" {
		t.Errorf("capabilities = %v", out.Capabilities)
	}

	// "name" is the older field spelling and must work too.
	resp := post(t, srv, "/api/show", `{"name":"boop-test-model"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf(`{"name":...} form: status = %d`, resp.StatusCode)
	}

	missing := post(t, srv, "/api/show", `{"model":"nope"}`)
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown model status = %d, want 404", missing.StatusCode)
	}
	var errOut struct {
		Error string `json:"error"`
	}
	decode(t, missing, &errOut)
	if errOut.Error != `model 'nope' not found` {
		t.Errorf("error = %q", errOut.Error)
	}
}

func TestLMStudioNativeModels(t *testing.T) {
	srv := fixtures.NewServer(t)

	var out struct {
		Data []struct {
			ID               string `json:"id"`
			Type             string `json:"type"`
			State            string `json:"state"`
			Quantization     string `json:"quantization"`
			MaxContextLength int    `json:"max_context_length"`
		} `json:"data"`
	}
	decode(t, get(t, srv, "/api/v0/models"), &out)

	if len(out.Data) != 2 {
		t.Fatalf("data = %+v", out.Data)
	}
	if out.Data[0].State != "loaded" || out.Data[1].State != "not-loaded" {
		t.Errorf("states = %q/%q", out.Data[0].State, out.Data[1].State)
	}
	if out.Data[0].Type != "llm" || out.Data[1].Type != "vlm" {
		t.Errorf("types = %q/%q", out.Data[0].Type, out.Data[1].Type)
	}
	if out.Data[0].MaxContextLength != 8192 || out.Data[0].Quantization != "Q4_K_M" {
		t.Errorf("metadata = %+v", out.Data[0])
	}
}

func TestLemonadeLifecycleFlipsLoadState(t *testing.T) {
	srv := fixtures.NewServer(t)

	load := post(t, srv, "/api/v1/load", `{"model_name":"boop-test-vision"}`)
	load.Body.Close()
	if load.StatusCode != http.StatusOK {
		t.Fatalf("load status = %d", load.StatusCode)
	}

	var out struct {
		Data []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"data"`
	}
	decode(t, get(t, srv, "/api/v0/models"), &out)
	if out.Data[1].State != "loaded" {
		t.Errorf("state after load = %q", out.Data[1].State)
	}

	unload := post(t, srv, "/api/v1/unload", `{"model_name":"boop-test-vision"}`)
	unload.Body.Close()
	decode(t, get(t, srv, "/api/v0/models"), &out)
	if out.Data[1].State != "not-loaded" {
		t.Errorf("state after unload = %q", out.Data[1].State)
	}

	missing := post(t, srv, "/api/v1/load", `{"model_name":"nope"}`)
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("unknown model status = %d, want 404", missing.StatusCode)
	}

	if n := len(srv.RequestsTo("/api/v1/load")); n != 2 {
		t.Errorf("lifecycle requests captured = %d, want 2", n)
	}
}

func TestHealthEndpoints(t *testing.T) {
	srv := fixtures.NewServer(t)
	for _, path := range []string{"/health", "/api/health", "/api/v1/health"} {
		var out struct {
			Status string `json:"status"`
		}
		decode(t, get(t, srv, path), &out)
		if out.Status != "ok" {
			t.Errorf("%s: status = %q", path, out.Status)
		}
	}

	root := get(t, srv, "/")
	defer root.Body.Close()
	body, err := io.ReadAll(root.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "Ollama is running" {
		t.Errorf("root body = %q", body)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	srv := fixtures.NewServer(t)
	resp := get(t, srv, "/v1/nope")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
