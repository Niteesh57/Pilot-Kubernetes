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
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const agentSystemPrompt = `You are an expert autonomous Kubernetes SRE agent.
You have access to cluster information, logs, events, and topology data provided to you.
Your goal is to answer the engineer's question or solve the operational problem thoroughly.
Structure your response as JSON with these fields:
- summary: Clear 2-3 sentence answer to the question.
- rootCause: If applicable, the specific technical root cause.
- commands: Array of exact kubectl commands to investigate further or fix the issue.
- yamlPatches: Array of YAML patch strings for any necessary resource changes.
Return ONLY valid JSON, no other text.`

// RunAgentQuery executes the autonomous SRE agent investigation.
func RunAgentQuery(ctx context.Context, k8sClient kubernetes.Interface, settings *AISettings, req *AgentRequest) (*AgentResponse, error) {
	steps := []AgentStep{}
	contextData := &strings.Builder{}

	// Step 1: Resolve namespace context
	step1 := AgentStep{Step: "1", Description: "Resolving cluster context"}
	namespace := req.Namespace
	if namespace == "" {
		namespace = "default"
	}
	contextData.WriteString(fmt.Sprintf("Target Namespace: %s\nQuery: %s\n\n", namespace, req.Query))
	step1.Output = fmt.Sprintf("Targeting namespace: %s", namespace)
	steps = append(steps, step1)

	// Step 2: Gather cluster state relevant to query
	step2 := AgentStep{Step: "2", Description: "Gathering cluster state for " + namespace}
	clusterInfo, _ := gatherClusterStateForQuery(ctx, k8sClient, namespace, req.Query)
	contextData.WriteString(clusterInfo)
	step2.Output = fmt.Sprintf("Gathered %d bytes of cluster context", len(clusterInfo))
	steps = append(steps, step2)

	// Step 3: Identify problematic resources
	step3 := AgentStep{Step: "3", Description: "Identifying problematic resources"}
	problemContext, _ := gatherProblematicResources(ctx, k8sClient, namespace)
	contextData.WriteString(problemContext)
	step3.Output = problemContext
	steps = append(steps, step3)

	// Step 4: AI synthesis
	step4 := AgentStep{Step: "4", Description: "Synthesizing findings with AI"}
	response := &AgentResponse{
		Query:        req.Query,
		Steps:        steps,
		GraphContext: "",
	}

	adapter, err := NewAdapter(settings)
	if err != nil {
		step4.Output = fmt.Sprintf("AI provider not configured (%v). Presenting live cluster diagnostics.", err)
		steps = append(steps, step4)
		response.Summary = fmt.Sprintf("Cluster diagnostics gathered for '%s' in namespace '%s'. Configure an AI Provider in AI Settings for automated synthesis.", req.Query, namespace)
		response.Commands = []string{
			fmt.Sprintf("kubectl get pods -n %s", namespace),
			fmt.Sprintf("kubectl get events -n %s --sort-by='.metadata.creationTimestamp'", namespace),
		}
		response.Steps = steps
		return response, nil
	}

	fullPrompt := fmt.Sprintf("Engineer's Question: %s\n\nCluster Context:\n%s", req.Query, contextData.String())
	rawResponse, err := adapter.Chat(ctx, agentSystemPrompt, fullPrompt)
	if err != nil {
		step4.Output = fmt.Sprintf("AI synthesis note: %v. Live diagnostics gathered above.", err)
		steps = append(steps, step4)
		response.Summary = fmt.Sprintf("Gathered live diagnostic state for '%s' in namespace '%s'. Note: The configured AI Provider returned: %v. Please verify your API Key and Model in AI Settings.", req.Query, namespace, err)
		response.Commands = []string{
			fmt.Sprintf("kubectl get pods -n %s", namespace),
			fmt.Sprintf("kubectl get events -n %s --sort-by='.metadata.creationTimestamp'", namespace),
		}
		response.Steps = steps
		return response, nil
	}
	step4.Output = "AI analysis complete"
	steps = append(steps, step4)
	response.Steps = steps

	cleaned := strings.TrimSpace(rawResponse)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	cleaned = strings.TrimSpace(cleaned)

	var parsed struct {
		Summary     string   `json:"summary"`
		RootCause   string   `json:"rootCause"`
		Commands    []string `json:"commands"`
		YAMLPatches []string `json:"yamlPatches"`
	}
	if err := parseJSON(cleaned, &parsed); err == nil {
		response.Summary = parsed.Summary
		response.RootCause = parsed.RootCause
		response.Commands = parsed.Commands
		response.YAMLPatches = parsed.YAMLPatches
	} else {
		response.Summary = rawResponse
	}

	return response, nil
}

// gatherClusterStateForQuery collects relevant cluster state for the SRE agent.
func gatherClusterStateForQuery(ctx context.Context, k8sClient kubernetes.Interface, namespace, query string) (string, error) {
	var sb strings.Builder

	// Node status
	nodes, _ := k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if nodes != nil {
		sb.WriteString("=== NODES ===\n")
		for _, node := range nodes.Items {
			ready := "NotReady"
			for _, cond := range node.Status.Conditions {
				if cond.Type == "Ready" && cond.Status == "True" {
					ready = "Ready"
				}
			}
			sb.WriteString(fmt.Sprintf("  %s: %s\n", node.Name, ready))
		}
	}

	// Pod states
	pods, _ := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if pods != nil {
		sb.WriteString(fmt.Sprintf("\n=== PODS in %s ===\n", namespace))
		for _, pod := range pods.Items {
			restarts := int32(0)
			waiting := ""
			for _, cs := range pod.Status.ContainerStatuses {
				restarts += cs.RestartCount
				if cs.State.Waiting != nil {
					waiting = cs.State.Waiting.Reason
				}
			}
			line := fmt.Sprintf("  %s: Phase=%s, Restarts=%d", pod.Name, pod.Status.Phase, restarts)
			if waiting != "" {
				line += fmt.Sprintf(", Waiting=%s", waiting)
			}
			sb.WriteString(line + "\n")
		}
	}

	// Recent events (Warning only)
	events, _ := k8sClient.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "type=Warning",
	})
	if events != nil && len(events.Items) > 0 {
		sb.WriteString(fmt.Sprintf("\n=== RECENT WARNING EVENTS in %s ===\n", namespace))
		count := 0
		for _, ev := range events.Items {
			sb.WriteString(fmt.Sprintf("  [%s] %s/%s: %s\n", ev.Reason, ev.InvolvedObject.Kind, ev.InvolvedObject.Name, ev.Message))
			count++
			if count >= 20 {
				break
			}
		}
	}

	// Deployments summary
	deployments, _ := k8sClient.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if deployments != nil {
		sb.WriteString(fmt.Sprintf("\n=== DEPLOYMENTS in %s ===\n", namespace))
		for _, d := range deployments.Items {
			sb.WriteString(fmt.Sprintf("  %s: Desired=%d, Ready=%d, Available=%d\n",
				d.Name, *d.Spec.Replicas, d.Status.ReadyReplicas, d.Status.AvailableReplicas))
		}
	}

	// Services
	services, _ := k8sClient.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	if services != nil {
		sb.WriteString(fmt.Sprintf("\n=== SERVICES in %s ===\n", namespace))
		for _, svc := range services.Items {
			sb.WriteString(fmt.Sprintf("  %s: Type=%s\n", svc.Name, svc.Spec.Type))
		}
	}

	return sb.String(), nil
}

// gatherProblematicResources finds failing/problematic resources in the namespace.
func gatherProblematicResources(ctx context.Context, k8sClient kubernetes.Interface, namespace string) (string, error) {
	var sb strings.Builder
	sb.WriteString("\n=== PROBLEMATIC RESOURCES ===\n")

	pods, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}

	found := false
	for _, pod := range pods.Items {
		for _, cs := range pod.Status.ContainerStatuses {
			isProblem := false
			reason := ""
			if cs.RestartCount > 5 {
				isProblem = true
				reason = fmt.Sprintf("high restart count (%d)", cs.RestartCount)
			}
			if cs.State.Waiting != nil && (cs.State.Waiting.Reason == "CrashLoopBackOff" || cs.State.Waiting.Reason == "Error" || cs.State.Waiting.Reason == "OOMKilled") {
				isProblem = true
				reason = cs.State.Waiting.Reason
			}
			if isProblem {
				found = true
				sb.WriteString(fmt.Sprintf("  Pod %s/%s: %s\n", pod.Name, cs.Name, reason))
			}
		}
	}
	if !found {
		sb.WriteString("  No obviously failing resources found.\n")
	}

	return sb.String(), nil
}
