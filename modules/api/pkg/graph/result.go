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
	"strconv"
	"strings"
)

// GraphNode is a typed node returned to the frontend.
type GraphNode struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Namespace  string            `json:"namespace"`
	Kind       string            `json:"kind"`
	Status     string            `json:"status,omitempty"`
	Properties map[string]string `json:"properties,omitempty"`
}

// GraphEdge is a typed directed relationship between two nodes.
type GraphEdge struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	Type   string `json:"type"`
}

// GraphResult is a typed subgraph.
type GraphResult struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

// BlastRadiusResult describes what depends on a resource and what it depends on.
type BlastRadiusResult struct {
	Target       GraphNode   `json:"target"`
	Dependents   []GraphNode `json:"dependents"`
	Dependencies []GraphNode `json:"dependencies"`
}

// AttackPath describes a privilege-escalation path through the RBAC graph.
type AttackPath struct {
	ServiceAccount string `json:"serviceAccount"`
	Namespace      string `json:"namespace"`
	Binding        string `json:"binding"`
	BindingKind    string `json:"bindingKind"`
	Role           string `json:"role"`
	RoleKind       string `json:"roleKind"`
	Severity       string `json:"severity"`
}

// Topology returns the full typed cluster graph.
func (c *Client) Topology(ctx context.Context) (*GraphResult, error) {
	nodes, err := c.queryNodes(ctx, "MATCH (n) RETURN n.name, n.namespace, labels(n), n.status")
	if err != nil {
		return nil, err
	}
	edges, err := c.queryEdges(ctx, "MATCH (a)-[r]->(b) RETURN a.name, a.namespace, labels(a), b.name, b.namespace, labels(b), type(r)")
	if err != nil {
		return nil, err
	}
	return &GraphResult{Nodes: nodes, Edges: edges}, nil
}

// Explore returns a typed neighborhood around the given resource.
func (c *Client) Explore(ctx context.Context, kind, name, namespace string) (*GraphResult, error) {
	result := &GraphResult{Nodes: []GraphNode{}, Edges: []GraphEdge{}}
	targetID := nodeID(kind, namespace, name)

	targetCypher := fmt.Sprintf(
		"MATCH (n:%s {name: '%s', namespace: '%s'}) RETURN n.name, n.namespace, labels(n), n.status LIMIT 1",
		kind, escapeCypher(name), escapeCypher(namespace),
	)
	if nodes, err := c.queryNodes(ctx, targetCypher); err == nil {
		result.Nodes = appendUniqueNodes(result.Nodes, nodes)
	}

	neighborCypher := fmt.Sprintf(
		"MATCH (n:%s {name: '%s', namespace: '%s'})-[*1]-(m) RETURN DISTINCT m.name, m.namespace, labels(m), m.status",
		kind, escapeCypher(name), escapeCypher(namespace),
	)
	if nodes, err := c.queryNodes(ctx, neighborCypher); err == nil {
		result.Nodes = appendUniqueNodes(result.Nodes, nodes)
	}

	edgeCypher := fmt.Sprintf(
		"MATCH (a)-[r]->(b) WHERE (a.name = '%s' AND a.namespace = '%s') OR (b.name = '%s' AND b.namespace = '%s') RETURN a.name, a.namespace, labels(a), b.name, b.namespace, labels(b), type(r)",
		escapeCypher(name), escapeCypher(namespace), escapeCypher(name), escapeCypher(namespace),
	)
	if edges, err := c.queryEdges(ctx, edgeCypher); err == nil {
		for _, edge := range edges {
			if edge.Source == targetID || edge.Target == targetID {
				result.Edges = append(result.Edges, edge)
			}
		}
	}

	return result, nil
}

// BlastRadius returns typed dependents (what breaks) and dependencies (what it needs).
func (c *Client) BlastRadius(ctx context.Context, kind, name, namespace string) (*BlastRadiusResult, error) {
	result := &BlastRadiusResult{
		Target:       GraphNode{ID: nodeID(kind, namespace, name), Name: name, Namespace: namespace, Kind: kind},
		Dependents:   []GraphNode{},
		Dependencies: []GraphNode{},
	}

	targetCypher := fmt.Sprintf(
		"MATCH (n:%s {name: '%s', namespace: '%s'}) RETURN n.name, n.namespace, labels(n), n.status LIMIT 1",
		kind, escapeCypher(name), escapeCypher(namespace),
	)
	if nodes, err := c.queryNodes(ctx, targetCypher); err == nil && len(nodes) > 0 {
		result.Target = nodes[0]
	}

	dependentsCypher := fmt.Sprintf(
		"MATCH (n:%s {name: '%s', namespace: '%s'})<-[*1..4]-(d) RETURN DISTINCT d.name, d.namespace, labels(d), d.status",
		kind, escapeCypher(name), escapeCypher(namespace),
	)
	if nodes, err := c.queryNodes(ctx, dependentsCypher); err == nil {
		result.Dependents = nodes
	}

	dependenciesCypher := fmt.Sprintf(
		"MATCH (n:%s {name: '%s', namespace: '%s'})-[*1..4]->(u) RETURN DISTINCT u.name, u.namespace, labels(u), u.status",
		kind, escapeCypher(name), escapeCypher(namespace),
	)
	if nodes, err := c.queryNodes(ctx, dependenciesCypher); err == nil {
		result.Dependencies = nodes
	}

	return result, nil
}

// AttackPaths returns privilege-escalation paths for sensitive RBAC bindings.
func (c *Client) AttackPaths(ctx context.Context, namespace string) ([]AttackPath, error) {
	cypher := "MATCH (sa:ServiceAccount)-[:BOUND_BY]->(b)-[:GRANTS]->(r) " +
		"WHERE (r:Role OR r:ClusterRole) AND r.sensitive = 'true' " +
		"RETURN sa.name, sa.namespace, b.name, labels(b), r.name, labels(r)"
	if namespace != "" {
		cypher = fmt.Sprintf(
			"MATCH (sa:ServiceAccount {namespace: '%s'})-[:BOUND_BY]->(b)-[:GRANTS]->(r) "+
				"WHERE (r:Role OR r:ClusterRole) AND r.sensitive = 'true' "+
				"RETURN sa.name, sa.namespace, b.name, labels(b), r.name, labels(r)",
			escapeCypher(namespace),
		)
	}

	result, err := c.Query(ctx, cypher)
	if err != nil {
		return nil, err
	}
	_, rows, err := parseRows(result)
	if err != nil {
		return nil, err
	}

	paths := []AttackPath{}
	for _, row := range rows {
		roleKind := primaryLabel(row, 5)
		severity := "high"
		if roleKind == "ClusterRole" {
			severity = "critical"
		}
		paths = append(paths, AttackPath{
			ServiceAccount: asString(row, 0),
			Namespace:      asString(row, 1),
			Binding:        asString(row, 2),
			BindingKind:    primaryLabel(row, 3),
			Role:           asString(row, 4),
			RoleKind:       roleKind,
			Severity:       severity,
		})
	}
	return paths, nil
}

func (c *Client) queryNodes(ctx context.Context, cypher string) ([]GraphNode, error) {
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
		namespace := asString(row, 1)
		kind := primaryLabel(row, 2)
		if name == "" || kind == "" {
			continue
		}
		nodes = append(nodes, GraphNode{
			ID:        nodeID(kind, namespace, name),
			Name:      name,
			Namespace: namespace,
			Kind:      kind,
			Status:    asString(row, 3),
		})
	}
	return nodes, nil
}

func (c *Client) queryEdges(ctx context.Context, cypher string) ([]GraphEdge, error) {
	result, err := c.Query(ctx, cypher)
	if err != nil {
		return nil, err
	}
	_, rows, err := parseRows(result)
	if err != nil {
		return nil, err
	}

	edges := []GraphEdge{}
	for _, row := range rows {
		srcKind := primaryLabel(row, 2)
		dstKind := primaryLabel(row, 5)
		if srcKind == "" || dstKind == "" {
			continue
		}
		src := nodeID(srcKind, asString(row, 1), asString(row, 0))
		dst := nodeID(dstKind, asString(row, 4), asString(row, 3))
		edges = append(edges, GraphEdge{
			ID:     src + "->" + dst,
			Source: src,
			Target: dst,
			Type:   asString(row, 6),
		})
	}
	return edges, nil
}

func nodeID(kind, namespace, name string) string {
	return fmt.Sprintf("%s:%s/%s", kind, namespace, name)
}

func appendUniqueNodes(dst []GraphNode, src []GraphNode) []GraphNode {
	seen := map[string]bool{}
	for _, n := range dst {
		seen[n.ID] = true
	}
	for _, n := range src {
		if !seen[n.ID] {
			dst = append(dst, n)
			seen[n.ID] = true
		}
	}
	return dst
}

// parseRows extracts header and rows from a FalkorDB GRAPH.QUERY result, tolerating
// both the [header, rows, stats] and [header, rows] layouts.
func parseRows(result interface{}) ([]string, [][]interface{}, error) {
	arr, ok := result.([]interface{})
	if !ok || len(arr) == 0 {
		return nil, nil, fmt.Errorf("unexpected FalkorDB result shape")
	}

	header := []string{}
	if h, ok := arr[0].([]interface{}); ok {
		for _, col := range h {
			header = append(header, asString(col))
		}
	}

	for i := 1; i < len(arr); i++ {
		rowsArr, ok := arr[i].([]interface{})
		if !ok {
			continue
		}
		rows := [][]interface{}{}
		for _, r := range rowsArr {
			if row, ok := r.([]interface{}); ok {
				rows = append(rows, row)
			}
		}
		return header, rows, nil
	}
	return header, nil, nil
}

// unwrapValue recursively unwraps FalkorDB RESP compact protocol tagged pairs [tag, value].
// In FalkorDB compact mode, a scalar is represented as [type_tag, value].
func unwrapValue(v interface{}) interface{} {
	if v == nil {
		return nil
	}
	arr, ok := v.([]interface{})
	if !ok {
		return v
	}
	if len(arr) == 2 {
		switch arr[0].(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
			tag := asInt64(arr[0])
			if tag == 1 { // Tag 1: NULL
				return nil
			}
			return unwrapValue(arr[1])
		}
	}
	return v
}

func cellToString(v interface{}) string {
	v = unwrapValue(v)
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case []interface{}:
		var parts []string
		for _, item := range t {
			s := cellToString(item)
			if s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	default:
		return fmt.Sprintf("%v", t)
	}
}

func asInt64(v interface{}) int64 {
	v = unwrapValue(v)
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	default:
		return 0
	}
}

func asString(v interface{}, idx ...int) string {
	if len(idx) > 0 {
		colIdx := idx[0]
		if row, ok := v.([]interface{}); ok {
			if colIdx >= 0 && colIdx < len(row) {
				return cellToString(row[colIdx])
			}
			return ""
		}
	}
	return cellToString(v)
}

func asStringSlice(v interface{}) []string {
	v = unwrapValue(v)
	var out []string
	switch t := v.(type) {
	case []interface{}:
		for _, e := range t {
			s := cellToString(e)
			if s != "" {
				out = append(out, s)
			}
		}
	case []string:
		out = append(out, t...)
	case string:
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

func primaryLabel(row []interface{}, idx int) string {
	if idx < 0 || idx >= len(row) {
		return ""
	}
	for _, label := range asStringSlice(row[idx]) {
		if label != "" {
			return label
		}
	}
	return ""
}

// Describe returns a human-readable summary of the subgraph for AI prompts.
func (r *GraphResult) Describe() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Graph neighborhood (%d nodes, %d edges):\n", len(r.Nodes), len(r.Edges)))
	for _, n := range r.Nodes {
		sb.WriteString(fmt.Sprintf("- %s %s/%s", n.Kind, n.Namespace, n.Name))
		if n.Status != "" {
			sb.WriteString(fmt.Sprintf(" [%s]", n.Status))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// Describe returns a human-readable blast radius summary for AI prompts.
func (r *BlastRadiusResult) Describe() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Blast radius for %s %s/%s:\n", r.Target.Kind, r.Target.Namespace, r.Target.Name))
	sb.WriteString(fmt.Sprintf("Dependents (%d):\n", len(r.Dependents)))
	for _, n := range r.Dependents {
		sb.WriteString(fmt.Sprintf("- %s %s/%s\n", n.Kind, n.Namespace, n.Name))
	}
	sb.WriteString(fmt.Sprintf("Dependencies (%d):\n", len(r.Dependencies)))
	for _, n := range r.Dependencies {
		sb.WriteString(fmt.Sprintf("- %s %s/%s\n", n.Kind, n.Namespace, n.Name))
	}
	return sb.String()
}
