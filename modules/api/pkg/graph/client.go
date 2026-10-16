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

// Package graph provides FalkorDB knowledge graph integration for cluster topology.
package graph

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"
	"k8s.io/klog/v2"
)

var (
	defaultClientMu sync.RWMutex
	defaultClient   *Client
)

// SetDefaultClient sets the process-wide FalkorDB client used by handlers.
func SetDefaultClient(c *Client) {
	defaultClientMu.Lock()
	defer defaultClientMu.Unlock()
	defaultClient = c
}

// DefaultClient returns the process-wide FalkorDB client, or nil when not configured.
func DefaultClient() *Client {
	defaultClientMu.RLock()
	defer defaultClientMu.RUnlock()
	return defaultClient
}

// Client wraps FalkorDB via the Redis protocol.
type Client struct {
	rdb       *redis.Client
	graphName string
}

// FalkorDBStatus describes the connection health.
type FalkorDBStatus struct {
	Connected bool   `json:"connected"`
	GraphName string `json:"graphName"`
	NodeCount int    `json:"nodeCount"`
	EdgeCount int    `json:"edgeCount"`
	Error     string `json:"error,omitempty"`
}

// NewClient creates a FalkorDB client using the Redis protocol.
func NewClient(addr, password, graphName string) *Client {
	rdb := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
	})
	if graphName == "" {
		graphName = "kubepilot"
	}
	return &Client{rdb: rdb, graphName: graphName}
}

// Ping checks connectivity to FalkorDB.
func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

// Close terminates the connection.
func (c *Client) Close() error {
	return c.rdb.Close()
}

// Query executes a Cypher read query against the graph and returns raw result.
func (c *Client) Query(ctx context.Context, cypher string) (interface{}, error) {
	result, err := c.rdb.Do(ctx, "GRAPH.QUERY", c.graphName, cypher, "--compact").Result()
	if err != nil {
		return nil, fmt.Errorf("FalkorDB query error: %w", err)
	}
	return result, nil
}

// RWQuery executes a Cypher write query.
func (c *Client) RWQuery(ctx context.Context, cypher string) error {
	_, err := c.rdb.Do(ctx, "GRAPH.QUERY", c.graphName, cypher).Result()
	return err
}

// Status returns FalkorDB connection status and basic graph statistics.
func (c *Client) Status(ctx context.Context) *FalkorDBStatus {
	if err := c.Ping(ctx); err != nil {
		return &FalkorDBStatus{Connected: false, Error: err.Error()}
	}

	nodeCount := 0
	edgeCount := 0

	if result, err := c.Query(ctx, "MATCH (n) RETURN count(n)"); err == nil {
		nodeCount = extractSingleInt(result)
	}
	if result, err := c.Query(ctx, "MATCH ()-[r]->() RETURN count(r)"); err == nil {
		edgeCount = extractSingleInt(result)
	}

	return &FalkorDBStatus{
		Connected: true,
		GraphName: c.graphName,
		NodeCount: nodeCount,
		EdgeCount: edgeCount,
	}
}

// UpsertNode creates or updates a node with the given labels and properties.
func (c *Client) UpsertNode(ctx context.Context, label, name, namespace string, props map[string]string) {
	setParts := []string{fmt.Sprintf("n.name = '%s'", escapeCypher(name))}
	if namespace != "" {
		setParts = append(setParts, fmt.Sprintf("n.namespace = '%s'", escapeCypher(namespace)))
	}
	for k, v := range props {
		setParts = append(setParts, fmt.Sprintf("n.%s = '%s'", k, escapeCypher(v)))
	}
	setClause := strings.Join(setParts, ", ")

	cypher := fmt.Sprintf(
		"MERGE (n:%s {name: '%s', namespace: '%s'}) SET %s",
		label, escapeCypher(name), escapeCypher(namespace), setClause,
	)
	if err := c.RWQuery(ctx, cypher); err != nil {
		klog.V(4).InfoS("FalkorDB upsert node failed", "label", label, "name", name, "error", err)
	}
}

// UpsertEdge creates or updates a directed relationship between two nodes.
func (c *Client) UpsertEdge(ctx context.Context, fromLabel, fromName, fromNS, relType, toLabel, toName, toNS string, props map[string]string) {
	cypher := fmt.Sprintf(
		"MATCH (a:%s {name: '%s', namespace: '%s'}), (b:%s {name: '%s', namespace: '%s'}) "+
			"MERGE (a)-[r:%s]->(b)",
		fromLabel, escapeCypher(fromName), escapeCypher(fromNS),
		toLabel, escapeCypher(toName), escapeCypher(toNS),
		relType,
	)
	if err := c.RWQuery(ctx, cypher); err != nil {
		klog.V(4).InfoS("FalkorDB upsert edge failed", "rel", relType, "from", fromName, "to", toName, "error", err)
	}
}

// DeleteNode removes a node by label and name.
func (c *Client) DeleteNode(ctx context.Context, label, name, namespace string) {
	cypher := fmt.Sprintf(
		"MATCH (n:%s {name: '%s', namespace: '%s'}) DETACH DELETE n",
		label, escapeCypher(name), escapeCypher(namespace),
	)
	if err := c.RWQuery(ctx, cypher); err != nil {
		klog.V(4).InfoS("FalkorDB delete node failed", "error", err)
	}
}

// ClearGraph removes all nodes and edges from the graph (use carefully).
func (c *Client) ClearGraph(ctx context.Context) error {
	return c.RWQuery(ctx, "MATCH (n) DETACH DELETE n")
}

// escapeCypher escapes single quotes in Cypher string values.
func escapeCypher(s string) string {
	return strings.ReplaceAll(s, "'", "\\'")
}

// extractSingleInt attempts to pull a single integer from a FalkorDB result.
func extractSingleInt(result interface{}) int {
	_, rows, err := parseRows(result)
	if err != nil || len(rows) == 0 || len(rows[0]) == 0 {
		return 0
	}
	return int(asInt64(rows[0][0]))
}
