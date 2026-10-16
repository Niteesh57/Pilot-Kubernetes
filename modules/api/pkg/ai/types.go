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

// ProviderKind enumerates supported AI providers.
type ProviderKind string

const (
	ProviderOpenAI    ProviderKind = "openai"
	ProviderGemini    ProviderKind = "gemini"
	ProviderAnthropic ProviderKind = "anthropic"
	ProviderGrok      ProviderKind = "grok"
	ProviderOllama    ProviderKind = "ollama"
	ProviderCustom    ProviderKind = "custom"
)

// AISettings holds the active AI provider configuration, persisted in a K8s Secret.
type AISettings struct {
	Provider    ProviderKind `json:"provider"`
	BaseURL     string       `json:"baseUrl"`
	APIKey      string       `json:"apiKey,omitempty"`
	Model       string       `json:"model"`
	MaxTokens   int          `json:"maxTokens"`
	Temperature float64      `json:"temperature"`
	// FalkorDB connection
	FalkorDBAddr      string `json:"falkordbAddr"`
	FalkorDBPassword  string `json:"falkordbPassword,omitempty"`
	FalkorDBGraphName string `json:"falkordbGraphName"`
}

// AISettingsResponse is returned to the frontend — API keys masked.
type AISettingsResponse struct {
	Provider          ProviderKind `json:"provider"`
	BaseURL           string       `json:"baseUrl"`
	APIKeyMasked      string       `json:"apiKeyMasked"`
	Model             string       `json:"model"`
	MaxTokens         int          `json:"maxTokens"`
	Temperature       float64      `json:"temperature"`
	FalkorDBAddr      string       `json:"falkordbAddr"`
	FalkorDBGraphName string       `json:"falkordbGraphName"`
}

// VerifyRequest is sent by the frontend to test a provider connection.
type VerifyRequest struct {
	Provider ProviderKind `json:"provider"`
	BaseURL  string       `json:"baseUrl"`
	APIKey   string       `json:"apiKey"`
	Model    string       `json:"model"`
}

// VerifyResponse reports the result of a provider connectivity test.
type VerifyResponse struct {
	Connected bool     `json:"connected"`
	LatencyMs int64    `json:"latencyMs"`
	Models    []string `json:"models"`
	ErrorMsg  string   `json:"errorMsg,omitempty"`
}

// AnalystRequest is used to trigger RCA for a specific resource.
type AnalystRequest struct {
	Namespace     string `json:"namespace"`
	Kind          string `json:"kind"` // Pod, Deployment, StatefulSet, etc.
	Name          string `json:"name"`
	ContainerName string `json:"containerName,omitempty"`
	CustomQuery   string `json:"customQuery,omitempty"`
}

// AnalystResponse returns a structured root cause analysis.
type AnalystResponse struct {
	Summary     string   `json:"summary"`
	RootCause   string   `json:"rootCause"`
	Evidence    string   `json:"evidence"`
	Remediation []string `json:"remediation"`
	RawAnalysis string   `json:"rawAnalysis"`
}

// ImprovementFinding represents a single audit finding.
type ImprovementFinding struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Category  string `json:"category"` // reliability, security, resources
	Severity  string `json:"severity"` // critical, warning, info
	Issue     string `json:"issue"`
	YAMLPatch string `json:"yamlPatch,omitempty"`
}

// ImprovementResponse holds the full cluster audit result.
type ImprovementResponse struct {
	TotalScanned int                  `json:"totalScanned"`
	Findings     []ImprovementFinding `json:"findings"`
	AISummary    string               `json:"aiSummary,omitempty"`
}

// SecurityFinding describes a single security risk.
type SecurityFinding struct {
	Kind      string `json:"kind"` // Role, ClusterRole, Pod, Deployment, etc.
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Category  string `json:"category"` // rbac, workload, network
	Severity  string `json:"severity"` // critical, high, medium, low
	Issue     string `json:"issue"`
	Details   string `json:"details"`
	YAMLPatch string `json:"yamlPatch,omitempty"`
}

// SecurityResponse holds the cluster security audit result.
type SecurityResponse struct {
	Score       int               `json:"score"` // 0–100
	Findings    []SecurityFinding `json:"findings"`
	AttackPaths []string          `json:"attackPaths,omitempty"`
	AISummary   string            `json:"aiSummary,omitempty"`
}

// AgentMessage is an exchange item in conversational multi-turn chat.
type AgentMessage struct {
	Role    string `json:"role"` // "user" or "assistant"
	Content string `json:"content"`
}

// AgentRequest is submitted by the user to the Autonomous SRE Problem Solver.
type AgentRequest struct {
	Namespace string         `json:"namespace,omitempty"`
	Query     string         `json:"query"`
	History   []AgentMessage `json:"history,omitempty"`
}

// ResourceItem represents a resource for @mention autocomplete.
type ResourceItem struct {
	Kind      string `json:"kind"` // Deployment, Pod, Service
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Status    string `json:"status,omitempty"`
}

// ResourcesResponse returns resources for @mention autocomplete suggestions.
type ResourcesResponse struct {
	Deployments []ResourceItem `json:"deployments"`
	Pods        []ResourceItem `json:"pods"`
	Services    []ResourceItem `json:"services"`
}

// AgentStep describes a single investigation step taken by the agent.
type AgentStep struct {
	Step        string `json:"step"`
	Description string `json:"description"`
	Output      string `json:"output,omitempty"`
}

// AgentResponse is the final output from the Autonomous SRE Agent.
type AgentResponse struct {
	Query        string      `json:"query"`
	Steps        []AgentStep `json:"steps"`
	Summary      string      `json:"summary"`
	RootCause    string      `json:"rootCause,omitempty"`
	Commands     []string    `json:"commands,omitempty"`
	YAMLPatches  []string    `json:"yamlPatches,omitempty"`
	GraphContext string      `json:"graphContext,omitempty"`
}
