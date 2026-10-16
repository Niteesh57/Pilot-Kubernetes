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
Analyze the provided cluster audit data and generate a comprehensive improvement summary formatted in clean, rich Markdown.
Structure your response as follows:

### Reliability & Hygiene Assessment
State the overall cluster reliability health, resource efficiency state, and single-point-of-failure exposure.

### Key Optimization Priorities
1. **Resource Limits & Requests**: Priority adjustments for containers with missing CPU/memory constraints.
2. **Health Probes**: Pods requiring liveness and readiness probe enforcement.
3. **High Availability & Anti-Affinity**: Workloads at risk of eviction or single node failure.

### Actionable Remediation Commands
Provide exact kubectl commands and YAML snippets in fenced code blocks.

Do not return a single block of unformatted text. Always use Markdown headings, lists, and code blocks.`

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

	// Always ensure an authoritative Markdown assessment is returned so the card displays on every run/re-audit
	if result.AISummary == "" {
		result.AISummary = generateFallbackImprovementMarkdown(totalScanned, findings, namespace)
	}

	return result, nil
}

func generateFallbackImprovementMarkdown(totalScanned int, findings []ImprovementFinding, namespace string) string {
	counts := map[string]int{"critical": 0, "warning": 0, "info": 0}
	var missingLimits, missingProbes, spofFindings, deadServices []ImprovementFinding

	for _, f := range findings {
		counts[f.Severity]++
		if strings.Contains(f.Issue, "resource limits") {
			missingLimits = append(missingLimits, f)
		} else if strings.Contains(f.Issue, "liveness") || strings.Contains(f.Issue, "readiness") {
			missingProbes = append(missingProbes, f)
		} else if strings.Contains(f.Issue, "Single-replica") {
			spofFindings = append(spofFindings, f)
		} else if strings.Contains(f.Issue, "no ready endpoints") {
			deadServices = append(deadServices, f)
		}
	}

	targetScope := "Entire Cluster"
	if namespace != "" && namespace != "all" && namespace != "_all" {
		targetScope = fmt.Sprintf("Namespace: %s", namespace)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("### Reliability & Hygiene Assessment (%s)\n", targetScope))
	sb.WriteString(fmt.Sprintf("Reliability evaluation scanned **%d total workloads** across the cluster. Detected **%d actionable hygiene and resiliency findings** (%d critical, %d warnings, %d informational).\n\n",
		totalScanned, len(findings), counts["critical"], counts["warning"], counts["info"]))

	if len(findings) == 0 {
		sb.WriteString("All analyzed workloads enforce strict CPU/memory limits, healthy probe configurations, and high availability redundancy. The cluster reliability posture is currently optimal.\n")
		return sb.String()
	}

	sb.WriteString("### Key Optimization Priorities\n")
	priorityIdx := 1
	if len(deadServices) > 0 {
		sb.WriteString(fmt.Sprintf("%d. **Unhealthy Service Endpoints (%d services)**: Services have 0 ready endpoints. Traffic to these services is actively failing.\n", priorityIdx, len(deadServices)))
		priorityIdx++
	}
	if len(missingLimits) > 0 {
		sb.WriteString(fmt.Sprintf("%d. **Resource Limits & Requests (%d containers)**: Workloads lack CPU/memory boundaries, exposing nodes to noisy-neighbor resource starvation and unexpected out-of-memory (OOM) eviction cascades.\n", priorityIdx, len(missingLimits)))
		priorityIdx++
	}
	if len(spofFindings) > 0 {
		sb.WriteString(fmt.Sprintf("%d. **High Availability & Single Points of Failure (%d deployments)**: Workloads run with a single replica without autoscaling (HPA). Node maintenance or pod restarts cause unmitigated service outages.\n", priorityIdx, len(spofFindings)))
		priorityIdx++
	}
	if len(missingProbes) > 0 {
		sb.WriteString(fmt.Sprintf("%d. **Health Probes (%d containers)**: Containers lack liveness or readiness probes, impairing Kubernetes' ability to self-heal hung containers or route traffic around startup delays.\n", priorityIdx, len(missingProbes)))
		priorityIdx++
	}

	sb.WriteString("\n### Actionable Remediation Commands & Manifests\n")
	sb.WriteString("```bash\n")
	count := 0
	for _, f := range findings {
		if f.YAMLPatch != "" {
			lines := strings.Split(f.YAMLPatch, "\n")
			for _, line := range lines {
				if strings.HasPrefix(strings.TrimSpace(line), "#") {
					sb.WriteString(fmt.Sprintf("%s\n", line))
				}
			}
			count++
			if count >= 3 {
				break
			}
		}
	}
	sb.WriteString("# Verify cluster workload health\nkubectl get deployments,services -A --field-selector metadata.namespace!=kube-system\n")
	sb.WriteString("```\n")

	return sb.String()
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
