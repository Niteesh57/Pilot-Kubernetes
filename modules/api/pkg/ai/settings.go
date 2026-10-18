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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"k8s.io/klog/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dashboard/api/pkg/args"
)

const aiSecretName = "kubepilot-ai-settings"

var (
	cachedSettingsMu sync.RWMutex
	cachedSettings   *AISettings
)

// getAISecretNamespace returns the namespace where the settings secret is stored.
func getAISecretNamespace() string {
	if ns := args.Namespace(); ns != "" {
		return ns
	}
	if env := os.Getenv("POD_NAMESPACE"); env != "" {
		return env
	}
	return "kubernetes-dashboard"
}

// defaultAISettings returns safe, minimal defaults.
func defaultAISettings() *AISettings {
	ns := getAISecretNamespace()
	return &AISettings{
		Provider:          ProviderOpenAI,
		BaseURL:           "https://api.openai.com/v1",
		Model:             "gpt-4o-mini",
		MaxTokens:         2048,
		Temperature:       0.3,
		FalkorDBAddr:      fmt.Sprintf("falkordb.%s.svc.cluster.local:6379", ns),
		FalkorDBGraphName: "kubepilot",
	}
}

// LoadAISettings loads AI settings from the cluster Secret or in-memory cache / env vars.
func LoadAISettings(client kubernetes.Interface) *AISettings {
	cachedSettingsMu.RLock()
	var inMemoryCopy *AISettings
	if cachedSettings != nil {
		c := *cachedSettings
		inMemoryCopy = &c
	}
	cachedSettingsMu.RUnlock()

	settings := defaultAISettings()

	// Candidate namespaces to check for the secret
	candidateNamespaces := []string{getAISecretNamespace(), "kubernetes-dashboard", "default", "kube-system"}
	seen := make(map[string]bool)
	var foundSecret *corev1.Secret

	if client != nil {
		for _, ns := range candidateNamespaces {
			if ns == "" || seen[ns] {
				continue
			}
			seen[ns] = true
			s, err := client.CoreV1().Secrets(ns).Get(context.Background(), aiSecretName, metav1.GetOptions{})
			if err == nil && s != nil {
				foundSecret = s
				break
			}
		}
	}

	if foundSecret != nil {
		if data, ok := foundSecret.Data["settings.json"]; ok {
			if err := json.Unmarshal(data, settings); err != nil {
				klog.ErrorS(err, "Failed to parse AI settings secret")
			}
		}
	} else if inMemoryCopy != nil {
		settings = inMemoryCopy
	}

	settings.APIKey = cleanAPIKey(settings.APIKey)

	// If API key is still missing, fallback to inMemoryCopy if available
	if settings.APIKey == "" && inMemoryCopy != nil && inMemoryCopy.APIKey != "" {
		settings.APIKey = inMemoryCopy.APIKey
	}

	// Environment variable fallback
	if settings.APIKey == "" {
		switch settings.Provider {
		case ProviderOpenAI:
			if k := os.Getenv("OPENAI_API_KEY"); k != "" {
				settings.APIKey = cleanAPIKey(k)
			}
		case ProviderGemini:
			if k := os.Getenv("GEMINI_API_KEY"); k != "" {
				settings.APIKey = cleanAPIKey(k)
			}
		case ProviderAnthropic:
			if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
				settings.APIKey = cleanAPIKey(k)
			}
		}
		if settings.APIKey == "" {
			if k := os.Getenv("AI_API_KEY"); k != "" {
				settings.APIKey = cleanAPIKey(k)
			}
		}
	}

	return settings
}

var envPrefixRegex = regexp.MustCompile(`^[A-Za-z0-9_]+=\s*(.*)$`)

// cleanAPIKey strips any leading env variable name prefix (e.g. OPENAI_API_KEY=xxx) and surrounding quotes.
func cleanAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if m := envPrefixRegex.FindStringSubmatch(key); len(m) > 1 {
		key = strings.TrimSpace(m[1])
	}
	return strings.Trim(key, `"' `)
}

// SaveAISettings persists AI settings to in-memory cache and to the cluster Secret.
func SaveAISettings(client kubernetes.Interface, settings *AISettings) error {
	settings.APIKey = cleanAPIKey(settings.APIKey)

	// Update in-memory cache immediately
	cachedSettingsMu.Lock()
	c := *settings
	cachedSettings = &c
	cachedSettingsMu.Unlock()

	if client == nil {
		return nil
	}

	data, err := json.Marshal(settings)
	if err != nil {
		return err
	}

	targetNs := getAISecretNamespace()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      aiSecretName,
			Namespace: targetNs,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kubepilot",
			},
		},
		Data: map[string][]byte{
			"settings.json": data,
		},
	}

	// Try update first, then create.
	_, err = client.CoreV1().Secrets(targetNs).Update(
		context.Background(), secret, metav1.UpdateOptions{},
	)
	if err != nil {
		_, err = client.CoreV1().Secrets(targetNs).Create(
			context.Background(), secret, metav1.CreateOptions{},
		)
	}
	return err
}

// MaskSettings returns a copy with the API key masked for frontend consumption.
func MaskSettings(s *AISettings) *AISettingsResponse {
	masked := ""
	if len(s.APIKey) > 4 {
		masked = strings.Repeat("•", len(s.APIKey)-4) + s.APIKey[len(s.APIKey)-4:]
	} else if s.APIKey != "" {
		masked = "••••"
	}
	return &AISettingsResponse{
		Provider:          s.Provider,
		BaseURL:           s.BaseURL,
		APIKeyMasked:      masked,
		Model:             s.Model,
		MaxTokens:         s.MaxTokens,
		Temperature:       s.Temperature,
		FalkorDBAddr:      s.FalkorDBAddr,
		FalkorDBGraphName: s.FalkorDBGraphName,
	}
}
