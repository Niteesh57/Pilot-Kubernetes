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

import {Component, OnInit} from '@angular/core';
import {AIService, GraphStatus, TopologyEdge, TopologyGraph, TopologyNode} from '@common/services/global/ai.service';

@Component({
  selector: 'kd-traffic-topology',
  templateUrl: './template.html',
  styleUrls: ['./style.scss'],
})
export class TrafficTopologyComponent implements OnInit {
  isLoading = false;
  graph: TopologyGraph | null = null;
  graphStatus: GraphStatus | null = null;
  error: string | null = null;
  selectedNamespace = '';
  searchQuery = '';
  kindFilter = 'ALL';
  selectedNode: TopologyNode | null = null;

  constructor(private readonly aiService: AIService) {}

  ngOnInit(): void {
    this.refresh();
  }

  refresh(): void {
    this.isLoading = true;
    this.error = null;

    this.aiService.getTopology(this.selectedNamespace || undefined).subscribe({
      next: graph => {
        this.graph = graph;
        this.isLoading = false;
      },
      error: err => {
        this.error = err.error?.message || 'Failed to load traffic topology.';
        this.isLoading = false;
      },
    });

    this.aiService.getGraphStatus().subscribe({
      next: st => {
        this.graphStatus = st;
      },
      error: () => {
        // FalkorDB status is optional info
      },
    });
  }

  get filteredNodes(): TopologyNode[] {
    if (!this.graph?.nodes) return [];
    return this.graph.nodes.filter(node => {
      const matchKind = this.kindFilter === 'ALL' || node.kind.toUpperCase() === this.kindFilter.toUpperCase();
      const matchQuery = !this.searchQuery || node.name.toLowerCase().includes(this.searchQuery.toLowerCase());
      return matchKind && matchQuery;
    });
  }

  get filteredEdges(): TopologyEdge[] {
    if (!this.graph?.edges) return [];
    const visibleNodeIds = new Set(this.filteredNodes.map(n => n.id));
    return this.graph.edges.filter(e => visibleNodeIds.has(e.source) && visibleNodeIds.has(e.target));
  }

  get ingressCount(): number {
    return this.graph?.nodes?.filter(n => n.kind.toLowerCase() === 'ingress').length || 0;
  }

  get serviceCount(): number {
    return this.graph?.nodes?.filter(n => n.kind.toLowerCase() === 'service').length || 0;
  }

  get podCount(): number {
    return this.graph?.nodes?.filter(n => n.kind.toLowerCase() === 'pod').length || 0;
  }

  selectNode(node: TopologyNode): void {
    this.selectedNode = node;
  }

  closeDrawer(): void {
    this.selectedNode = null;
  }

  getInboundEdges(nodeId: string): TopologyEdge[] {
    return this.graph?.edges?.filter(e => e.target === nodeId) || [];
  }

  getOutboundEdges(nodeId: string): TopologyEdge[] {
    return this.graph?.edges?.filter(e => e.source === nodeId) || [];
  }

  getNodeById(id: string): TopologyNode | undefined {
    return this.graph?.nodes?.find(n => n.id === id);
  }
}
