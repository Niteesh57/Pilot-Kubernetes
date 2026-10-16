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
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Namespace  string            `json:"namespace"`
	Kind       string            `json:"kind"` // ingress, service, deployment, statefulset, daemonset, job, cronjob, pod, configmap, secret, serviceaccount, pvc
	Status     string            `json:"status"`
	Group      string            `json:"group"` // Application/Workload grouping key
	Labels     map[string]string `json:"labels,omitempty"`
	Properties map[string]string `json:"properties,omitempty"` // metadata: ports, replicas, images, etc.
}

// TopologyEdge represents a directed relationship between two nodes.
type TopologyEdge struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	Type     string `json:"type"` // routes-to, exposes, manages, mounts-config, mounts-secret, uses-account, claims-storage, env-interlink
	Port     string `json:"port,omitempty"`
	Protocol string `json:"protocol,omitempty"` // HTTP, HTTPS, TCP, gRPC
}

// TopologyGraph holds the full service interconnection topology.
type TopologyGraph struct {
	Nodes     []TopologyNode `json:"nodes"`
	Edges     []TopologyEdge `json:"edges"`
	Groups    []string       `json:"groups"` // List of distinct application/workload groups
	Timestamp int64          `json:"timestamp"`
}

// GetTopology discovers and returns the live service topology for the given namespace.
// Pass namespace="" to discover across all namespaces.
func GetTopology(ctx context.Context, k8sClient kubernetes.Interface, namespace string) (*TopologyGraph, error) {
	graph := &TopologyGraph{
		Timestamp: time.Now().UnixMilli(),
		Groups:    []string{},
	}

	nodesMap := map[string]TopologyNode{}
	var edges []TopologyEdge
	edgeSet := map[string]bool{}
	groupSet := map[string]bool{}

	addNode := func(n TopologyNode) {
		if n.Group == "" {
			n.Group = n.Name
		}
		groupSet[n.Group] = true
		if existing, exists := nodesMap[n.ID]; exists {
			// Update properties/group if newly available
			if existing.Group == "" || existing.Group == existing.Name {
				existing.Group = n.Group
				nodesMap[n.ID] = existing
			}
			return
		}
		nodesMap[n.ID] = n
	}

	addEdge := func(e TopologyEdge) {
		key := e.Source + "|" + e.Type + "|" + e.Target
		if !edgeSet[key] {
			edgeSet[key] = true
			edges = append(edges, e)
		}
	}

	nsFilter := namespace

	// ─── 1. Workload Controllers (Deployments, StatefulSets, DaemonSets, Jobs, CronJobs) ───
	deployments, _ := k8sClient.AppsV1().Deployments(nsFilter).List(ctx, metav1.ListOptions{})
	if deployments != nil {
		for _, dep := range deployments.Items {
			depID := fmt.Sprintf("deployment:%s/%s", dep.Namespace, dep.Name)
			status := fmt.Sprintf("%d/%d replicas", dep.Status.ReadyReplicas, dep.Status.Replicas)
			if dep.Status.ReadyReplicas == dep.Status.Replicas && dep.Status.Replicas > 0 {
				status = "Running"
			} else if dep.Status.Replicas == 0 {
				status = "Scaled down"
			} else {
				status = "Degraded"
			}
			addNode(TopologyNode{
				ID:        depID,
				Name:      dep.Name,
				Namespace: dep.Namespace,
				Kind:      "deployment",
				Status:    status,
				Group:     dep.Name,
				Labels:    dep.Labels,
				Properties: map[string]string{
					"replicas": fmt.Sprintf("%d ready / %d total", dep.Status.ReadyReplicas, dep.Status.Replicas),
				},
			})
		}
	}

	statefulSets, _ := k8sClient.AppsV1().StatefulSets(nsFilter).List(ctx, metav1.ListOptions{})
	if statefulSets != nil {
		for _, sts := range statefulSets.Items {
			stsID := fmt.Sprintf("statefulset:%s/%s", sts.Namespace, sts.Name)
			status := fmt.Sprintf("%d/%d replicas", sts.Status.ReadyReplicas, sts.Status.Replicas)
			if sts.Status.ReadyReplicas == sts.Status.Replicas && sts.Status.Replicas > 0 {
				status = "Running"
			}
			addNode(TopologyNode{
				ID:        stsID,
				Name:      sts.Name,
				Namespace: sts.Namespace,
				Kind:      "statefulset",
				Status:    status,
				Group:     sts.Name,
				Labels:    sts.Labels,
			})
		}
	}

	daemonSets, _ := k8sClient.AppsV1().DaemonSets(nsFilter).List(ctx, metav1.ListOptions{})
	if daemonSets != nil {
		for _, ds := range daemonSets.Items {
			dsID := fmt.Sprintf("daemonset:%s/%s", ds.Namespace, ds.Name)
			status := fmt.Sprintf("%d ready", ds.Status.NumberReady)
			addNode(TopologyNode{
				ID:        dsID,
				Name:      ds.Name,
				Namespace: ds.Namespace,
				Kind:      "daemonset",
				Status:    status,
				Group:     ds.Name,
				Labels:    ds.Labels,
			})
		}
	}

	jobs, _ := k8sClient.BatchV1().Jobs(nsFilter).List(ctx, metav1.ListOptions{})
	if jobs != nil {
		for _, job := range jobs.Items {
			jobID := fmt.Sprintf("job:%s/%s", job.Namespace, job.Name)
			status := "Running"
			if job.Status.Succeeded > 0 {
				status = "Completed"
			} else if job.Status.Failed > 0 {
				status = "Failed"
			}
			addNode(TopologyNode{
				ID:        jobID,
				Name:      job.Name,
				Namespace: job.Namespace,
				Kind:      "job",
				Status:    status,
				Group:     job.Name,
				Labels:    job.Labels,
			})
		}
	}

	// ─── 2. Services & Ingresses ──────────────────────────────────
	services, _ := k8sClient.CoreV1().Services(nsFilter).List(ctx, metav1.ListOptions{})
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
				Status:    "Active",
				Group:     svc.Name,
				Labels:    svc.Spec.Selector,
				Properties: map[string]string{
					"clusterIP": svc.Spec.ClusterIP,
					"ports":     strings.Join(ports, ", "),
					"type":      string(svc.Spec.Type),
				},
			})

			// Link Service -> Workload if selector matches deployment/daemonset/statefulset
			if deployments != nil {
				for _, dep := range deployments.Items {
					if dep.Namespace == svc.Namespace && labelsMatch(svc.Spec.Selector, dep.Spec.Template.Labels) {
						depID := fmt.Sprintf("deployment:%s/%s", dep.Namespace, dep.Name)
						addEdge(TopologyEdge{
							ID:       svcID + "->" + depID,
							Source:   svcID,
							Target:   depID,
							Type:     "exposes",
							Protocol: "TCP",
						})
					}
				}
			}
		}
	}

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
				Name:      ing.Name,
				Namespace: ing.Namespace,
				Kind:      "ingress",
				Status:    "Active",
				Group:     ing.Name,
				Properties: map[string]string{
					"hosts": strings.Join(hosts, ", "),
				},
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
						Type:     "routes-to",
						Port:     port,
						Protocol: "HTTP",
					})
				}
			}
		}
	}

	// ─── 3. Pods & Attached Dependencies (ConfigMap, Secret, SA, PVC) ───
	pods, _ := k8sClient.CoreV1().Pods(nsFilter).List(ctx, metav1.ListOptions{})
	if pods != nil {
		for _, pod := range pods.Items {
			podID := fmt.Sprintf("pod:%s/%s", pod.Namespace, pod.Name)
			workloadOwner, workloadKind := getWorkloadOwner(&pod)
			groupName := workloadOwner
			if groupName == "" {
				groupName = getPodOwnerName(&pod)
			}
			if groupName == "" {
				groupName = pod.Name
			}

			phase := string(pod.Status.Phase)
			// Check container restart counts and waiting reasons
			restartCount := int32(0)
			for _, cs := range pod.Status.ContainerStatuses {
				restartCount += cs.RestartCount
				if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
					phase = cs.State.Waiting.Reason
				}
			}

			images := []string{}
			for _, c := range pod.Spec.Containers {
				images = append(images, c.Image)
			}

			addNode(TopologyNode{
				ID:        podID,
				Name:      pod.Name,
				Namespace: pod.Namespace,
				Kind:      "pod",
				Status:    phase,
				Group:     groupName,
				Labels:    pod.Labels,
				Properties: map[string]string{
					"podIP":    pod.Status.PodIP,
					"node":     pod.Spec.NodeName,
					"restarts": fmt.Sprintf("%d", restartCount),
					"images":   strings.Join(images, ", "),
				},
			})

			// Link Workload -> Pod
			if workloadOwner != "" {
				ownerID := fmt.Sprintf("%s:%s/%s", workloadKind, pod.Namespace, workloadOwner)
				addEdge(TopologyEdge{
					ID:     ownerID + "->" + podID,
					Source: ownerID,
					Target: podID,
					Type:   "manages",
				})
			}

			// Link Service -> Pod if Service selects Pod
			if services != nil {
				for _, svc := range services.Items {
					if svc.Namespace == pod.Namespace && labelsMatch(svc.Spec.Selector, pod.Labels) {
						svcID := fmt.Sprintf("service:%s/%s", svc.Namespace, svc.Name)
						addEdge(TopologyEdge{
							ID:       svcID + "->" + podID,
							Source:   svcID,
							Target:   podID,
							Type:     "exposes",
							Protocol: "TCP",
						})
					}
				}
			}

			// Attached ServiceAccount
			if sa := pod.Spec.ServiceAccountName; sa != "" && sa != "default" {
				saID := fmt.Sprintf("serviceaccount:%s/%s", pod.Namespace, sa)
				addNode(TopologyNode{
					ID:        saID,
					Name:      sa,
					Namespace: pod.Namespace,
					Kind:      "serviceaccount",
					Status:    "Bound",
					Group:     groupName,
				})
				addEdge(TopologyEdge{
					ID:     podID + "->sa->" + saID,
					Source: podID,
					Target: saID,
					Type:   "uses-account",
				})
			}

			// Attached Volumes (ConfigMaps, Secrets, PVCs)
			for _, vol := range pod.Spec.Volumes {
				if vol.ConfigMap != nil {
					cmID := fmt.Sprintf("configmap:%s/%s", pod.Namespace, vol.ConfigMap.Name)
					addNode(TopologyNode{
						ID:        cmID,
						Name:      vol.ConfigMap.Name,
						Namespace: pod.Namespace,
						Kind:      "configmap",
						Status:    "Mounted",
						Group:     groupName,
					})
					addEdge(TopologyEdge{
						ID:     podID + "->cm->" + cmID,
						Source: podID,
						Target: cmID,
						Type:   "mounts-config",
					})
				}
				if vol.Secret != nil {
					secID := fmt.Sprintf("secret:%s/%s", pod.Namespace, vol.Secret.SecretName)
					addNode(TopologyNode{
						ID:        secID,
						Name:      vol.Secret.SecretName,
						Namespace: pod.Namespace,
						Kind:      "secret",
						Status:    "Mounted",
						Group:     groupName,
					})
					addEdge(TopologyEdge{
						ID:     podID + "->sec->" + secID,
						Source: podID,
						Target: secID,
						Type:   "mounts-secret",
					})
				}
				if vol.PersistentVolumeClaim != nil {
					pvcID := fmt.Sprintf("pvc:%s/%s", pod.Namespace, vol.PersistentVolumeClaim.ClaimName)
					addNode(TopologyNode{
						ID:        pvcID,
						Name:      vol.PersistentVolumeClaim.ClaimName,
						Namespace: pod.Namespace,
						Kind:      "pvc",
						Status:    "Bound",
						Group:     groupName,
					})
					addEdge(TopologyEdge{
						ID:     podID + "->pvc->" + pvcID,
						Source: podID,
						Target: pvcID,
						Type:   "claims-storage",
					})
				}
			}

			// Environment Variable ConfigMap / Secret References
			for _, container := range pod.Spec.Containers {
				for _, envFrom := range container.EnvFrom {
					if envFrom.ConfigMapRef != nil {
						cmID := fmt.Sprintf("configmap:%s/%s", pod.Namespace, envFrom.ConfigMapRef.Name)
						addNode(TopologyNode{
							ID:        cmID,
							Name:      envFrom.ConfigMapRef.Name,
							Namespace: pod.Namespace,
							Kind:      "configmap",
							Status:    "EnvSource",
							Group:     groupName,
						})
						addEdge(TopologyEdge{
							ID:     podID + "->envcm->" + cmID,
							Source: podID,
							Target: cmID,
							Type:   "mounts-config",
						})
					}
					if envFrom.SecretRef != nil {
						secID := fmt.Sprintf("secret:%s/%s", pod.Namespace, envFrom.SecretRef.Name)
						addNode(TopologyNode{
							ID:        secID,
							Name:      envFrom.SecretRef.Name,
							Namespace: pod.Namespace,
							Kind:      "secret",
							Status:    "EnvSource",
							Group:     groupName,
						})
						addEdge(TopologyEdge{
							ID:     podID + "->envsec->" + secID,
							Source: podID,
							Target: secID,
							Type:   "mounts-secret",
						})
					}
				}
			}
		}
	}

	// ─── 4. Assemble Final Graph ───
	for _, n := range nodesMap {
		graph.Nodes = append(graph.Nodes, n)
	}
	graph.Edges = edges
	for g := range groupSet {
		if g != "" {
			graph.Groups = append(graph.Groups, g)
		}
	}

	return graph, nil
}

// getWorkloadOwner resolves the direct or parent workload controller (Deployment, StatefulSet, Job, etc.)
func getWorkloadOwner(pod metav1.Object) (string, string) {
	for _, ref := range pod.GetOwnerReferences() {
		kind := strings.ToLower(ref.Kind)
		if kind == "replicaset" {
			// ReplicaSet names are typically <deployment>-<hash>, strip hash to get deployment name
			parts := strings.Split(ref.Name, "-")
			if len(parts) > 1 {
				depName := strings.Join(parts[:len(parts)-1], "-")
				return depName, "deployment"
			}
			return ref.Name, "deployment"
		}
		if kind == "statefulset" || kind == "daemonset" || kind == "job" || kind == "cronjob" {
			return ref.Name, kind
		}
	}
	return "", ""
}

// getPodOwnerName returns the owning app label as a heuristic
func getPodOwnerName(pod metav1.Object) string {
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
	if name, ok := labels["app.kubernetes.io/instance"]; ok {
		return name
	}
	return ""
}

// labelsMatch checks if all selector labels match
func labelsMatch(selector, labels map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}
