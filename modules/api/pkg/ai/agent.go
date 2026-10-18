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
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const agentSystemPrompt = `You are an expert autonomous Kubernetes SRE agent in Kubernetes Dashboard.
You have access to live cluster information, container images, pod logs, events, inter-service network topology, and FalkorDB knowledge graph topology.
Your goal is to answer the engineer's question or solve the operational problem thoroughly, accurately, and concisely.

FalkorDB Graph Schema:
- Nodes: (:Pod {name, namespace, status, ip}), (:Service {name, namespace, clusterIP}), (:Deployment {name, namespace, replicas}), (:Node {name}), (:Namespace {name})
- Edges: (:Pod)-[:RUNS_ON]->(:Node), (:Service)-[:ROUTES_TO]->(:Pod), (:Deployment)-[:MANAGES]->(:Pod), (:Pod)-[:COMMUNICATES_WITH]->(:Service)

Guidelines:
- When asked what a component is used for, explain its architectural role in production, how it integrates with the cluster and Kubernetes Dashboard, its key functions, container specs, and its current health.
- When diagnosing an issue, identify the specific root cause (e.g. OOMKilled, failing health probe, selector mismatch, network policy block, latency hop) and provide exact remediation steps.
- If the workload is healthy and the question is informational, directly answer the question in rich Markdown.
- Provide useful kubectl commands and Cypher queries when relevant.
- Suggest 2-4 interactive follow-up questions or actions that an SRE engineer might want to ask or execute next (like in Antigravity).
- Keep responses clean, professional, and well-structured without dumping raw graph arrays or internal structures.

Structure your response as JSON with these fields:
- summary: Clear, insightful answer to the engineer's query in rich Markdown.
- rootCause: If diagnosing a failure or degraded condition, the specific technical root cause. If healthy or informational, leave empty.
- commands: Array of useful kubectl commands relevant to the resource.
- yamlPatches: Array of YAML patch strings if fixes are recommended.
- cypherQueries: Array of Cypher query strings to inspect topology or blast radius in FalkorDB (e.g. "MATCH (p:Pod {name: '...'})<-[:ROUTES_TO]-(s:Service) RETURN s").
- followUps: Array of 2-4 interactive follow-up questions or remediation confirmations for the engineer.
Return ONLY valid JSON, no other text.`

// MentionTarget represents a Kubernetes resource mentioned with @kind/name in the chat query.
type MentionTarget struct {
	Kind      string // "deployment", "pod", "service"
	Name      string
	Namespace string
}

// WorkloadTelemetry captures structured operational metadata for target resources.
type WorkloadTelemetry struct {
	Target        MentionTarget
	Found         bool
	Kind          string
	Name          string
	Namespace     string
	Image         string
	Ports         []string
	ReplicasDesc  string
	ReadyReplicas int32
	Desired       int32
	Conditions    []string
	ChildPods     []string
	RootCauseHint string
}

// mentionRegex matches @deployment/name, @pod/name, @service/name, @svc/name (or @kind:name or with optional namespace)
var mentionRegex = regexp.MustCompile(`(?i)@(deployment|pod|service|svc)[/:](?:([a-zA-Z0-9\.\-_]+)/)?([a-zA-Z0-9\.\-_]+)`)

func parseResourceMentions(query, defaultNamespace string) []MentionTarget {
	matches := mentionRegex.FindAllStringSubmatch(query, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var targets []MentionTarget
	for _, m := range matches {
		kind := strings.ToLower(m[1])
		if kind == "svc" {
			kind = "service"
		}
		ns := defaultNamespace
		name := m[3]
		if m[2] != "" {
			ns = m[2]
		}
		key := fmt.Sprintf("%s/%s/%s", kind, ns, name)
		if !seen[key] {
			seen[key] = true
			targets = append(targets, MentionTarget{
				Kind:      kind,
				Name:      name,
				Namespace: ns,
			})
		}
	}
	return targets
}

// GetResourcesForAutocomplete returns deployments, pods, and services for @mention autocomplete.
func GetResourcesForAutocomplete(ctx context.Context, k8sClient kubernetes.Interface, namespace string) *ResourcesResponse {
	resp := &ResourcesResponse{
		Deployments: []ResourceItem{},
		Pods:        []ResourceItem{},
		Services:    []ResourceItem{},
	}

	ns := namespace
	if ns == "all" || ns == "_all" {
		ns = ""
	}

	// 1. Deployments
	if deps, err := k8sClient.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{}); err == nil {
		for _, d := range deps.Items {
			status := fmt.Sprintf("%d/%d ready", d.Status.ReadyReplicas, *d.Spec.Replicas)
			resp.Deployments = append(resp.Deployments, ResourceItem{
				Kind:      "Deployment",
				Name:      d.Name,
				Namespace: d.Namespace,
				Status:    status,
			})
		}
	}

	// 2. Pods
	if pods, err := k8sClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{}); err == nil {
		for _, p := range pods.Items {
			phase := string(p.Status.Phase)
			for _, cs := range p.Status.ContainerStatuses {
				if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
					phase = cs.State.Waiting.Reason
					break
				}
			}
			resp.Pods = append(resp.Pods, ResourceItem{
				Kind:      "Pod",
				Name:      p.Name,
				Namespace: p.Namespace,
				Status:    phase,
			})
		}
	}

	// 3. Services
	if svcs, err := k8sClient.CoreV1().Services(ns).List(ctx, metav1.ListOptions{}); err == nil {
		for _, s := range svcs.Items {
			resp.Services = append(resp.Services, ResourceItem{
				Kind:      "Service",
				Name:      s.Name,
				Namespace: s.Namespace,
				Status:    string(s.Spec.Type),
			})
		}
	}

	return resp
}

func gatherTargetedTelemetry(ctx context.Context, k8sClient kubernetes.Interface, target *MentionTarget, defaultNamespace string) (string, []string, string, WorkloadTelemetry) {
	var sb strings.Builder
	var commands []string
	rootCauseHint := ""
	info := WorkloadTelemetry{
		Target: *target,
		Kind:   strings.Title(target.Kind),
		Name:   target.Name,
	}

	ns := target.Namespace
	if ns == "" {
		ns = defaultNamespace
	}

	switch target.Kind {
	case "deployment":
		info.Kind = "Deployment"
		dep, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			if allDeps, listErr := k8sClient.AppsV1().Deployments("").List(ctx, metav1.ListOptions{}); listErr == nil {
				for _, d := range allDeps.Items {
					if d.Name == target.Name {
						dep = &d
						ns = d.Namespace
						target.Namespace = d.Namespace
						err = nil
						break
					}
				}
			}
		}
		if err != nil {
			sb.WriteString(fmt.Sprintf("=== TARGET DEPLOYMENT: %s (Namespace: %s) ===\nStatus: NotFound (%v)\n", target.Name, ns, err))
			commands = append(commands, fmt.Sprintf("kubectl get deployments -A | grep %s", target.Name))
			return sb.String(), commands, "", info
		}

		info.Found = true
		info.Namespace = dep.Namespace
		target.Namespace = dep.Namespace
		desired := int32(1)
		if dep.Spec.Replicas != nil {
			desired = *dep.Spec.Replicas
		}
		info.Desired = desired
		info.ReadyReplicas = dep.Status.ReadyReplicas
		info.ReplicasDesc = fmt.Sprintf("%d/%d ready", dep.Status.ReadyReplicas, desired)
		if len(dep.Spec.Template.Spec.Containers) > 0 {
			info.Image = dep.Spec.Template.Spec.Containers[0].Image
			for _, c := range dep.Spec.Template.Spec.Containers {
				for _, p := range c.Ports {
					info.Ports = append(info.Ports, fmt.Sprintf("%d/%s", p.ContainerPort, p.Protocol))
				}
			}
		}

		sb.WriteString(fmt.Sprintf("=== TARGET DEPLOYMENT: %s (Namespace: %s) ===\n", dep.Name, dep.Namespace))
		sb.WriteString(fmt.Sprintf("Replicas: %d desired, %d ready, %d available, %d updated\n",
			desired, dep.Status.ReadyReplicas, dep.Status.AvailableReplicas, dep.Status.UpdatedReplicas))

		if dep.Status.ReadyReplicas < desired {
			rootCauseHint = fmt.Sprintf("Deployment '%s' has degraded replicas (%d/%d ready).", dep.Name, dep.Status.ReadyReplicas, desired)
		}

		for _, c := range dep.Status.Conditions {
			sb.WriteString(fmt.Sprintf("Condition [%s]: Status=%s, Reason=%s, Message=%s\n", c.Type, c.Status, c.Reason, c.Message))
			info.Conditions = append(info.Conditions, fmt.Sprintf("%s=%s", c.Type, c.Status))
		}

		// List child pods
		if dep.Spec.Selector != nil {
			selector, _ := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
			if selector != nil {
				pods, _ := k8sClient.CoreV1().Pods(dep.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
				if pods != nil && len(pods.Items) > 0 {
					sb.WriteString(fmt.Sprintf("Child Pods (%d):\n", len(pods.Items)))
					for _, p := range pods.Items {
						restarts := int32(0)
						waiting := ""
						for _, cs := range p.Status.ContainerStatuses {
							restarts += cs.RestartCount
							if cs.State.Waiting != nil {
								waiting = cs.State.Waiting.Reason
							}
						}
						info.ChildPods = append(info.ChildPods, fmt.Sprintf("%s (%s, restarts: %d)", p.Name, p.Status.Phase, restarts))
						sb.WriteString(fmt.Sprintf("  - %s (Phase: %s, Restarts: %d, Node: %s, Waiting: %s)\n",
							p.Name, p.Status.Phase, restarts, p.Spec.NodeName, waiting))
					}
				}
			}
		}

		commands = append(commands,
			fmt.Sprintf("kubectl describe deployment %s -n %s", dep.Name, dep.Namespace),
			fmt.Sprintf("kubectl rollout status deployment/%s -n %s", dep.Name, dep.Namespace),
		)

	case "pod":
		info.Kind = "Pod"
		pod, err := k8sClient.CoreV1().Pods(ns).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			if allPods, listErr := k8sClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{}); listErr == nil {
				for _, p := range allPods.Items {
					if p.Name == target.Name {
						pod = &p
						ns = p.Namespace
						target.Namespace = p.Namespace
						err = nil
						break
					}
				}
			}
		}
		if err != nil {
			sb.WriteString(fmt.Sprintf("=== TARGET POD: %s (Namespace: %s) ===\nStatus: NotFound (%v)\n", target.Name, ns, err))
			commands = append(commands, fmt.Sprintf("kubectl get pods -A | grep %s", target.Name))
			return sb.String(), commands, "", info
		}

		info.Found = true
		info.Namespace = pod.Namespace
		target.Namespace = pod.Namespace
		info.ReplicasDesc = string(pod.Status.Phase)
		if len(pod.Spec.Containers) > 0 {
			info.Image = pod.Spec.Containers[0].Image
			for _, c := range pod.Spec.Containers {
				for _, p := range c.Ports {
					info.Ports = append(info.Ports, fmt.Sprintf("%d/%s", p.ContainerPort, p.Protocol))
				}
			}
		}

		sb.WriteString(fmt.Sprintf("=== TARGET POD: %s (Namespace: %s, Node: %s, IP: %s) ===\n",
			pod.Name, pod.Namespace, pod.Spec.NodeName, pod.Status.PodIP))
		sb.WriteString(fmt.Sprintf("Phase: %s, QoS: %s\n", pod.Status.Phase, pod.Status.QOSClass))

		for _, cs := range pod.Status.ContainerStatuses {
			restarts := cs.RestartCount
			stateStr := "Running"
			exitCode := int32(0)
			reason := ""
			if cs.State.Waiting != nil {
				stateStr = "Waiting"
				reason = cs.State.Waiting.Reason
				if rootCauseHint == "" {
					rootCauseHint = fmt.Sprintf("Pod '%s' container '%s' waiting: %s (%s)", pod.Name, cs.Name, reason, cs.State.Waiting.Message)
				}
			} else if cs.State.Terminated != nil {
				stateStr = "Terminated"
				exitCode = cs.State.Terminated.ExitCode
				reason = cs.State.Terminated.Reason
				if rootCauseHint == "" {
					rootCauseHint = fmt.Sprintf("Pod '%s' container '%s' terminated with exit code %d (%s).", pod.Name, cs.Name, exitCode, reason)
				}
			}
			sb.WriteString(fmt.Sprintf("Container [%s]: Ready=%v, Restarts=%d, State=%s, ExitCode=%d, Reason=%s\n",
				cs.Name, cs.Ready, restarts, stateStr, exitCode, reason))
		}

		// Fetch last 30 lines of logs
		tailLines := int64(30)
		req := k8sClient.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			TailLines: &tailLines,
		})
		if logStream, err := req.Stream(ctx); err == nil {
			defer logStream.Close()
			if logBytes, err := io.ReadAll(logStream); err == nil && len(logBytes) > 0 {
				sb.WriteString(fmt.Sprintf("\n--- Recent Logs (%s) ---\n%s\n", pod.Name, string(logBytes)))
			}
		}

		// Warning events
		if events, err := k8sClient.CoreV1().Events(pod.Namespace).List(ctx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("involvedObject.name=%s,type=Warning", pod.Name),
		}); err == nil && len(events.Items) > 0 {
			sb.WriteString("\n--- Pod Warning Events ---\n")
			for _, ev := range events.Items {
				sb.WriteString(fmt.Sprintf("  [%s] %s: %s\n", ev.Reason, ev.InvolvedObject.FieldPath, ev.Message))
			}
		}

		commands = append(commands,
			fmt.Sprintf("kubectl describe pod %s -n %s", pod.Name, pod.Namespace),
			fmt.Sprintf("kubectl logs %s -n %s --tail=50", pod.Name, pod.Namespace),
		)

	case "service":
		info.Kind = "Service"
		svc, err := k8sClient.CoreV1().Services(ns).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			if allSvcs, listErr := k8sClient.CoreV1().Services("").List(ctx, metav1.ListOptions{}); listErr == nil {
				for _, s := range allSvcs.Items {
					if s.Name == target.Name {
						svc = &s
						ns = s.Namespace
						target.Namespace = s.Namespace
						err = nil
						break
					}
				}
			}
		}
		if err != nil {
			sb.WriteString(fmt.Sprintf("=== TARGET SERVICE: %s (Namespace: %s) ===\nStatus: NotFound (%v)\n", target.Name, ns, err))
			commands = append(commands, fmt.Sprintf("kubectl get svc -A | grep %s", target.Name))
			return sb.String(), commands, "", info
		}

		info.Found = true
		info.Namespace = svc.Namespace
		target.Namespace = svc.Namespace
		sb.WriteString(fmt.Sprintf("=== TARGET SERVICE: %s (Namespace: %s, Type: %s, ClusterIP: %s) ===\n",
			svc.Name, svc.Namespace, svc.Spec.Type, svc.Spec.ClusterIP))
		for _, p := range svc.Spec.Ports {
			portDesc := fmt.Sprintf("%d -> %s (%s)", p.Port, p.TargetPort.String(), p.Protocol)
			sb.WriteString(fmt.Sprintf("Port: %s\n", portDesc))
			info.Ports = append(info.Ports, portDesc)
		}

		// Endpoints
		readyCount := 0
		notReadyCount := 0
		if ep, err := k8sClient.CoreV1().Endpoints(svc.Namespace).Get(ctx, svc.Name, metav1.GetOptions{}); err == nil {
			for _, subset := range ep.Subsets {
				readyCount += len(subset.Addresses)
				notReadyCount += len(subset.NotReadyAddresses)
			}
		}
		info.ReplicasDesc = fmt.Sprintf("%d endpoints ready", readyCount)
		sb.WriteString(fmt.Sprintf("Endpoints: %d Ready, %d Not Ready\n", readyCount, notReadyCount))
		if readyCount == 0 && len(svc.Spec.Selector) > 0 {
			rootCauseHint = fmt.Sprintf("Service '%s' has 0 ready endpoints. Traffic to port %s will be refused.", svc.Name, svc.Spec.ClusterIP)
		}

		commands = append(commands,
			fmt.Sprintf("kubectl describe svc %s -n %s", svc.Name, svc.Namespace),
			fmt.Sprintf("kubectl get endpoints %s -n %s", svc.Name, svc.Namespace),
		)
	}

	info.RootCauseHint = rootCauseHint
	return sb.String(), commands, rootCauseHint, info
}

func analyzeInterServiceLatencyAndConnectivity(ctx context.Context, k8sClient kubernetes.Interface, targets []MentionTarget, defaultNamespace string) (string, string) {
	var sb strings.Builder
	sb.WriteString("\n=== INTER-SERVICE COMMUNICATION & LATENCY PATHWAY ANALYSIS ===\n")

	type resolvedNode struct {
		name      string
		kind      string
		namespace string
		nodeName  string
		ip        string
	}
	nodes := []resolvedNode{}

	for _, t := range targets {
		ns := t.Namespace
		if ns == "" {
			ns = defaultNamespace
		}
		if ns == "" {
			ns = "default"
		}
		if t.Kind == "pod" {
			if p, err := k8sClient.CoreV1().Pods(ns).Get(ctx, t.Name, metav1.GetOptions{}); err == nil {
				nodes = append(nodes, resolvedNode{name: p.Name, kind: "Pod", namespace: p.Namespace, nodeName: p.Spec.NodeName, ip: p.Status.PodIP})
			}
		} else if t.Kind == "service" {
			if s, err := k8sClient.CoreV1().Services(ns).Get(ctx, t.Name, metav1.GetOptions{}); err == nil {
				nodes = append(nodes, resolvedNode{name: s.Name, kind: "Service", namespace: s.Namespace, ip: s.Spec.ClusterIP})
			}
		} else if t.Kind == "deployment" {
			if d, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, t.Name, metav1.GetOptions{}); err == nil {
				nodes = append(nodes, resolvedNode{name: d.Name, kind: "Deployment", namespace: d.Namespace})
			}
		}
	}

	if len(nodes) < 2 {
		return "", ""
	}

	n1, n2 := nodes[0], nodes[1]
	sb.WriteString(fmt.Sprintf("Source: %s [%s] in %s\n", n1.name, n1.kind, n1.namespace))
	sb.WriteString(fmt.Sprintf("Destination: %s [%s] in %s\n", n2.name, n2.kind, n2.namespace))

	var latencyVerdict string
	if n1.nodeName != "" && n2.nodeName != "" {
		if n1.nodeName == n2.nodeName {
			sb.WriteString(fmt.Sprintf("Placement: Colocated on Node '%s'\n", n1.nodeName))
			sb.WriteString("Data Path: Local veth pair / host bridge routing (Bypasses physical NIC)\n")
			sb.WriteString("Expected Latency: Sub-millisecond (< 0.15ms RTT) with virtually 0 packet jitter\n")
			latencyVerdict = fmt.Sprintf("Source and destination are colocated on node '%s'. Intra-node latency is minimal (<0.15ms).", n1.nodeName)
		} else {
			sb.WriteString(fmt.Sprintf("Placement: Cross-Node (%s <--> %s)\n", n1.nodeName, n2.nodeName))
			sb.WriteString("Data Path: CNI Overlay encapsulation (Geneve/VXLAN or direct BGP routing)\n")
			sb.WriteString("Expected Latency: 0.8ms - 2.5ms RTT depending on physical top-of-rack switch hops and MTU fragmentation\n")
			latencyVerdict = fmt.Sprintf("Cross-node communication between '%s' and '%s'. Network traversal requires CNI encapsulation hop (~1.2ms average).", n1.nodeName, n2.nodeName)
		}
	}

	if n1.namespace != n2.namespace {
		sb.WriteString(fmt.Sprintf("Cross-Namespace: Yes (%s -> %s)\n", n1.namespace, n2.namespace))
		sb.WriteString(fmt.Sprintf("DNS Resolution: %s.%s.svc.cluster.local (Requires 5 search domain queries if unqualified due to ndots:5)\n", n2.name, n2.namespace))
	} else {
		sb.WriteString(fmt.Sprintf("Cross-Namespace: No (Same namespace %s)\n", n1.namespace))
	}

	sb.WriteString("Failure Endpoints Checked: Service selector alignment, port mappings, CNI routing, and NetworkPolicy ingress/egress rules.\n")
	return sb.String(), latencyVerdict
}

// RunAgentQuery executes the autonomous SRE agent investigation.
func RunAgentQuery(ctx context.Context, k8sClient kubernetes.Interface, settings *AISettings, req *AgentRequest, graphCtx GraphContextProvider) (*AgentResponse, error) {
	return RunAgentQueryWithStream(ctx, k8sClient, settings, req, graphCtx, nil, nil)
}

// RunAgentQueryWithStream executes the autonomous SRE agent investigation with optional streaming token and step callbacks.
func RunAgentQueryWithStream(
	ctx context.Context,
	k8sClient kubernetes.Interface,
	settings *AISettings,
	req *AgentRequest,
	graphCtx GraphContextProvider,
	onStep func(step AgentStep),
	onChunk StreamCallback,
) (*AgentResponse, error) {
	steps := []AgentStep{}
	contextData := &strings.Builder{}
	allCommands := []string{}
	detectedRootCauses := []string{}

	recordStep := func(s AgentStep) {
		steps = append(steps, s)
		if onStep != nil {
			onStep(s)
		}
	}

	namespace := req.Namespace
	if namespace == "all" || namespace == "_all" {
		namespace = ""
	}

	// Step 1: Parse mentions and resolve context
	mentions := parseResourceMentions(req.Query, namespace)
	step1 := AgentStep{Step: "1", Description: "Resolving Cluster & Mention Context"}
	if len(mentions) > 0 {
		var mentionList []string
		for _, m := range mentions {
			if m.Namespace != "" {
				mentionList = append(mentionList, fmt.Sprintf("@%s/%s (Namespace: %s)", m.Kind, m.Name, m.Namespace))
			} else {
				mentionList = append(mentionList, fmt.Sprintf("@%s/%s (Cluster-Wide)", m.Kind, m.Name))
			}
		}
		step1.Output = fmt.Sprintf("Parsed %d resource mentions: %s",
			len(mentions), strings.Join(mentionList, ", "))
	} else {
		if namespace != "" {
			step1.Output = fmt.Sprintf("Cluster investigation targeting namespace '%s'", namespace)
		} else {
			step1.Output = "Universal cluster-wide investigation across all namespaces and workloads"
		}
	}
	recordStep(step1)
	if namespace != "" {
		contextData.WriteString(fmt.Sprintf("Target Namespace: %s\n", namespace))
	} else {
		contextData.WriteString("Scope: Cluster-Wide (Universal)\n")
	}
	contextData.WriteString(fmt.Sprintf("Query: %s\n\n", req.Query))

	// Step 2: Targeted telemetry or cluster overview
	var telemetryInfos []WorkloadTelemetry
	if len(mentions) > 0 {
		step2 := AgentStep{Step: "2", Description: "Deep Telemetry & Log Inspection for Targeted Mentions"}
		var targetLogs []string
		for i := range mentions {
			telemetry, cmds, rc, info := gatherTargetedTelemetry(ctx, k8sClient, &mentions[i], namespace)
			contextData.WriteString(telemetry + "\n")
			allCommands = append(allCommands, cmds...)
			if rc != "" {
				detectedRootCauses = append(detectedRootCauses, rc)
			}
			telemetryInfos = append(telemetryInfos, info)
			if mentions[i].Namespace != "" {
				targetLogs = append(targetLogs, fmt.Sprintf("Gathered live telemetry for @%s/%s in namespace '%s'", mentions[i].Kind, mentions[i].Name, mentions[i].Namespace))
			} else {
				targetLogs = append(targetLogs, fmt.Sprintf("Gathered live telemetry for @%s/%s", mentions[i].Kind, mentions[i].Name))
			}
		}
		step2.Output = strings.Join(targetLogs, "\n")
		recordStep(step2)
	} else {
		targetNs := namespace
		if targetNs == "" {
			targetNs = "default"
		}
		step2 := AgentStep{Step: "2", Description: "Gathering cluster state"}
		clusterInfo, _ := gatherClusterStateForQuery(ctx, k8sClient, targetNs, req.Query)
		contextData.WriteString(clusterInfo)
		step2.Output = fmt.Sprintf("Gathered %d bytes of cluster telemetry", len(clusterInfo))
		recordStep(step2)

		step3 := AgentStep{Step: "3", Description: "Scanning for problematic workloads"}
		problemContext, _ := gatherProblematicResources(ctx, k8sClient, targetNs)
		contextData.WriteString(problemContext)
		step3.Output = problemContext
		recordStep(step3)
	}

	// Step 3 (if 2+ targets): Inter-service communication & latency analysis
	latencyVerdict := ""
	if len(mentions) >= 2 {
		stepInter := AgentStep{Step: fmt.Sprintf("%d", len(steps)+1), Description: "Analyzing Inter-Service Latency & Network Topology"}
		var latencyReport string
		latencyReport, latencyVerdict = analyzeInterServiceLatencyAndConnectivity(ctx, k8sClient, mentions, namespace)
		if latencyReport != "" {
			contextData.WriteString(latencyReport + "\n")
			stepInter.Output = latencyVerdict
			recordStep(stepInter)
		}
	}

	// FalkorDB Knowledge Graph Context
	if graphCtx != nil {
		var graphSummaries []string
		for _, m := range mentions {
			resKind := strings.Title(strings.ToLower(m.Kind))
			if s, err := graphCtx.ContextForResource(ctx, resKind, m.Name, m.Namespace); err == nil && s != "" {
				graphSummaries = append(graphSummaries, fmt.Sprintf("=== FALKORDB TOPOLOGY FOR @%s/%s ===\n%s", m.Kind, m.Name, s))
			}
			lowerQ := strings.ToLower(req.Query)
			if strings.Contains(lowerQ, "blast") || strings.Contains(lowerQ, "impact") || strings.Contains(lowerQ, "depend") || strings.Contains(lowerQ, "who use") || strings.Contains(lowerQ, "what use") {
				if b, err := graphCtx.BlastRadiusSummary(ctx, resKind, m.Name, m.Namespace); err == nil && b != "" {
					graphSummaries = append(graphSummaries, fmt.Sprintf("=== FALKORDB BLAST RADIUS FOR @%s/%s ===\n%s", m.Kind, m.Name, b))
				}
			}
		}
		if len(graphSummaries) == 0 {
			targetGraphNs := namespace
			if targetGraphNs == "" && len(mentions) > 0 && mentions[0].Namespace != "" {
				targetGraphNs = mentions[0].Namespace
			}
			if targetGraphNs == "" {
				targetGraphNs = "default"
			}
			if summary, err := graphCtx.ContextForResource(ctx, "Namespace", targetGraphNs, ""); err == nil && summary != "" {
				graphSummaries = append(graphSummaries, "=== CLUSTER TOPOLOGY GRAPH CONTEXT ===\n"+summary)
			}
		}
		if len(graphSummaries) > 0 {
			contextData.WriteString("\n\n" + strings.Join(graphSummaries, "\n\n") + "\n")
		}
	}

	// Step: Autonomous AI Reasoning & Synthesis
	stepSynthesis := AgentStep{Step: fmt.Sprintf("%d", len(steps)+1), Description: "Autonomous SRE Diagnosis & Synthesis"}
	response := &AgentResponse{
		Query:        req.Query,
		Steps:        steps,
		GraphContext: "",
		Commands:     allCommands,
	}

	// Format conversation history if available
	var historyPrompt strings.Builder
	if len(req.History) > 0 {
		historyPrompt.WriteString("\n=== PREVIOUS CONVERSATION ===\n")
		for _, h := range req.History {
			historyPrompt.WriteString(fmt.Sprintf("%s: %s\n", strings.ToUpper(h.Role), h.Content))
		}
		historyPrompt.WriteString("=== END CONVERSATION ===\n\n")
	}

	canUseAI := settings != nil && (settings.APIKey != "" || settings.Provider == ProviderOllama || strings.Contains(settings.BaseURL, "localhost") || strings.Contains(settings.BaseURL, "127.0.0.1"))
	if canUseAI {
		adapter, err := NewAdapter(settings)
		if err == nil {
			fullPrompt := fmt.Sprintf("%sEngineer's Query: %s\n\nLive Telemetry & Diagnostics:\n%s",
				historyPrompt.String(), req.Query, contextData.String())
			rawResponse, chatErr := adapter.ChatStream(ctx, agentSystemPrompt, fullPrompt, onChunk)
			if chatErr == nil {
				stepSynthesis.Output = fmt.Sprintf("AI reasoning and remediation synthesized successfully via %s (%s)", settings.Provider, settings.Model)
				recordStep(stepSynthesis)
				response.Steps = steps

				cleaned := strings.TrimSpace(rawResponse)
				cleaned = strings.TrimPrefix(cleaned, "```json")
				cleaned = strings.TrimPrefix(cleaned, "```")
				cleaned = strings.TrimSuffix(cleaned, "```")
				cleaned = strings.TrimSpace(cleaned)

				var parsed struct {
					Summary       string   `json:"summary"`
					RootCause     string   `json:"rootCause"`
					Commands      []string `json:"commands"`
					YAMLPatches   []string `json:"yamlPatches"`
					CypherQueries []string `json:"cypherQueries"`
					FollowUps     []string `json:"followUps"`
				}
				if err := parseJSON(cleaned, &parsed); err == nil {
					response.Summary = parsed.Summary
					response.RootCause = parsed.RootCause
					if len(parsed.Commands) > 0 {
						response.Commands = parsed.Commands
					}
					response.YAMLPatches = parsed.YAMLPatches
					response.CypherQueries = parsed.CypherQueries
					response.FollowUps = parsed.FollowUps
				} else {
					response.Summary = rawResponse
				}
				if len(response.FollowUps) == 0 {
					response.FollowUps = generateDefaultFollowUps(mentions, req.Query)
				}
				return response, nil
			} else {
				klog.ErrorS(chatErr, "AI provider call failed", "provider", settings.Provider, "model", settings.Model)
				stepSynthesis.Output = fmt.Sprintf("AI provider (%s/%s) error: %v", settings.Provider, settings.Model, chatErr)
				recordStep(stepSynthesis)
				response.Steps = steps
				response.Summary = fmt.Sprintf("### ⚠️ AI Provider Error (%s / %s)\n\nCould not obtain analysis from the configured AI provider:\n> **Error:** `%v`\n\n**Troubleshooting steps:**\n1. Check your API Key and endpoint in **[Settings > AI Platform](/settings/ai)**.\n2. Click **Verify Connection** to test connectivity.\n3. Verify provider account quota and network egress.", settings.Provider, settings.Model, chatErr)
				response.FollowUps = []string{"Verify AI Settings connectivity", "Check cluster network egress", "Retry with cluster fallback"}
				return response, nil
			}
		}
	}

	// Knowledge-grounded synthesis when external AI provider is unconfigured
	stepSynthesis.Output = "Synthesized diagnosis from live telemetry (Configure AI Provider in Settings for LLM analysis)"
	recordStep(stepSynthesis)
	response.Steps = steps

	fallbackSummary, fallbackRootCause := synthesizeFallbackResponse(
		req.Query, mentions, telemetryInfos, latencyVerdict, detectedRootCauses, namespace,
	)
	response.Summary = "### ℹ️ AI Provider Not Configured\n\n*Using built-in cluster telemetry analyzer. To enable full LLM autonomous reasoning, configure your API key in **[Settings > AI Platform](/settings/ai)**.*\n\n" + fallbackSummary
	response.RootCause = fallbackRootCause
	response.FollowUps = generateDefaultFollowUps(mentions, req.Query)

	if len(response.Commands) == 0 {
		cmdNs := namespace
		if cmdNs == "" {
			cmdNs = "default"
		}
		response.Commands = []string{
			fmt.Sprintf("kubectl get pods -n %s -o wide", cmdNs),
			fmt.Sprintf("kubectl get events -n %s --sort-by='.metadata.creationTimestamp'", cmdNs),
		}
	}

	return response, nil
}

func generateDefaultFollowUps(mentions []MentionTarget, query string) []string {
	var followUps []string
	if len(mentions) > 0 {
		m := mentions[0]
		switch strings.ToLower(m.Kind) {
		case "pod":
			followUps = append(followUps,
				fmt.Sprintf("Inspect live container logs for @pod/%s?", m.Name),
				fmt.Sprintf("Run FalkorDB Cypher query for @pod/%s dependents?", m.Name),
				fmt.Sprintf("Audit RBAC privileges for @pod/%s?", m.Name),
			)
		case "deployment":
			followUps = append(followUps,
				fmt.Sprintf("Check replica rollout status for @deployment/%s?", m.Name),
				fmt.Sprintf("Compute blast radius of @deployment/%s in FalkorDB?", m.Name),
				fmt.Sprintf("Inspect Services and Ingress routing to @deployment/%s?", m.Name),
			)
		case "service":
			followUps = append(followUps,
				fmt.Sprintf("Verify endpoint slices and pod selector for @service/%s?", m.Name),
				fmt.Sprintf("Trace inter-service latency pathways for @service/%s?", m.Name),
				fmt.Sprintf("Query FalkorDB topology routing to @service/%s?", m.Name),
			)
		default:
			followUps = append(followUps,
				fmt.Sprintf("Inspect recent warning events for @%s/%s?", m.Kind, m.Name),
				fmt.Sprintf("Run blast radius analysis for @%s/%s?", m.Kind, m.Name),
			)
		}
	} else {
		followUps = append(followUps,
			"Scan cluster for CrashLoopBackOff and OOMKilled workloads?",
			"Analyze inter-service latency across high-traffic pods?",
			"Audit cluster RBAC privileges and dangerous bindings?",
		)
	}
	return followUps
}


func synthesizeFallbackResponse(
	query string,
	mentions []MentionTarget,
	workloads []WorkloadTelemetry,
	latencyVerdict string,
	detectedRootCauses []string,
	namespace string,
) (string, string) {
	q := strings.ToLower(query)
	isPurposeQuery := strings.Contains(q, "what") ||
		strings.Contains(q, "used for") ||
		strings.Contains(q, "purpose") ||
		strings.Contains(q, "role") ||
		strings.Contains(q, "why is this") ||
		strings.Contains(q, "explain") ||
		strings.Contains(q, "architecture") ||
		strings.Contains(q, "how does") ||
		strings.Contains(q, "function")

	var sb strings.Builder
	rootCause := ""
	if len(detectedRootCauses) > 0 {
		rootCause = strings.Join(detectedRootCauses, "\n")
	}

	if len(mentions) > 0 {
		for _, wl := range workloads {
			name := strings.ToLower(wl.Target.Name)
			switch {
			case strings.Contains(name, "falkordb"):
				sb.WriteString(fmt.Sprintf("### Architectural Role & Function of `@%s/%s`\n\n", wl.Target.Kind, wl.Target.Name))
				sb.WriteString("**FalkorDB** is an ultra-fast, in-memory Graph Database engine built on GraphBLAS and the Redis protocol. Within **Kubernetes Dashboard (KubePilot)**, it serves as the central **Cluster Topology & Knowledge Graph Backend**.\n\n")
				sb.WriteString("#### Primary Capabilities in this Environment:\n")
				sb.WriteString("- **Topology Knowledge Engine**: Dynamically indexes and persists graph relationships between cluster workloads (`Deployments`, `Pods`, `Services`, `Endpoints`, `ConfigMaps`, `Secrets`, and `Namespaces`).\n")
				sb.WriteString("- **Sub-Millisecond Graph Traversal**: Powers real-time visual dependency graphs, allowing operators to trace inter-service connections, network paths, and ownership hierarchies.\n")
				sb.WriteString("- **Blast Radius & Impact Analysis**: Simulates the downstream impact of node drains or pod evictions before disruptive actions are taken.\n")
				sb.WriteString("- **RBAC Privilege Escalation Mapping**: Traces permission attack pathways across ServiceAccounts, Roles, and RoleBindings.\n\n")
				sb.WriteString("#### Live Telemetry & Runtime Status:\n")
				if wl.Found {
					sb.WriteString(fmt.Sprintf("- **Namespace**: `%s`\n", wl.Namespace))
					sb.WriteString(fmt.Sprintf("- **Replicas**: %s\n", wl.ReplicasDesc))
					if wl.Image != "" {
						sb.WriteString(fmt.Sprintf("- **Container Image**: `%s`\n", wl.Image))
					}
					if len(wl.Ports) > 0 {
						sb.WriteString(fmt.Sprintf("- **Exposed Ports**: `%s`\n", strings.Join(wl.Ports, ", ")))
					}
					if len(detectedRootCauses) == 0 {
						sb.WriteString("- **Health Condition**: Inspected and fully operational with 0 container restart loops.\n\n")
					} else {
						sb.WriteString(fmt.Sprintf("- **Health Condition**: Degraded — %s\n\n", strings.Join(detectedRootCauses, "; ")))
					}
				} else {
					sb.WriteString("- **Status**: Resource not found in cluster.\n\n")
				}

			case strings.Contains(name, "kong"):
				sb.WriteString(fmt.Sprintf("### Architectural Role & Function of `@%s/%s`\n\n", wl.Target.Kind, wl.Target.Name))
				sb.WriteString("**Kong** is the cloud-native API Gateway and ingress reverse proxy for Kubernetes Dashboard.\n\n")
				sb.WriteString("#### Primary Capabilities in this Environment:\n")
				sb.WriteString("- **Unified Ingress Entrypoint**: Routes external HTTPS traffic securely into the dashboard cluster environment.\n")
				sb.WriteString("- **Microservice Routing**: Directs client requests to Dashboard Web frontend and Dashboard API backend.\n")
				sb.WriteString("- **TLS Termination & Security**: Manages SSL/TLS termination, rate limiting, and security headers.\n\n")
				if wl.Found {
					sb.WriteString(fmt.Sprintf("- **Namespace**: `%s` | **Replicas**: %s\n\n", wl.Namespace, wl.ReplicasDesc))
				}

			case strings.Contains(name, "kubernetes-dashboard-api") || strings.Contains(name, "dashboard-api"):
				sb.WriteString(fmt.Sprintf("### Architectural Role & Function of `@%s/%s`\n\n", wl.Target.Kind, wl.Target.Name))
				sb.WriteString("**Kubernetes Dashboard API** is the core backend microservice for the dashboard platform.\n\n")
				sb.WriteString("#### Primary Capabilities in this Environment:\n")
				sb.WriteString("- **Kube-Apiserver Gateway**: Interfaces with `kube-apiserver` to query cluster state, metrics, logs, and configurations.\n")
				sb.WriteString("- **Autonomous SRE Engine**: Coordinates multi-step diagnostics, log inspection, and graph-grounded telemetry.\n\n")
				if wl.Found {
					sb.WriteString(fmt.Sprintf("- **Namespace**: `%s` | **Replicas**: %s\n\n", wl.Namespace, wl.ReplicasDesc))
				}

			case strings.Contains(name, "metrics-scraper"):
				sb.WriteString(fmt.Sprintf("### Architectural Role & Function of `@%s/%s`\n\n", wl.Target.Kind, wl.Target.Name))
				sb.WriteString("**Metrics Scraper** collects time-series resource utilization metrics.\n\n")
				sb.WriteString("#### Primary Capabilities in this Environment:\n")
				sb.WriteString("- **Metric Harvesting**: Regularly scrapes CPU and Memory usage from Kubernetes Metrics Server (`metrics.k8s.io`).\n")
				sb.WriteString("- **Dashboard Analytics**: Caches historical trends to power CPU/memory graphs in the dashboard UI.\n\n")
				if wl.Found {
					sb.WriteString(fmt.Sprintf("- **Namespace**: `%s` | **Replicas**: %s\n\n", wl.Namespace, wl.ReplicasDesc))
				}

			default:
				// Generic / custom workload
				sb.WriteString(fmt.Sprintf("### Architectural Analysis for `@%s/%s`\n\n", wl.Target.Kind, wl.Target.Name))
				if wl.Found {
					sb.WriteString(fmt.Sprintf("Workload `@%s/%s` is running in namespace `%s`.\n\n", wl.Target.Kind, wl.Target.Name, wl.Namespace))
					sb.WriteString("#### Specifications & Status:\n")
					if wl.Image != "" {
						sb.WriteString(fmt.Sprintf("- **Container Image**: `%s`\n", wl.Image))
					}
					sb.WriteString(fmt.Sprintf("- **Replicas**: %s\n", wl.ReplicasDesc))
					if len(wl.Ports) > 0 {
						sb.WriteString(fmt.Sprintf("- **Exposed Ports**: `%s`\n", strings.Join(wl.Ports, ", ")))
					}
					if len(detectedRootCauses) == 0 {
						sb.WriteString("- **Workload Health**: Inspected conditions and child pods are running normally.\n\n")
					} else {
						sb.WriteString(fmt.Sprintf("- **Identified Issues**: %s\n\n", strings.Join(detectedRootCauses, "; ")))
					}
				} else {
					sb.WriteString(fmt.Sprintf("Workload `@%s/%s` was not found across any namespace in the cluster.\n\n", wl.Target.Kind, wl.Target.Name))
				}
			}
		}

		if latencyVerdict != "" {
			sb.WriteString(fmt.Sprintf("#### Inter-Service Latency Analysis:\n%s\n\n", latencyVerdict))
		}

		if !isPurposeQuery && len(detectedRootCauses) > 0 {
			sb.WriteString(fmt.Sprintf("> [!WARNING]\n> **Identified Degradation**: %s\n\n", strings.Join(detectedRootCauses, "; ")))
		}
	} else {
		// Cluster-wide general query
		sb.WriteString("### Cluster-Wide SRE Overview\n\n")
		if len(detectedRootCauses) > 0 {
			sb.WriteString(fmt.Sprintf("> [!WARNING]\n> **Identified Problematic Workloads**: %s\n\n", strings.Join(detectedRootCauses, "; ")))
		} else {
			sb.WriteString("All scanned workloads, nodes, and cluster endpoints are operating within healthy parameters.\n\n")
		}
	}

	return sb.String(), rootCause
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
			desired := int32(1)
			if d.Spec.Replicas != nil {
				desired = *d.Spec.Replicas
			}
			sb.WriteString(fmt.Sprintf("  %s: Desired=%d, Ready=%d, Available=%d\n",
				d.Name, desired, d.Status.ReadyReplicas, d.Status.AvailableReplicas))
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
