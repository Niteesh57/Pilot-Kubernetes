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
	"strings"

	"k8s.io/klog/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dashboard/api/pkg/args"
)

const aiSecretName = "kubepilot-ai-settings"

var aiSecretNamespace = args.Namespace()

// defaultAISettings returns safe, minimal defaults.
func defaultAISettings() *AISettings {
	namespace := args.Namespace()
	return &AISettings{
		Provider:          ProviderOpenAI,
		BaseURL:           "https://api.openai.com/v1",
		Model:             "gpt-4o-mini",
		MaxTokens:         2048,
		Temperature:       0.3,
		FalkorDBAddr:      fmt.Sprintf("falkordb.%s.svc.cluster.local:6379", namespace),
		FalkorDBGraphName: "kubepilot",
	}
}

// LoadAISettings loads AI settings from the cluster Secret (falls back to defaults).
func LoadAISettings(client kubernetes.Interface) *AISettings {
	secret, err := client.CoreV1().Secrets(aiSecretNamespace).Get(
		context.Background(), aiSecretName, metav1.GetOptions{},
	)
	if err != nil {
		klog.V(3).InfoS("AI settings secret not found, using defaults", "error", err)
		return defaultAISettings()
	}

	settings := defaultAISettings()
	if data, ok := secret.Data["settings.json"]; ok {
		if err := json.Unmarshal(data, settings); err != nil {
			klog.ErrorS(err, "Failed to parse AI settings secret")
		}
	}
	settings.APIKey = cleanAPIKey(settings.APIKey)
	return settings
}

// cleanAPIKey strips any env variable prefix (e.g. NVIDIA_API_KEY= or GROQ_API_KEY=) and quotes.
func cleanAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if idx := strings.Index(key, "="); idx != -1 {
		key = strings.TrimSpace(key[idx+1:])
	}
	return strings.Trim(key, `"' `)
}

// SaveAISettings persists AI settings to the cluster Secret.
func SaveAISettings(client kubernetes.Interface, settings *AISettings) error {
	settings.APIKey = cleanAPIKey(settings.APIKey)
	data, err := json.Marshal(settings)
	if err != nil {
		return err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      aiSecretName,
			Namespace: aiSecretNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kubepilot",
			},
		},
		Data: map[string][]byte{
			"settings.json": data,
		},
	}

	// Try update first, then create.
	_, err = client.CoreV1().Secrets(aiSecretNamespace).Update(
		context.Background(), secret, metav1.UpdateOptions{},
	)
	if err != nil {
		_, err = client.CoreV1().Secrets(aiSecretNamespace).Create(
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
