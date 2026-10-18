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
	"strings"
)

// CatalogQuery is a curated read-only Cypher query surfaced in the UI.
type CatalogQuery struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Cypher      string `json:"cypher"`
}

// DashboardQuery is a saved query inside a dashboard.
type DashboardQuery struct {
	Title  string `json:"title"`
	Cypher string `json:"cypher"`
}

// Dashboard is a saved collection of graph queries.
type Dashboard struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Queries     []DashboardQuery `json:"queries"`
}

// LogEvent is a log line ingested into the graph.
type LogEvent struct {
	ID        string `json:"id"`
	Pod       string `json:"pod"`
	Namespace string `json:"namespace"`
	Container string `json:"container"`
	Level     string `json:"level"`
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
}

// ServiceDiagnosis correlates a service with its pods, metrics, and recent logs.
type ServiceDiagnosis struct {
	Service      GraphNode   `json:"service"`
	Pods         []GraphNode `json:"pods"`
	LogEvents    []LogEvent  `json:"logEvents"`
	Dependencies []GraphNode `json:"dependencies"`
}

// QueryResult represents structured tabular output from a Cypher query.
type QueryResult struct {
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
	Stats   []string   `json:"stats,omitempty"`
}

// QueryTabular executes a Cypher query and parses it into structured columns and clean rows.
func (c *Client) QueryTabular(ctx context.Context, cypher string) (*QueryResult, error) {
	result, err := c.Query(ctx, cypher)
	if err != nil {
		return nil, err
	}
	columns, rawRows, err := parseRows(result)
	if err != nil {
		return &QueryResult{
			Columns: []string{},
			Rows:    [][]string{},
		}, nil
	}

	cleanRows := make([][]string, len(rawRows))
	for rIdx, row := range rawRows {
		cleanRow := make([]string, len(row))
		for cIdx, cell := range row {
			cleanRow[cIdx] = cellToString(cell)
		}
		cleanRows[rIdx] = cleanRow
	}

	return &QueryResult{
		Columns: columns,
		Rows:    cleanRows,
	}, nil
}

// QueryCatalog returns curated Cypher queries for common observability questions.
func (c *Client) QueryCatalog() []CatalogQuery {
	return []CatalogQuery{
		{
			Name:        "All Pods & Status",
			Description: "List all cluster pods with namespace, phase, and container restarts.",
			Cypher:      "MATCH (p:Pod) RETURN p.name, p.namespace, p.phase, p.restarts ORDER BY p.name",
		},
		{
			Name:        "Top CPU Pods",
			Description: "Pods consuming the most CPU in the cluster.",
			Cypher:      "MATCH (p:Pod) RETURN p.name, p.namespace, COALESCE(p.cpuMilli, 0) AS cpu_milli, COALESCE(p.memoryMi, 0) AS mem_mi ORDER BY cpu_milli DESC LIMIT 10",
		},
		{
			Name:        "Pod Restart Counts",
			Description: "Pods sorted by container restart count.",
			Cypher:      "MATCH (p:Pod) RETURN p.name, p.namespace, COALESCE(p.restarts, 0) AS restarts ORDER BY restarts DESC LIMIT 10",
		},
		{
			Name:        "Workloads & Replicas",
			Description: "Deployments and workload replica scaling status.",
			Cypher:      "MATCH (d:Deployment) RETURN d.name, d.namespace, d.replicas, d.desired, d.available, d.status",
		},
		{
			Name:        "Single Replica Workloads",
			Description: "Deployments running a single replica (single point of failure).",
			Cypher:      "MATCH (d:Deployment) WHERE d.desired = 1 OR d.desired = '1' RETURN d.name, d.namespace, d.desired, d.status",
		},
		{
			Name:        "Sensitive RBAC Paths",
			Description: "ServiceAccounts bound to sensitive roles.",
			Cypher:      "MATCH (sa:ServiceAccount)-[:BOUND_BY]->(b)-[:GRANTS]->(r) WHERE r.sensitive = 'true' RETURN sa.name, sa.namespace, r.name, labels(r)",
		},
		{
			Name:        "Service Dependencies",
			Description: "What each service connects to (workloads, storage, config).",
			Cypher:      "MATCH (s:Service)-[*1..2]->(dep) RETURN DISTINCT s.name, s.namespace, dep.name, dep.namespace, labels(dep) LIMIT 20",
		},
	}
}

// ServiceLogEvents returns recent ingested log events for a service's pods.
func (c *Client) ServiceLogEvents(ctx context.Context, namespace, serviceName string, limit int) ([]LogEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	cypher := fmt.Sprintf(
		"MATCH (s:Service {name: '%s', namespace: '%s'})-[:SELECTS]->(p:Pod)-[:EMITTED]->(l:LogEvent) "+
			"RETURN l.name, l.namespace, l.container, l.level, l.timestamp, l.message "+
			"ORDER BY l.timestamp DESC LIMIT %d",
		escapeCypher(serviceName), escapeCypher(namespace), limit,
	)
	result, err := c.Query(ctx, cypher)
	if err != nil {
		return nil, err
	}
	_, rows, err := parseRows(result)
	if err != nil {
		return nil, err
	}

	events := []LogEvent{}
	for _, row := range rows {
		pod := asString(row, 0)
		ns := asString(row, 1)
		container := asString(row, 2)
		level := asString(row, 3)
		timestamp := asString(row, 4)
		message := asString(row, 5)
		events = append(events, LogEvent{
			ID:        fmt.Sprintf("%s/%s/%s/%s", ns, pod, container, timestamp),
			Pod:       pod,
			Namespace: ns,
			Container: container,
			Level:     level,
			Timestamp: timestamp,
			Message:   message,
		})
	}
	return events, nil
}

// DiagnoseService correlates a service with pods, latest metrics, logs, and dependencies.
func (c *Client) DiagnoseService(ctx context.Context, namespace, name string) (*ServiceDiagnosis, error) {
	result := &ServiceDiagnosis{Pods: []GraphNode{}, LogEvents: []LogEvent{}, Dependencies: []GraphNode{}}

	serviceCypher := fmt.Sprintf(
		"MATCH (s:Service {name: '%s', namespace: '%s'}) RETURN s.name, s.namespace, labels(s), s.status LIMIT 1",
		escapeCypher(name), escapeCypher(namespace),
	)
	if nodes, err := c.queryNodes(ctx, serviceCypher); err == nil && len(nodes) > 0 {
		result.Service = nodes[0]
	} else {
		result.Service = GraphNode{ID: nodeID("Service", namespace, name), Name: name, Namespace: namespace, Kind: "Service"}
	}

	if pods, err := c.queryServicePods(ctx, namespace, name); err == nil {
		result.Pods = pods
	}

	if events, err := c.ServiceLogEvents(ctx, namespace, name, 50); err == nil {
		result.LogEvents = events
	}

	dependenciesCypher := fmt.Sprintf(
		"MATCH (s:Service {name: '%s', namespace: '%s'})-[*1..3]->(dep) RETURN DISTINCT dep.name, dep.namespace, labels(dep), dep.status",
		escapeCypher(name), escapeCypher(namespace),
	)
	if deps, err := c.queryNodes(ctx, dependenciesCypher); err == nil {
		result.Dependencies = deps
	}

	return result, nil
}

// ListDashboards returns saved dashboards with their queries in saved order.
func (c *Client) ListDashboards(ctx context.Context) ([]Dashboard, error) {
	result, err := c.Query(ctx, "MATCH (d:Dashboard) OPTIONAL MATCH (d)-[:CONTAINS]->(q:DashboardQuery) RETURN d.name, d.description, q.title, q.cypher, COALESCE(q.order, 0) AS q_order ORDER BY d.name, q_order ASC")
	if err != nil {
		return nil, err
	}
	_, rows, err := parseRows(result)
	if err != nil {
		return nil, err
	}

	index := map[string]*Dashboard{}
	order := []string{}
	for _, row := range rows {
		name := asString(row, 0)
		if name == "" {
			continue
		}
		d, exists := index[name]
		if !exists {
			index[name] = &Dashboard{Name: name, Description: asString(row, 1), Queries: []DashboardQuery{}}
			order = append(order, name)
			d = index[name]
		}
		if title, cypher := asString(row, 2), asString(row, 3); title != "" && cypher != "" {
			d.Queries = append(d.Queries, DashboardQuery{Title: title, Cypher: cypher})
		}
	}

	dashboards := []Dashboard{}
	for _, name := range order {
		dashboards = append(dashboards, *index[name])
	}
	return dashboards, nil
}

// SaveDashboard upserts a dashboard and its queries preserving order.
func (c *Client) SaveDashboard(ctx context.Context, dashboard Dashboard) error {
	cypher := fmt.Sprintf("MERGE (d:Dashboard {name: '%s'}) SET d.description = '%s'",
		escapeCypher(dashboard.Name), escapeCypher(dashboard.Description))
	if err := c.RWQuery(ctx, cypher); err != nil {
		return err
	}

	cypher = fmt.Sprintf("MATCH (d:Dashboard {name: '%s'})-[r:CONTAINS]->(q:DashboardQuery) DETACH DELETE q",
		escapeCypher(dashboard.Name))
	if err := c.RWQuery(ctx, cypher); err != nil {
		return err
	}

	for idx, q := range dashboard.Queries {
		cypher = fmt.Sprintf(
			"MATCH (d:Dashboard {name: '%s'}) CREATE (d)-[:CONTAINS]->(:DashboardQuery {title: '%s', cypher: '%s', order: %d})",
			escapeCypher(dashboard.Name), escapeCypher(q.Title), escapeCypher(q.Cypher), idx,
		)
		if err := c.RWQuery(ctx, cypher); err != nil {
			return err
		}
	}
	return nil
}

// DeleteDashboard removes a saved dashboard and its queries.
func (c *Client) DeleteDashboard(ctx context.Context, name string) error {
	return c.RWQuery(ctx, fmt.Sprintf("MATCH (d:Dashboard {name: '%s'}) DETACH DELETE d", escapeCypher(name)))
}

func (c *Client) queryServicePods(ctx context.Context, namespace, serviceName string) ([]GraphNode, error) {
	cypher := fmt.Sprintf(
		"MATCH (s:Service {name: '%s', namespace: '%s'})-[:SELECTS]->(p:Pod) "+
			"RETURN p.name, p.namespace, labels(p), p.status, p.cpuMilli, p.memoryMi",
		escapeCypher(serviceName), escapeCypher(namespace),
	)
	result, err := c.Query(ctx, cypher)
	if err != nil {
		return nil, err
	}
	_, rows, err := parseRows(result)
	if err != nil {
		return nil, err
	}

	nodes := []GraphNode{}
	for _, row := range rows {
		name := asString(row, 0)
		ns := asString(row, 1)
		kind := primaryLabel(row, 2)
		if name == "" || kind == "" {
			continue
		}
		props := map[string]string{}
		if v := asString(row, 4); v != "" {
			props["cpuMilli"] = v
		}
		if v := asString(row, 5); v != "" {
			props["memoryMi"] = v
		}
		nodes = append(nodes, GraphNode{
			ID:         nodeID(kind, ns, name),
			Name:       name,
			Namespace:  ns,
			Kind:       kind,
			Status:     asString(row, 3),
			Properties: props,
		})
	}
	return nodes, nil
}

// Describe returns a human-readable diagnosis for AI prompts.
func (d *ServiceDiagnosis) Describe() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Diagnosis for Service %s/%s:\n", d.Service.Namespace, d.Service.Name))
	sb.WriteString(fmt.Sprintf("Pods (%d):\n", len(d.Pods)))
	for _, pod := range d.Pods {
		sb.WriteString(fmt.Sprintf("- %s/%s [%s]", pod.Namespace, pod.Name, pod.Status))
		if cpu := pod.Properties["cpuMilli"]; cpu != "" {
			sb.WriteString(fmt.Sprintf(" cpu=%smi", cpu))
		}
		if mem := pod.Properties["memoryMi"]; mem != "" {
			sb.WriteString(fmt.Sprintf(" mem=%sMi", mem))
		}
		sb.WriteString("\n")
	}
	sb.WriteString(fmt.Sprintf("Recent log events (%d):\n", len(d.LogEvents)))
	for _, event := range d.LogEvents {
		sb.WriteString(fmt.Sprintf("- [%s] %s: %s\n", event.Level, event.Pod, event.Message))
	}
	return sb.String()
}
