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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const improvementSystemPrompt = `You are a senior Kubernetes reliability and platform engineer.
Analyze the provided cluster audit data and generate a comprehensive improvement summary.
Focus on reliability, resource efficiency, and operational best practices.
Return a 2-3 paragraph executive summary in plain text (no JSON).`

// RunImprovementAudit audits the cluster for reliability and resource issues.
func RunImprovementAudit(ctx context.Context, k8sClient kubernetes.Interface, settings *AISettings, namespace string) (*ImprovementResponse, error) {
	findings := []ImprovementFinding{}

	namespaces := []string{namespace}
	if namespace == "" || namespace == "all" {
		nsList, err := k8sClient.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		namespaces = []string{}
		for _, ns := range nsList.Items {
			namespaces = append(namespaces, ns.Name)
		}
	}

	totalScanned := 0

	for _, ns := range namespaces {
		// Audit Deployments
		deployments, err := k8sClient.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}

		for _, d := range deployments.Items {
			totalScanned++
			for _, container := range d.Spec.Template.Spec.Containers {
				// Missing resource limits
				if container.Resources.Limits == nil || len(container.Resources.Limits) == 0 {
					findings = append(findings, ImprovementFinding{
						Namespace: ns, Kind: "Deployment", Name: d.Name,
						Category: "resources", Severity: "warning",
						Issue: fmt.Sprintf("Container '%s' has no resource limits (CPU/Memory). This can cause noisy-neighbor issues and unexpected OOM kills.", container.Name),
						YAMLPatch: fmt.Sprintf(`# Add to spec.template.spec.containers[name=%s]:
resources:
  requests:
    cpu: "100m"
    memory: "128Mi"
  limits:
    cpu: "500m"
    memory: "512Mi"`, container.Name),
					})
				}

				// Missing liveness probe
				if container.LivenessProbe == nil {
					findings = append(findings, ImprovementFinding{
						Namespace: ns, Kind: "Deployment", Name: d.Name,
						Category: "reliability", Severity: "warning",
						Issue: fmt.Sprintf("Container '%s' has no liveness probe. Kubernetes cannot detect and restart stuck containers.", container.Name),
						YAMLPatch: fmt.Sprintf(`# Add to spec.template.spec.containers[name=%s]:
livenessProbe:
  httpGet:
    path: /healthz
    port: 8080
  initialDelaySeconds: 30
  periodSeconds: 10
  failureThreshold: 3`, container.Name),
					})
				}

				// Missing readiness probe
				if container.ReadinessProbe == nil {
					findings = append(findings, ImprovementFinding{
						Namespace: ns, Kind: "Deployment", Name: d.Name,
						Category: "reliability", Severity: "info",
						Issue: fmt.Sprintf("Container '%s' has no readiness probe. Traffic may be sent before the application is ready.", container.Name),
					})
				}
			}

			// Single replica without HPA
			if d.Spec.Replicas != nil && *d.Spec.Replicas == 1 {
				// Check for HPA
				hpas, _ := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{})
				hasHPA := false
				if hpas != nil {
					for _, hpa := range hpas.Items {
						if hpa.Spec.ScaleTargetRef.Name == d.Name {
							hasHPA = true
							break
						}
					}
				}
				if !hasHPA {
					findings = append(findings, ImprovementFinding{
						Namespace: ns, Kind: "Deployment", Name: d.Name,
						Category: "reliability", Severity: "warning",
						Issue: "Single-replica Deployment with no HPA. This is a single point of failure; any pod restart will cause downtime.",
						YAMLPatch: fmt.Sprintf(`# Consider adding a HorizontalPodAutoscaler:
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: %s-hpa
  namespace: %s
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: %s
  minReplicas: 2
  maxReplicas: 10
  metrics:
  - type: Resource
    resource:
      name: cpu
      target:
        type: Utilization
        averageUtilization: 70`, d.Name, ns, d.Name),
					})
				}
			}
		}

		// Audit Services with no endpoints
		services, err := k8sClient.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
		if err == nil {
			for _, svc := range services.Items {
				if svc.Spec.Type == corev1.ServiceTypeExternalName {
					continue
				}
				endpoints, err := k8sClient.CoreV1().Endpoints(ns).Get(ctx, svc.Name, metav1.GetOptions{})
				if err != nil {
					continue
				}
				hasReady := false
				for _, subset := range endpoints.Subsets {
					if len(subset.Addresses) > 0 {
						hasReady = true
						break
					}
				}
				if !hasReady && len(svc.Spec.Selector) > 0 {
					findings = append(findings, ImprovementFinding{
						Namespace: ns, Kind: "Service", Name: svc.Name,
						Category: "reliability", Severity: "critical",
						Issue: "Service has no ready endpoints. Requests to this service will fail. Check if backing pods are running and selectors match.",
					})
				}
			}
		}
	}

	result := &ImprovementResponse{
		TotalScanned: totalScanned,
		Findings:     findings,
	}

	// Generate AI summary if findings exist and AI is configured
	if len(findings) > 0 && settings != nil && settings.APIKey != "" {
		adapter, err := NewAdapter(settings)
		if err == nil {
			summary := buildFindingsSummary(findings)
			aiSummary, err := adapter.Chat(ctx, improvementSystemPrompt,
				fmt.Sprintf("Cluster has %d workloads scanned with %d findings:\n\n%s", totalScanned, len(findings), summary))
			if err == nil {
				result.AISummary = aiSummary
			}
		}
	}

	return result, nil
}

func buildFindingsSummary(findings []ImprovementFinding) string {
	var sb strings.Builder
	critical := 0
	warnings := 0
	for _, f := range findings {
		switch f.Severity {
		case "critical":
			critical++
		case "warning":
			warnings++
		}
		sb.WriteString(fmt.Sprintf("- [%s] %s/%s: %s\n", f.Severity, f.Kind, f.Name, f.Issue))
		if sb.Len() > 3000 {
			sb.WriteString("... (truncated)")
			break
		}
	}
	return fmt.Sprintf("Critical: %d, Warnings: %d, Info: %d\n\n%s",
		critical, warnings, len(findings)-critical-warnings, sb.String())
}
