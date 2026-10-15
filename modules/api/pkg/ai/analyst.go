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
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const analystSystemPrompt = `You are an expert Kubernetes Site Reliability Engineer (SRE) and DevOps specialist.
Your task is to perform root cause analysis (RCA) for the Kubernetes resource provided.
Analyze the logs, events, and resource status carefully.
Provide a structured JSON response with the following fields:
- summary: A 1-2 sentence executive summary of the problem.
- rootCause: The specific technical root cause identified.
- evidence: Key log lines, error messages, or event descriptions that confirm the root cause.
- remediation: An array of 3-5 specific, actionable remediation steps (use kubectl commands where appropriate).
Return ONLY valid JSON, no other text.`

// RunAnalysis performs root cause analysis on a Kubernetes resource.
func RunAnalysis(ctx context.Context, k8sClient kubernetes.Interface, settings *AISettings, req *AnalystRequest) (*AnalystResponse, error) {
	clusterContext, err := gatherAnalystContext(ctx, k8sClient, req)
	if err != nil {
		return nil, fmt.Errorf("failed to gather context: %w", err)
	}

	adapter, err := NewAdapter(settings)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize AI adapter: %w", err)
	}

	userPrompt := fmt.Sprintf(
		"Analyze the following Kubernetes %s '%s' in namespace '%s':\n\n%s",
		req.Kind, req.Name, req.Namespace, clusterContext,
	)
	if req.CustomQuery != "" {
		userPrompt += "\n\nAdditional question: " + req.CustomQuery
	}

	rawResponse, err := adapter.Chat(ctx, analystSystemPrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("AI analysis failed: %w", err)
	}

	response := &AnalystResponse{RawAnalysis: rawResponse}
	cleaned := strings.TrimSpace(rawResponse)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	cleaned = strings.TrimSpace(cleaned)

	var parsed struct {
		Summary     string   `json:"summary"`
		RootCause   string   `json:"rootCause"`
		Evidence    string   `json:"evidence"`
		Remediation []string `json:"remediation"`
	}
	if err := parseJSON(cleaned, &parsed); err == nil {
		response.Summary = parsed.Summary
		response.RootCause = parsed.RootCause
		response.Evidence = parsed.Evidence
		response.Remediation = parsed.Remediation
	} else {
		response.Summary = "Analysis complete. See raw analysis for details."
		response.RootCause = rawResponse
	}

	return response, nil
}

func gatherAnalystContext(ctx context.Context, k8sClient kubernetes.Interface, req *AnalystRequest) (string, error) {
	var sb strings.Builder
	switch strings.ToLower(req.Kind) {
	case "pod":
		return gatherPodContext(ctx, k8sClient, req, &sb)
	case "deployment", "statefulset", "daemonset", "replicaset":
		return gatherWorkloadContext(ctx, k8sClient, req, &sb)
	default:
		return gatherGenericContext(ctx, k8sClient, req, &sb)
	}
}

func gatherPodContext(ctx context.Context, k8sClient kubernetes.Interface, req *AnalystRequest, sb *strings.Builder) (string, error) {
	pod, err := k8sClient.CoreV1().Pods(req.Namespace).Get(ctx, req.Name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("pod not found: %w", err)
	}

	sb.WriteString("=== POD STATUS ===\n")
	sb.WriteString(fmt.Sprintf("Phase: %s\n", pod.Status.Phase))
	sb.WriteString(fmt.Sprintf("Node: %s\n", pod.Spec.NodeName))

	for _, cs := range pod.Status.ContainerStatuses {
		sb.WriteString(fmt.Sprintf("\nContainer: %s\n", cs.Name))
		sb.WriteString(fmt.Sprintf("  Ready: %v, Restarts: %d\n", cs.Ready, cs.RestartCount))
		if cs.State.Waiting != nil {
			sb.WriteString(fmt.Sprintf("  Waiting: %s - %s\n", cs.State.Waiting.Reason, cs.State.Waiting.Message))
		}
		if cs.State.Terminated != nil {
			sb.WriteString(fmt.Sprintf("  Terminated: Exit=%d, Reason=%s, Message=%s\n",
				cs.State.Terminated.ExitCode, cs.State.Terminated.Reason, cs.State.Terminated.Message))
		}
		if cs.LastTerminationState.Terminated != nil {
			sb.WriteString(fmt.Sprintf("  Last Exit: Code=%d, Reason=%s\n",
				cs.LastTerminationState.Terminated.ExitCode, cs.LastTerminationState.Terminated.Reason))
		}
	}

	containerName := req.ContainerName
	if containerName == "" && len(pod.Spec.Containers) > 0 {
		containerName = pod.Spec.Containers[0].Name
	}

	tailLines := int64(150)

	sb.WriteString("\n=== CURRENT LOGS (last 150 lines) ===\n")
	logStream, err := k8sClient.CoreV1().Pods(req.Namespace).GetLogs(req.Name, &corev1.PodLogOptions{
		Container: containerName,
		TailLines: &tailLines,
	}).Stream(ctx)
	if err == nil {
		buf, _ := io.ReadAll(io.LimitReader(logStream, 32*1024))
		logStream.Close()
		sb.Write(buf)
	}

	sb.WriteString("\n=== PREVIOUS CONTAINER LOGS ===\n")
	prevStream, err := k8sClient.CoreV1().Pods(req.Namespace).GetLogs(req.Name, &corev1.PodLogOptions{
		Container: containerName,
		TailLines: &tailLines,
		Previous:  true,
	}).Stream(ctx)
	if err == nil {
		buf, _ := io.ReadAll(io.LimitReader(prevStream, 16*1024))
		prevStream.Close()
		sb.Write(buf)
	} else {
		sb.WriteString("(no previous logs)\n")
	}

	sb.WriteString("\n=== KUBERNETES EVENTS ===\n")
	events, err := k8sClient.CoreV1().Events(req.Namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Pod", req.Name),
	})
	if err == nil {
		for _, ev := range events.Items {
			sb.WriteString(fmt.Sprintf("[%s] %s: %s\n", ev.Type, ev.Reason, ev.Message))
		}
	}

	return sb.String(), nil
}

func gatherWorkloadContext(ctx context.Context, k8sClient kubernetes.Interface, req *AnalystRequest, sb *strings.Builder) (string, error) {
	sb.WriteString(fmt.Sprintf("=== %s: %s/%s ===\n", req.Kind, req.Namespace, req.Name))

	pods, err := k8sClient.CoreV1().Pods(req.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}

	var failingPods []string
	for _, pod := range pods.Items {
		for _, ref := range pod.OwnerReferences {
			if strings.EqualFold(ref.Kind, req.Kind) && ref.Name == req.Name {
				for _, cs := range pod.Status.ContainerStatuses {
					if cs.State.Waiting != nil || cs.RestartCount > 3 {
						failingPods = append(failingPods, pod.Name)
					}
				}
			}
		}
	}

	if len(failingPods) > 0 {
		sb.WriteString(fmt.Sprintf("Failing pods: %v\n", failingPods))
		podReq := &AnalystRequest{
			Namespace:     req.Namespace,
			Kind:          "Pod",
			Name:          failingPods[0],
			ContainerName: req.ContainerName,
		}
		var podSb strings.Builder
		podCtx, _ := gatherPodContext(ctx, k8sClient, podReq, &podSb)
		sb.WriteString(podCtx)
	}

	sb.WriteString("\n=== WORKLOAD EVENTS ===\n")
	events, _ := k8sClient.CoreV1().Events(req.Namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s", req.Name),
	})
	if events != nil {
		for _, ev := range events.Items {
			sb.WriteString(fmt.Sprintf("[%s] %s: %s\n", ev.Type, ev.Reason, ev.Message))
		}
	}

	return sb.String(), nil
}

func gatherGenericContext(ctx context.Context, k8sClient kubernetes.Interface, req *AnalystRequest, sb *strings.Builder) (string, error) {
	sb.WriteString(fmt.Sprintf("Resource: %s %s/%s\n", req.Kind, req.Namespace, req.Name))

	events, _ := k8sClient.CoreV1().Events(req.Namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s", req.Name),
	})
	if events != nil {
		sb.WriteString("Events:\n")
		for _, ev := range events.Items {
			sb.WriteString(fmt.Sprintf("[%s] %s: %s\n", ev.Type, ev.Reason, ev.Message))
		}
	}

	return sb.String(), nil
}
