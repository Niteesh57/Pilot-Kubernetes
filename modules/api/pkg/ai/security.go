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

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const securitySystemPrompt = `You are a Kubernetes security expert specializing in RBAC and Pod Security Standards.
Analyze the provided audit data and generate an executive security summary.
Include overall risk level (Critical/High/Medium/Low), top 3 priority remediations, and specific kubectl or YAML fixes.
Return a 2-3 paragraph plain text summary.`

// sensitiveVerbs that indicate elevated privilege.
var sensitiveVerbs = map[string]bool{
	"impersonate": true,
	"escalate":    true,
	"bind":        true,
	"*":           true,
}

// sensitiveResources that warrant close scrutiny.
var sensitiveResources = map[string]bool{
	"secrets":                    true,
	"serviceaccounts/token":      true,
	"pods/exec":                  true,
	"pods/attach":                true,
	"clusterroles":               true,
	"clusterrolebindings":        true,
	"roles":                      true,
	"rolebindings":               true,
	"*":                          true,
}

// RunSecurityAudit audits the cluster for RBAC and workload security issues.
func RunSecurityAudit(ctx context.Context, k8sClient kubernetes.Interface, settings *AISettings, namespace string) (*SecurityResponse, error) {
	findings := []SecurityFinding{}

	// ─── RBAC Audit ───────────────────────────────────────────────
	clusterRoles, _ := k8sClient.RbacV1().ClusterRoles().List(ctx, metav1.ListOptions{})
	if clusterRoles != nil {
		for _, cr := range clusterRoles.Items {
			rbacFindings := auditRBACRules(cr.Rules, "ClusterRole", cr.Name, "")
			findings = append(findings, rbacFindings...)
		}
	}

	roles, _ := k8sClient.RbacV1().Roles("").List(ctx, metav1.ListOptions{})
	if roles != nil {
		for _, r := range roles.Items {
			rbacFindings := auditRBACRules(r.Rules, "Role", r.Name, r.Namespace)
			findings = append(findings, rbacFindings...)
		}
	}

	// Check for default SA with cluster-admin
	crbs, _ := k8sClient.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{})
	if crbs != nil {
		for _, crb := range crbs.Items {
			if crb.RoleRef.Name == "cluster-admin" {
				for _, subject := range crb.Subjects {
					if subject.Kind == "ServiceAccount" && subject.Name == "default" {
						findings = append(findings, SecurityFinding{
							Kind: "ClusterRoleBinding", Name: crb.Name, Namespace: subject.Namespace,
							Category: "rbac", Severity: "critical",
							Issue:   "Default ServiceAccount has cluster-admin access.",
							Details: "The 'default' ServiceAccount should never have cluster-admin privileges. This grants any pod using the default SA unrestricted cluster access.",
							YAMLPatch: fmt.Sprintf("kubectl delete clusterrolebinding %s", crb.Name),
						})
					}
				}
			}
		}
	}

	// ─── Workload Security Context Audit ──────────────────────────
	namespaces := []string{namespace}
	if namespace == "" || namespace == "all" {
		nsList, _ := k8sClient.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		if nsList != nil {
			namespaces = []string{}
			for _, ns := range nsList.Items {
				namespaces = append(namespaces, ns.Name)
			}
		}
	}

	for _, ns := range namespaces {
		pods, err := k8sClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, pod := range pods.Items {
			// Host namespace checks
			if pod.Spec.HostPID {
				findings = append(findings, SecurityFinding{
					Kind: "Pod", Name: pod.Name, Namespace: ns,
					Category: "workload", Severity: "high",
					Issue:   "Pod uses hostPID, allowing access to all host process IDs.",
					Details: "This can be used for privilege escalation. Remove hostPID: true from pod spec.",
				})
			}
			if pod.Spec.HostNetwork {
				findings = append(findings, SecurityFinding{
					Kind: "Pod", Name: pod.Name, Namespace: ns,
					Category: "workload", Severity: "high",
					Issue:   "Pod uses hostNetwork, bypassing network isolation.",
					Details: "The pod can access all network services on the host. Remove hostNetwork: true.",
				})
			}

			// Container security checks
			for _, c := range pod.Spec.Containers {
				if c.SecurityContext == nil {
					findings = append(findings, SecurityFinding{
						Kind: "Pod", Name: fmt.Sprintf("%s/%s", pod.Name, c.Name), Namespace: ns,
						Category: "workload", Severity: "medium",
						Issue:   fmt.Sprintf("Container '%s' has no securityContext defined.", c.Name),
						Details: "Without a securityContext, containers may run with overly permissive defaults.",
						YAMLPatch: fmt.Sprintf(`# Add to container spec for '%s':
securityContext:
  runAsNonRoot: true
  runAsUser: 1000
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop:
    - ALL`, c.Name),
					})
					continue
				}

				sc := c.SecurityContext
				if sc.Privileged != nil && *sc.Privileged {
					findings = append(findings, SecurityFinding{
						Kind: "Pod", Name: fmt.Sprintf("%s/%s", pod.Name, c.Name), Namespace: ns,
						Category: "workload", Severity: "critical",
						Issue:   fmt.Sprintf("Container '%s' is running in privileged mode.", c.Name),
						Details: "Privileged containers have full host access equivalent to root. This is a critical security risk.",
					})
				}
				if sc.AllowPrivilegeEscalation != nil && *sc.AllowPrivilegeEscalation {
					findings = append(findings, SecurityFinding{
						Kind: "Pod", Name: fmt.Sprintf("%s/%s", pod.Name, c.Name), Namespace: ns,
						Category: "workload", Severity: "high",
						Issue:   fmt.Sprintf("Container '%s' allows privilege escalation.", c.Name),
						Details: "Set allowPrivilegeEscalation: false to prevent privilege escalation attacks.",
					})
				}
				if sc.RunAsUser != nil && *sc.RunAsUser == 0 {
					findings = append(findings, SecurityFinding{
						Kind: "Pod", Name: fmt.Sprintf("%s/%s", pod.Name, c.Name), Namespace: ns,
						Category: "workload", Severity: "high",
						Issue:   fmt.Sprintf("Container '%s' runs as root (UID 0).", c.Name),
						Details: "Running as root increases blast radius of any container escape. Use runAsUser with a non-zero UID.",
					})
				}
			}
		}

		// Check namespaces missing NetworkPolicies (no isolation)
		policies, _ := k8sClient.NetworkingV1().NetworkPolicies(ns).List(ctx, metav1.ListOptions{})
		if policies != nil && len(policies.Items) == 0 {
			// Only flag user namespaces
			if ns != "kube-system" && ns != "kube-public" && ns != "kube-node-lease" {
				findings = append(findings, SecurityFinding{
					Kind: "Namespace", Name: ns, Namespace: ns,
					Category: "network", Severity: "medium",
					Issue:   fmt.Sprintf("Namespace '%s' has no NetworkPolicies. All pods can communicate freely.", ns),
					Details: "Implement a default-deny NetworkPolicy and explicitly allow required traffic.",
					YAMLPatch: fmt.Sprintf(`# Default deny all ingress in namespace %s:
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny-ingress
  namespace: %s
spec:
  podSelector: {}
  policyTypes:
  - Ingress`, ns, ns),
				})
			}
		}
	}

	// Calculate score: start at 100, deduct per severity
	score := 100
	for _, f := range findings {
		switch f.Severity {
		case "critical":
			score -= 15
		case "high":
			score -= 8
		case "medium":
			score -= 3
		case "low":
			score -= 1
		}
	}
	if score < 0 {
		score = 0
	}

	result := &SecurityResponse{
		Score:    score,
		Findings: findings,
	}

	// Generate AI summary
	if settings != nil && settings.APIKey != "" && len(findings) > 0 {
		adapter, err := NewAdapter(settings)
		if err == nil {
			summaryText := buildSecuritySummary(score, findings)
			aiSummary, err := adapter.Chat(ctx, securitySystemPrompt, summaryText)
			if err == nil {
				result.AISummary = aiSummary
			}
		}
	}

	return result, nil
}

func auditRBACRules(rules []rbacv1.PolicyRule, kind, name, namespace string) []SecurityFinding {
	var findings []SecurityFinding
	for _, rule := range rules {
		for _, verb := range rule.Verbs {
			if sensitiveVerbs[verb] {
				for _, resource := range rule.Resources {
					if sensitiveResources[resource] || verb == "*" || resource == "*" {
						sev := "high"
						if verb == "*" && resource == "*" {
							sev = "critical"
						}
						findings = append(findings, SecurityFinding{
							Kind: kind, Name: name, Namespace: namespace,
							Category: "rbac", Severity: sev,
							Issue: fmt.Sprintf("%s '%s' grants '%s' on '%s'. This is an overly permissive RBAC rule.",
								kind, name, strings.Join(rule.Verbs, ","), strings.Join(rule.Resources, ",")),
							Details: "Review and restrict this rule to the minimum required permissions (principle of least privilege).",
						})
						break
					}
				}
			}
		}
	}
	return findings
}

func buildSecuritySummary(score int, findings []SecurityFinding) string {
	counts := map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0}
	for _, f := range findings {
		counts[f.Severity]++
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Security Score: %d/100\n", score))
	sb.WriteString(fmt.Sprintf("Critical: %d, High: %d, Medium: %d, Low: %d\n\n", counts["critical"], counts["high"], counts["medium"], counts["low"]))
	sb.WriteString("Findings:\n")
	for i, f := range findings {
		if i >= 20 {
			sb.WriteString("... (truncated)\n")
			break
		}
		sb.WriteString(fmt.Sprintf("- [%s] %s/%s: %s\n", f.Severity, f.Kind, f.Name, f.Issue))
	}
	return sb.String()
}

// parseJSON is a helper to unmarshal JSON safely.
func parseJSON(data string, v interface{}) error {
	return json.Unmarshal([]byte(data), v)
}
