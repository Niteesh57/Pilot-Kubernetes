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

package graph

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// Syncer continuously syncs Kubernetes cluster state into FalkorDB.
type Syncer struct {
	k8sClient kubernetes.Interface
	graph     *Client
	interval  time.Duration
}

// NewSyncer creates a cluster-to-graph syncer.
func NewSyncer(k8sClient kubernetes.Interface, graph *Client, intervalSeconds int) *Syncer {
	if intervalSeconds <= 0 {
		intervalSeconds = 60
	}
	return &Syncer{
		k8sClient: k8sClient,
		graph:     graph,
		interval:  time.Duration(intervalSeconds) * time.Second,
	}
}

// Start begins continuous synchronization. Should be called in a goroutine.
func (s *Syncer) Start(ctx context.Context) {
	klog.InfoS("FalkorDB cluster syncer started", "interval", s.interval)

	// Initial sync immediately
	if err := s.sync(ctx); err != nil {
		klog.ErrorS(err, "Initial FalkorDB sync failed")
	}

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			klog.InfoS("FalkorDB syncer stopped")
			return
		case <-ticker.C:
			if err := s.sync(ctx); err != nil {
				klog.ErrorS(err, "FalkorDB sync error")
			}
		}
	}
}

// sync performs one full sync cycle.
func (s *Syncer) sync(ctx context.Context) error {
	klog.V(3).InfoS("Syncing cluster state to FalkorDB")

	if err := s.syncNodes(ctx); err != nil {
		klog.ErrorS(err, "Node sync error")
	}
	if err := s.syncNamespaces(ctx); err != nil {
		klog.ErrorS(err, "Namespace sync error")
	}
	if err := s.syncPods(ctx); err != nil {
		klog.ErrorS(err, "Pod sync error")
	}
	if err := s.syncDeployments(ctx); err != nil {
		klog.ErrorS(err, "Deployment sync error")
	}
	if err := s.syncStatefulSets(ctx); err != nil {
		klog.ErrorS(err, "StatefulSet sync error")
	}
	if err := s.syncServices(ctx); err != nil {
		klog.ErrorS(err, "Service sync error")
	}
	if err := s.syncIngresses(ctx); err != nil {
		klog.ErrorS(err, "Ingress sync error")
	}
	if err := s.syncServiceAccounts(ctx); err != nil {
		klog.ErrorS(err, "ServiceAccount sync error")
	}

	klog.V(3).InfoS("FalkorDB sync complete")
	return nil
}

func (s *Syncer) syncNodes(ctx context.Context) error {
	nodes, err := s.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, node := range nodes.Items {
		ready := "NotReady"
		for _, cond := range node.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == "True" {
				ready = "Ready"
			}
		}
		s.graph.UpsertNode(ctx, "Node", node.Name, "", map[string]string{
			"status": ready,
		})
	}
	return nil
}

func (s *Syncer) syncNamespaces(ctx context.Context) error {
	namespaces, err := s.k8sClient.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, ns := range namespaces.Items {
		s.graph.UpsertNode(ctx, "Namespace", ns.Name, "", map[string]string{
			"status": string(ns.Status.Phase),
		})
	}
	return nil
}

func (s *Syncer) syncPods(ctx context.Context) error {
	pods, err := s.k8sClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, pod := range pods.Items {
		phase := string(pod.Status.Phase)
		restarts := int32(0)
		for _, cs := range pod.Status.ContainerStatuses {
			restarts += cs.RestartCount
		}

		s.graph.UpsertNode(ctx, "Pod", pod.Name, pod.Namespace, map[string]string{
			"phase":    phase,
			"nodeName": pod.Spec.NodeName,
			"restarts": fmt.Sprintf("%d", restarts),
		})

		// RUNS_ON -> Node
		if pod.Spec.NodeName != "" {
			s.graph.UpsertEdge(ctx, "Pod", pod.Name, pod.Namespace, "RUNS_ON", "Node", pod.Spec.NodeName, "", nil)
		}

		// IN_NAMESPACE -> Namespace
		s.graph.UpsertEdge(ctx, "Pod", pod.Name, pod.Namespace, "IN_NAMESPACE", "Namespace", pod.Namespace, "", nil)

		// MANAGED_BY -> Deployment/StatefulSet/DaemonSet
		for _, ref := range pod.OwnerReferences {
			s.graph.UpsertEdge(ctx, "Pod", pod.Name, pod.Namespace, "MANAGED_BY", ref.Kind, ref.Name, pod.Namespace, nil)
		}
	}
	return nil
}

func (s *Syncer) syncDeployments(ctx context.Context) error {
	deployments, err := s.k8sClient.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, d := range deployments.Items {
		desired := int32(0)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		s.graph.UpsertNode(ctx, "Deployment", d.Name, d.Namespace, map[string]string{
			"desired":   fmt.Sprintf("%d", desired),
			"ready":     fmt.Sprintf("%d", d.Status.ReadyReplicas),
			"available": fmt.Sprintf("%d", d.Status.AvailableReplicas),
		})
		s.graph.UpsertEdge(ctx, "Deployment", d.Name, d.Namespace, "IN_NAMESPACE", "Namespace", d.Namespace, "", nil)
	}
	return nil
}

func (s *Syncer) syncStatefulSets(ctx context.Context) error {
	statefulSets, err := s.k8sClient.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, ss := range statefulSets.Items {
		s.graph.UpsertNode(ctx, "StatefulSet", ss.Name, ss.Namespace, map[string]string{
			"replicas": fmt.Sprintf("%d", ss.Status.Replicas),
			"ready":    fmt.Sprintf("%d", ss.Status.ReadyReplicas),
		})
	}
	return nil
}

func (s *Syncer) syncServices(ctx context.Context) error {
	services, err := s.k8sClient.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, svc := range services.Items {
		s.graph.UpsertNode(ctx, "Service", svc.Name, svc.Namespace, map[string]string{
			"type":      string(svc.Spec.Type),
			"clusterIP": svc.Spec.ClusterIP,
		})

		// SELECTS -> Pod (via label selector)
		if len(svc.Spec.Selector) > 0 {
			pods, _ := s.k8sClient.CoreV1().Pods(svc.Namespace).List(ctx, metav1.ListOptions{})
			if pods != nil {
				for _, pod := range pods.Items {
					if labelsMatch(svc.Spec.Selector, pod.Labels) {
						s.graph.UpsertEdge(ctx, "Service", svc.Name, svc.Namespace, "SELECTS", "Pod", pod.Name, pod.Namespace, nil)
					}
				}
			}
		}
	}
	return nil
}

func (s *Syncer) syncIngresses(ctx context.Context) error {
	ingresses, err := s.k8sClient.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, ing := range ingresses.Items {
		hosts := ""
		for _, rule := range ing.Spec.Rules {
			hosts += rule.Host + " "
		}
		s.graph.UpsertNode(ctx, "Ingress", ing.Name, ing.Namespace, map[string]string{
			"hosts": hosts,
		})

		// ROUTES_TO -> Service
		for _, rule := range ing.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}
			for _, path := range rule.HTTP.Paths {
				if path.Backend.Service != nil {
					s.graph.UpsertEdge(ctx, "Ingress", ing.Name, ing.Namespace, "ROUTES_TO",
						"Service", path.Backend.Service.Name, ing.Namespace, nil)
				}
			}
		}
	}
	return nil
}

func (s *Syncer) syncServiceAccounts(ctx context.Context) error {
	sas, err := s.k8sClient.CoreV1().ServiceAccounts("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, sa := range sas.Items {
		s.graph.UpsertNode(ctx, "ServiceAccount", sa.Name, sa.Namespace, nil)
	}
	return nil
}

// labelsMatch checks if all selector labels are present and match in the target labels.
func labelsMatch(selector, labels map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}
