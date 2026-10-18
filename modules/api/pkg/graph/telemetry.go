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
	"io"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	metricapi "k8s.io/dashboard/api/pkg/integration/metric/api"
	"k8s.io/dashboard/types"
)

const (
	defaultLogRetention = 30 * time.Minute
	maxLogLinesPerPod   = 20
	maxLogEventsPerPod  = 5
	maxLogPodsPerSync   = 200
)

// TelemetrySyncer ingests live resource metrics and pod logs into the knowledge graph.
type TelemetrySyncer struct {
	k8s      kubernetes.Interface
	metrics  func() metricapi.MetricClient
	graph    func() *Client
	interval time.Duration
}

// NewTelemetrySyncer creates a metrics/log syncer. metrics may return nil until the
// metrics-scraper integration becomes healthy.
func NewTelemetrySyncer(k8s kubernetes.Interface, metrics func() metricapi.MetricClient, graph func() *Client, intervalSeconds int) *TelemetrySyncer {
	if intervalSeconds <= 0 {
		intervalSeconds = 60
	}
	return &TelemetrySyncer{
		k8s:      k8s,
		metrics:  metrics,
		graph:    graph,
		interval: time.Duration(intervalSeconds) * time.Second,
	}
}

// Start begins continuous telemetry synchronization.
func (s *TelemetrySyncer) Start(ctx context.Context) {
	if err := s.sync(ctx); err != nil {
		klog.ErrorS(err, "Initial telemetry sync failed")
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.sync(ctx); err != nil {
				klog.ErrorS(err, "Telemetry sync error")
			}
		}
	}
}

// SyncNow performs an immediate telemetry sync.
func (s *TelemetrySyncer) SyncNow(ctx context.Context) error {
	return s.sync(ctx)
}

func (s *TelemetrySyncer) sync(ctx context.Context) error {
	if s.graph() == nil {
		return nil
	}
	if err := s.syncPodMetrics(ctx); err != nil {
		klog.ErrorS(err, "Pod metric sync error")
	}
	if err := s.syncNodeMetrics(ctx); err != nil {
		klog.ErrorS(err, "Node metric sync error")
	}
	if err := s.syncPodLogs(ctx); err != nil {
		klog.ErrorS(err, "Pod log sync error")
	}
	if err := s.cullTelemetry(ctx); err != nil {
		klog.ErrorS(err, "Telemetry cull error")
	}
	return nil
}

func (s *TelemetrySyncer) syncPodMetrics(ctx context.Context) error {
	client := s.metrics()
	if client == nil {
		return nil
	}
	pods, err := s.k8s.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	if len(pods.Items) == 0 {
		return nil
	}

	selectors := make([]metricapi.ResourceSelector, 0, len(pods.Items))
	for _, pod := range pods.Items {
		selectors = append(selectors, metricapi.ResourceSelector{
			Namespace:    pod.Namespace,
			ResourceType: types.ResourceKindPod,
			ResourceName: pod.Name,
			UID:          pod.UID,
		})
	}
	cached := &metricapi.CachedResources{Pods: pods.Items}

	cpuMetrics, _ := client.DownloadMetric(selectors, metricapi.CpuUsage, cached).GetMetrics()
	memMetrics, _ := client.DownloadMetric(selectors, metricapi.MemoryUsage, cached).GetMetrics()

	for i, pod := range pods.Items {
		cpu := latestMetricValue(cpuMetrics, i)
		mem := latestMetricValue(memMetrics, i)
		if cpu < 0 && mem < 0 {
			continue
		}
		props := map[string]string{"metricTimestamp": strconv.FormatInt(time.Now().Unix(), 10)}
		if cpu >= 0 {
			props["cpuMilli"] = strconv.FormatInt(cpu, 10)
		}
		if mem >= 0 {
			props["memoryMi"] = strconv.FormatInt(mem, 10)
		}
		s.graph().UpsertNode(ctx, "Pod", pod.Name, pod.Namespace, props)
	}
	return nil
}

func (s *TelemetrySyncer) syncNodeMetrics(ctx context.Context) error {
	client := s.metrics()
	if client == nil {
		return nil
	}
	nodes, err := s.k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	selectors := make([]metricapi.ResourceSelector, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		selectors = append(selectors, metricapi.ResourceSelector{
			ResourceType: types.ResourceKindNode,
			ResourceName: node.Name,
			UID:          node.UID,
		})
	}
	if len(selectors) == 0 {
		return nil
	}

	cpuMetrics, _ := client.DownloadMetric(selectors, metricapi.CpuUsage, metricapi.NoResourceCache).GetMetrics()
	memMetrics, _ := client.DownloadMetric(selectors, metricapi.MemoryUsage, metricapi.NoResourceCache).GetMetrics()

	for i, node := range nodes.Items {
		cpu := latestMetricValue(cpuMetrics, i)
		mem := latestMetricValue(memMetrics, i)
		if cpu < 0 && mem < 0 {
			continue
		}
		props := map[string]string{"metricTimestamp": strconv.FormatInt(time.Now().Unix(), 10)}
		if cpu >= 0 {
			props["cpuMilli"] = strconv.FormatInt(cpu, 10)
		}
		if mem >= 0 {
			props["memoryMi"] = strconv.FormatInt(mem, 10)
		}
		s.graph().UpsertNode(ctx, "Node", node.Name, "", props)
	}
	return nil
}

func (s *TelemetrySyncer) syncPodLogs(ctx context.Context) error {
	pods, err := s.k8s.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	processed := 0
	tail := int64(maxLogLinesPerPod)
	for _, pod := range pods.Items {
		if processed >= maxLogPodsPerSync {
			break
		}
		processed++

		container := firstContainerName(&pod)
		if container == "" {
			continue
		}
		stream, err := s.k8s.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container:  container,
			TailLines:  &tail,
			Timestamps: true,
		}).Stream(ctx)
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(stream, 64*1024))
		stream.Close()

		events := 0
		for _, line := range strings.Split(string(data), "\n") {
			level, ok := classifyLogLine(line)
			if !ok {
				continue
			}
			s.upsertLogEvent(ctx, pod.Name, pod.Namespace, container, line, level)
			events++
			if events >= maxLogEventsPerPod {
				break
			}
		}
	}
	return nil
}

func (s *TelemetrySyncer) upsertLogEvent(ctx context.Context, podName, namespace, container, message, level string) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	cypher := fmt.Sprintf(
		"MATCH (p:Pod {name: '%s', namespace: '%s'}) "+
			"CREATE (p)-[:EMITTED]->(l:LogEvent {name: '%s', namespace: '%s', container: '%s', level: '%s', timestamp: %s, message: '%s'})",
		escapeCypher(podName), escapeCypher(namespace), escapeCypher(podName), escapeCypher(namespace),
		escapeCypher(container), escapeCypher(level), timestamp, escapeCypher(truncate(message, 1000)),
	)
	if err := s.graph().RWQuery(ctx, cypher); err != nil {
		klog.V(4).InfoS("FalkorDB log event upsert failed", "pod", podName, "error", err)
	}
}

func (s *TelemetrySyncer) cullTelemetry(ctx context.Context) error {
	cutoff := time.Now().Add(-defaultLogRetention).Unix()
	cypher := fmt.Sprintf("MATCH (l:LogEvent) WHERE l.timestamp < %d DETACH DELETE l", cutoff)
	return s.graph().RWQuery(ctx, cypher)
}

func latestMetricValue(metrics []metricapi.Metric, index int) int64 {
	if index < 0 || index >= len(metrics) {
		return -1
	}
	points := metrics[index].MetricPoints
	if len(points) == 0 {
		return -1
	}
	return int64(points[len(points)-1].Value)
}

func firstContainerName(pod *corev1.Pod) string {
	if len(pod.Spec.Containers) == 0 {
		return ""
	}
	return pod.Spec.Containers[0].Name
}

func classifyLogLine(line string) (string, bool) {
	lower := strings.ToLower(line)
	level := "warning"
	switch {
	case strings.Contains(lower, "fatal"), strings.Contains(lower, "panic"),
		strings.Contains(lower, "exception"), strings.Contains(lower, "error"):
		level = "error"
	case strings.Contains(lower, "warn"), strings.Contains(lower, "timeout"),
		strings.Contains(lower, "refused"), strings.Contains(lower, "failed"),
		strings.Contains(lower, "crash"), strings.Contains(lower, "denied"):
		level = "warning"
	default:
		return "", false
	}
	return level, true
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}
