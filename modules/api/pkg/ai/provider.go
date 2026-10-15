// Copyright 2017 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultBaseURLs holds known default base URLs for each provider.
var defaultBaseURLs = map[ProviderKind]string{
	ProviderOpenAI:    "https://api.openai.com/v1",
	ProviderGemini:    "https://generativelanguage.googleapis.com/v1beta",
	ProviderAnthropic: "https://api.anthropic.com/v1",
	ProviderGrok:      "https://api.x.ai/v1",
	ProviderOllama:    "http://localhost:11434/v1",
}

// ProviderAdapter is a universal interface for all AI providers.
type ProviderAdapter interface {
	Chat(ctx context.Context, systemPrompt, userPrompt string) (string, error)
	ListModels(ctx context.Context) ([]string, error)
	Ping(ctx context.Context) (int64, error) // returns latency ms
}

// NewAdapter creates a provider adapter from the given settings.
func NewAdapter(settings *AISettings) (ProviderAdapter, error) {
	baseURL := settings.BaseURL
	if baseURL == "" {
		if u, ok := defaultBaseURLs[settings.Provider]; ok {
			baseURL = u
		}
	}
	switch settings.Provider {
	case ProviderGemini:
		return &geminiAdapter{apiKey: settings.APIKey, baseURL: baseURL, model: settings.Model}, nil
	case ProviderAnthropic:
		return &anthropicAdapter{apiKey: settings.APIKey, baseURL: baseURL, model: settings.Model}, nil
	default:
		// OpenAI-compatible: covers OpenAI, Grok, Ollama, Custom
		return &openAIAdapter{apiKey: settings.APIKey, baseURL: baseURL, model: settings.Model}, nil
	}
}

// NewAdapterFromRequest creates a provider adapter from a VerifyRequest (for one-time tests).
func NewAdapterFromRequest(req *VerifyRequest) (ProviderAdapter, error) {
	return NewAdapter(&AISettings{
		Provider: req.Provider,
		BaseURL:  req.BaseURL,
		APIKey:   req.APIKey,
		Model:    req.Model,
	})
}

// VerifyConnection tests connectivity to an AI provider and returns latency and supported models.
func VerifyConnection(ctx context.Context, req *VerifyRequest) *VerifyResponse {
	adapter, err := NewAdapterFromRequest(req)
	if err != nil {
		return &VerifyResponse{ErrorMsg: err.Error()}
	}

	latencyMs, err := adapter.Ping(ctx)
	if err != nil {
		return &VerifyResponse{ErrorMsg: err.Error()}
	}

	models, _ := adapter.ListModels(ctx)
	return &VerifyResponse{
		Connected: true,
		LatencyMs: latencyMs,
		Models:    models,
	}
}

// ============================================================
// OpenAI-Compatible Adapter (OpenAI, Grok, Ollama, Custom)
// ============================================================

type openAIAdapter struct {
	apiKey  string
	baseURL string
	model   string
}

type openAIChatRequest struct {
	Model    string              `json:"model"`
	Messages []openAIMessage     `json:"messages"`
	MaxTokens int                `json:"max_tokens"`
}
type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type openAIChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}
type openAIModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func (a *openAIAdapter) Chat(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	model := a.model
	if model == "" {
		model = "gpt-4o-mini"
	}
	reqBody, _ := json.Marshal(openAIChatRequest{
		Model: model,
		Messages: []openAIMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		MaxTokens: 2048,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", a.baseURL+"/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	if a.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r openAIChatResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("parse error: %s", string(body))
	}
	if r.Error != nil {
		return "", fmt.Errorf("provider error: %s", r.Error.Message)
	}
	if len(r.Choices) == 0 {
		return "", fmt.Errorf("no response from provider")
	}
	return r.Choices[0].Message.Content, nil
}

func (a *openAIAdapter) ListModels(ctx context.Context) ([]string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", a.baseURL+"/models", nil)
	if a.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r openAIModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(r.Data))
	for _, m := range r.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

func (a *openAIAdapter) Ping(ctx context.Context) (int64, error) {
	start := time.Now()
	model := a.model
	if model == "" {
		model = "gpt-4o-mini"
	}
	reqBody, _ := json.Marshal(openAIChatRequest{
		Model: model,
		Messages: []openAIMessage{
			{Role: "user", Content: "ping"},
		},
		MaxTokens: 1,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", a.baseURL+"/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	if a.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return time.Since(start).Milliseconds(), nil
}

// ============================================================
// Google Gemini Adapter
// ============================================================

type geminiAdapter struct {
	apiKey  string
	baseURL string
	model   string
}

type geminiRequest struct {
	Contents []geminiContent `json:"contents"`
}
type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}
type geminiPart struct {
	Text string `json:"text"`
}
type geminiResponse struct {
	Candidates []struct {
		Content geminiContent `json:"content"`
	} `json:"candidates"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}
type geminiModelsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

func (a *geminiAdapter) Chat(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	model := a.model
	if model == "" {
		model = "gemini-1.5-flash"
	}
	reqBody, _ := json.Marshal(geminiRequest{
		Contents: []geminiContent{
			{Role: "user", Parts: []geminiPart{{Text: systemPrompt + "\n\n" + userPrompt}}},
		},
	})
	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", a.baseURL, model, a.apiKey)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var r geminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	if r.Error != nil {
		return "", fmt.Errorf("gemini error: %s", r.Error.Message)
	}
	if len(r.Candidates) == 0 || len(r.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("no response from Gemini")
	}
	return r.Candidates[0].Content.Parts[0].Text, nil
}

func (a *geminiAdapter) ListModels(ctx context.Context) ([]string, error) {
	url := fmt.Sprintf("%s/models?key=%s", a.baseURL, a.apiKey)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r geminiModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(r.Models))
	for _, m := range r.Models {
		// strip prefix "models/"
		name := strings.TrimPrefix(m.Name, "models/")
		names = append(names, name)
	}
	return names, nil
}

func (a *geminiAdapter) Ping(ctx context.Context) (int64, error) {
	start := time.Now()
	model := a.model
	if model == "" {
		model = "gemini-1.5-flash"
	}
	reqBody, _ := json.Marshal(geminiRequest{
		Contents: []geminiContent{
			{Role: "user", Parts: []geminiPart{{Text: "ping"}}},
		},
	})
	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", a.baseURL, model, a.apiKey)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return time.Since(start).Milliseconds(), nil
}

// ============================================================
// Anthropic Claude Adapter
// ============================================================

type anthropicAdapter struct {
	apiKey  string
	baseURL string
	model   string
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
}
type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type anthropicResponse struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (a *anthropicAdapter) Chat(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	model := a.model
	if model == "" {
		model = "claude-3-5-sonnet-20241022"
	}
	reqBody, _ := json.Marshal(anthropicRequest{
		Model:     model,
		MaxTokens: 2048,
		System:    systemPrompt,
		Messages: []anthropicMessage{
			{Role: "user", Content: userPrompt},
		},
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", a.baseURL+"/messages", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var r anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	if r.Error != nil {
		return "", fmt.Errorf("anthropic error: %s", r.Error.Message)
	}
	if len(r.Content) == 0 {
		return "", fmt.Errorf("no response from Anthropic")
	}
	return r.Content[0].Text, nil
}

func (a *anthropicAdapter) ListModels(ctx context.Context) ([]string, error) {
	// Anthropic does not have a public models list endpoint; return known models.
	return []string{
		"claude-3-5-sonnet-20241022",
		"claude-3-5-haiku-20241022",
		"claude-3-opus-20240229",
		"claude-3-sonnet-20240229",
		"claude-3-haiku-20240307",
	}, nil
}

func (a *anthropicAdapter) Ping(ctx context.Context) (int64, error) {
	start := time.Now()
	model := a.model
	if model == "" {
		model = "claude-3-5-haiku-20241022"
	}
	reqBody, _ := json.Marshal(anthropicRequest{
		Model:     model,
		MaxTokens: 1,
		System:    "You are a test agent.",
		Messages:  []anthropicMessage{{Role: "user", Content: "ping"}},
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", a.baseURL+"/messages", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return time.Since(start).Milliseconds(), nil
}
