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

// Package traffic discovers and maps live service interconnections in the cluster.
package traffic

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// TopologyNode represents a cluster resource in the topology graph.
type TopologyNode struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Kind      string            `json:"kind"` // ingress, service, pod, namespace
	Status    string            `json:"status"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// TopologyEdge represents a directed relationship between two nodes.
type TopologyEdge struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	Type     string `json:"type"` // ingress-to-service, service-to-pod, env-interlink
	Port     string `json:"port,omitempty"`
	Protocol string `json:"protocol,omitempty"` // HTTP, HTTPS, TCP, gRPC
}

// TopologyGraph holds the full service interconnection topology.
type TopologyGraph struct {
	Nodes     []TopologyNode `json:"nodes"`
	Edges     []TopologyEdge `json:"edges"`
	Timestamp int64          `json:"timestamp"`
}

// GetTopology discovers and returns the live service topology for the given namespace.
// Pass namespace="" to discover across all namespaces.
func GetTopology(ctx context.Context, k8sClient kubernetes.Interface, namespace string) (*TopologyGraph, error) {
	graph := &TopologyGraph{
		Timestamp: time.Now().UnixMilli(),
	}

	nodesMap := map[string]TopologyNode{}
	var edges []TopologyEdge
	edgeSet := map[string]bool{}

	addNode := func(n TopologyNode) {
		if _, exists := nodesMap[n.ID]; !exists {
			nodesMap[n.ID] = n
		}
	}

	addEdge := func(e TopologyEdge) {
		key := e.Source + "|" + e.Type + "|" + e.Target
		if !edgeSet[key] {
			edgeSet[key] = true
			edges = append(edges, e)
		}
	}

	nsFilter := namespace

	// ─── Namespaces ───────────────────────────────────────────────
	nsList, _ := k8sClient.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if nsList != nil {
		for _, ns := range nsList.Items {
			if nsFilter != "" && ns.Name != nsFilter {
				continue
			}
			addNode(TopologyNode{
				ID: "ns:" + ns.Name, Name: ns.Name, Kind: "namespace",
				Status: string(ns.Status.Phase),
			})
		}
	}

	// ─── Ingresses ────────────────────────────────────────────────
	ingresses, _ := k8sClient.NetworkingV1().Ingresses(nsFilter).List(ctx, metav1.ListOptions{})
	if ingresses != nil {
		for _, ing := range ingresses.Items {
			ingID := fmt.Sprintf("ingress:%s/%s", ing.Namespace, ing.Name)
			hosts := []string{}
			for _, rule := range ing.Spec.Rules {
				hosts = append(hosts, rule.Host)
			}
			addNode(TopologyNode{
				ID:        ingID,
				Name:      ing.Name + " [" + strings.Join(hosts, ",") + "]",
				Namespace: ing.Namespace,
				Kind:      "ingress",
				Status:    "active",
			})

			for _, rule := range ing.Spec.Rules {
				if rule.HTTP == nil {
					continue
				}
				for _, path := range rule.HTTP.Paths {
					if path.Backend.Service == nil {
						continue
					}
					svcID := fmt.Sprintf("service:%s/%s", ing.Namespace, path.Backend.Service.Name)
					port := ""
					if path.Backend.Service.Port.Number != 0 {
						port = fmt.Sprintf("%d", path.Backend.Service.Port.Number)
					} else {
						port = path.Backend.Service.Port.Name
					}
					addEdge(TopologyEdge{
						ID:       ingID + "->" + svcID,
						Source:   ingID,
						Target:   svcID,
						Type:     "ingress-to-service",
						Port:     port,
						Protocol: "HTTP",
					})
				}
			}
		}
	}

	// ─── Services → Pods ──────────────────────────────────────────
	services, _ := k8sClient.CoreV1().Services(nsFilter).List(ctx, metav1.ListOptions{})
	pods, _ := k8sClient.CoreV1().Pods(nsFilter).List(ctx, metav1.ListOptions{})

	if services != nil {
		for _, svc := range services.Items {
			svcID := fmt.Sprintf("service:%s/%s", svc.Namespace, svc.Name)
			ports := []string{}
			for _, p := range svc.Spec.Ports {
				ports = append(ports, fmt.Sprintf("%d/%s", p.Port, p.Protocol))
			}
			addNode(TopologyNode{
				ID:        svcID,
				Name:      svc.Name,
				Namespace: svc.Namespace,
				Kind:      "service",
				Status:    "active",
				Labels:    svc.Spec.Selector,
			})

			// Service -> Pod edges via label selectors
			if pods != nil && len(svc.Spec.Selector) > 0 {
				for _, pod := range pods.Items {
					if pod.Namespace != svc.Namespace {
						continue
					}
					if labelsMatch(svc.Spec.Selector, pod.Labels) {
						podID := fmt.Sprintf("pod:%s/%s", pod.Namespace, pod.Name)
						phase := string(pod.Status.Phase)
						addNode(TopologyNode{
							ID:        podID,
							Name:      pod.Name,
							Namespace: pod.Namespace,
							Kind:      "pod",
							Status:    phase,
						})
						addEdge(TopologyEdge{
							ID:       svcID + "->" + podID,
							Source:   svcID,
							Target:   podID,
							Type:     "service-to-pod",
							Protocol: "TCP",
						})
					}
				}
			}
		}
	}

	// ─── Inter-service Links via Env Vars ─────────────────────────
	if pods != nil && services != nil {
		// Build a set of service names for fast lookup
		serviceNames := map[string]string{} // name -> svcID
		for _, svc := range services.Items {
			serviceNames[svc.Name] = fmt.Sprintf("service:%s/%s", svc.Namespace, svc.Name)
			serviceNames[svc.Name+"."+svc.Namespace] = fmt.Sprintf("service:%s/%s", svc.Namespace, svc.Name)
			serviceNames[svc.Name+"."+svc.Namespace+".svc.cluster.local"] = fmt.Sprintf("service:%s/%s", svc.Namespace, svc.Name)
		}

		for _, pod := range pods.Items {
			podOwner := getPodOwner(&pod)
			if podOwner == "" {
				continue
			}
			ownerID := fmt.Sprintf("service:%s/%s", pod.Namespace, podOwner)

			for _, container := range pod.Spec.Containers {
				for _, envVar := range container.Env {
					val := strings.ToLower(envVar.Value)
					for svcName, targetSvcID := range serviceNames {
						// Only link to other services, not itself
						if targetSvcID == ownerID {
							continue
						}
						if strings.Contains(val, strings.ToLower(svcName)) {
							addEdge(TopologyEdge{
								ID:     ownerID + "->env->" + targetSvcID,
								Source: ownerID,
								Target: targetSvcID,
								Type:   "env-interlink",
							})
						}
					}
				}
			}
		}
	}

	// Assemble nodes slice
	for _, n := range nodesMap {
		graph.Nodes = append(graph.Nodes, n)
	}
	graph.Edges = edges

	return graph, nil
}

// getPodOwner returns the owning service name for a pod by resolving via owner references.
// This is a best-effort lookup: we look at the pod's labels and see if a service selects it.
func getPodOwner(pod interface{ GetLabels() map[string]string }) string {
	// We use the "app" or "app.kubernetes.io/name" label as a heuristic for the owning service.
	labels := pod.GetLabels()
	if labels == nil {
		return ""
	}
	if name, ok := labels["app"]; ok {
		return name
	}
	if name, ok := labels["app.kubernetes.io/name"]; ok {
		return name
	}
	return ""
}

// labelsMatch checks if all selector labels are present in the target labels.
func labelsMatch(selector, labels map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return len(selector) > 0
}
