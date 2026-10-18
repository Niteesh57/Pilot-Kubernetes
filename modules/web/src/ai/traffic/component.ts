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

import {Component, OnDestroy, OnInit} from '@angular/core';
import {Router} from '@angular/router';
import {
  AIService,
  AgentResponse,
  TopologyEdge,
  TopologyGraph,
  TopologyNode,
} from '@common/services/global/ai.service';

export interface LayoutNode extends TopologyNode {
  x: number;
  y: number;
  width: number;
  height: number;
  shortName: string;
}

export interface LayoutEdge extends TopologyEdge {
  path: string;
  midX: number;
  midY: number;
  label: string;
}

@Component({
  selector: 'kd-traffic-topology',
  templateUrl: './template.html',
  styleUrls: ['./style.scss'],
})
export class TrafficTopologyComponent implements OnInit, OnDestroy {
  isLoading = false;
  graph: TopologyGraph | null = null;
  error: string | null = null;
  selectedNamespace = '';
  searchQuery = '';
  selectedGroup = 'ALL';
  selectedNode: TopologyNode | null = null;

  // Inline AI Investigation state (inside drawer)
  isInvestigating = false;
  investigationText = '';
  investigationResult: AgentResponse | null = null;
  investigationError: string | null = null;
  private cancelInvestigation_?: () => void;

  // Zoom and Pan
  zoomScale = 1.0;
  panX = 40;
  panY = 40;
  isPanning = false;
  private startMouseX = 0;
  private startMouseY = 0;

  // Computed SVG layout
  layoutNodes: LayoutNode[] = [];
  layoutEdges: LayoutEdge[] = [];
  svgWidth = 1400;
  svgHeight = 800;

  constructor(
    private readonly aiService: AIService,
    private readonly router: Router,
  ) {}

  ngOnInit(): void {
    this.refresh();
  }

  ngOnDestroy(): void {
    this.stopInvestigation();
  }

  refresh(): void {
    this.isLoading = true;
    this.error = null;
    this.selectedNode = null;

    // Discover universal cluster-wide topology independent of active namespace selection
    this.aiService.getTopology().subscribe({
      next: graph => {
        this.graph = graph;
        this.isLoading = false;
        this.calculateLayout();
      },
      error: err => {
        this.error = err.error?.message || 'Failed to discover cluster topology.';
        this.isLoading = false;
      },
    });
  }

  get availableGroups(): string[] {
    if (!this.graph?.nodes) return [];
    const set = new Set<string>();
    for (const n of this.graph.nodes) {
      if (n.group) set.add(n.group);
    }
    return Array.from(set).sort();
  }

  onGroupChange(): void {
    this.calculateLayout();
  }

  calculateLayout(): void {
    if (!this.graph?.nodes || this.graph.nodes.length === 0) {
      this.layoutNodes = [];
      this.layoutEdges = [];
      return;
    }

    // Filter nodes by group and search query
    let filtered = this.graph.nodes;
    if (this.selectedGroup !== 'ALL') {
      filtered = filtered.filter(n => n.group === this.selectedGroup);
    }
    if (this.searchQuery) {
      const q = this.searchQuery.toLowerCase();
      filtered = filtered.filter(
        n =>
          n.name.toLowerCase().includes(q) ||
          n.kind.toLowerCase().includes(q) ||
          n.namespace.toLowerCase().includes(q),
      );
    }

    const filteredIds = new Set(filtered.map(n => n.id));

    // Categorize into vertical hierarchy layers:
    // Layer 0: Ingresses
    // Layer 1: Services
    // Layer 2: Workloads (Deployments, StatefulSets, DaemonSets, Jobs, CronJobs)
    // Layer 3: Pods
    // Layer 4: Configuration & Identity (ConfigMaps, Secrets, ServiceAccounts, PVCs)
    const layers: LayoutNode[][] = [[], [], [], [], []];
    const nodeWidth = 230;
    const nodeHeight = 68;
    const xSpacing = 40;
    const ySpacing = 95;

    for (const raw of filtered) {
      const kind = raw.kind.toLowerCase();
      let layerIndex = 2; // default workload

      if (kind === 'ingress') {
        layerIndex = 0;
      } else if (kind === 'service') {
        layerIndex = 1;
      } else if (
        kind === 'deployment' ||
        kind === 'statefulset' ||
        kind === 'daemonset' ||
        kind === 'job' ||
        kind === 'cronjob'
      ) {
        layerIndex = 2;
      } else if (kind === 'pod') {
        layerIndex = 3;
      } else if (
        kind === 'configmap' ||
        kind === 'secret' ||
        kind === 'serviceaccount' ||
        kind === 'pvc' ||
        kind === 'persistentvolumeclaim'
      ) {
        layerIndex = 4;
      }

      const shortName = raw.name.length > 26 ? raw.name.substring(0, 24) + '…' : raw.name;

      layers[layerIndex].push({
        ...raw,
        x: 0,
        y: 0,
        width: nodeWidth,
        height: nodeHeight,
        shortName,
      });
    }

    // Determine max layer count for width calculation
    let maxNodesInLayer = 1;
    for (const l of layers) {
      if (l.length > maxNodesInLayer) maxNodesInLayer = l.length;
    }

    const contentWidth = Math.max(1200, maxNodesInLayer * (nodeWidth + xSpacing) + 120);
    const startY = 40;

    const positionedNodes: LayoutNode[] = [];
    const posMap = new Map<string, LayoutNode>();

    for (let li = 0; li < layers.length; li++) {
      const layer = layers[li];
      if (layer.length === 0) continue;

      const layerTotalWidth = layer.length * nodeWidth + (layer.length - 1) * xSpacing;
      const startX = Math.max(60, (contentWidth - layerTotalWidth) / 2);

      for (let ni = 0; ni < layer.length; ni++) {
        const node = layer[ni];
        node.x = startX + ni * (nodeWidth + xSpacing);
        node.y = startY + li * (nodeHeight + ySpacing);
        positionedNodes.push(node);
        posMap.set(node.id, node);
      }
    }

    // Calculate Edges
    const layoutEdges: LayoutEdge[] = [];
    if (this.graph.edges) {
      for (const edge of this.graph.edges) {
        if (!filteredIds.has(edge.source) || !filteredIds.has(edge.target)) {
          continue;
        }
        const src = posMap.get(edge.source);
        const tgt = posMap.get(edge.target);
        if (!src || !tgt) continue;

        // Connect from bottom-center of source to top-center of target
        const sx = src.x + src.width / 2;
        const sy = src.y + src.height;
        const tx = tgt.x + tgt.width / 2;
        const ty = tgt.y;

        const deltaY = Math.max(30, Math.abs(ty - sy) / 2);
        const path = `M ${sx} ${sy} C ${sx} ${sy + deltaY}, ${tx} ${ty - deltaY}, ${tx} ${ty}`;
        const midX = (sx + tx) / 2;
        const midY = (sy + ty) / 2;

        layoutEdges.push({
          ...edge,
          path,
          midX,
          midY,
          label: edge.port ? `:${edge.port}` : edge.type || '',
        });
      }
    }

    this.layoutNodes = positionedNodes;
    this.layoutEdges = layoutEdges;
    this.svgWidth = contentWidth + 100;
    this.svgHeight = startY + layers.length * (nodeHeight + ySpacing) + 120;
  }

  // Pan and Zoom
  zoomIn(): void {
    this.zoomScale = Math.min(2.0, this.zoomScale + 0.15);
  }

  zoomOut(): void {
    this.zoomScale = Math.max(0.35, this.zoomScale - 0.15);
  }

  resetZoom(): void {
    this.zoomScale = 1.0;
    this.panX = 40;
    this.panY = 40;
  }

  startPan(e: MouseEvent): void {
    if (e.button !== 0) return;
    this.isPanning = true;
    this.startMouseX = e.clientX - this.panX;
    this.startMouseY = e.clientY - this.panY;
  }

  doPan(e: MouseEvent): void {
    if (!this.isPanning) return;
    this.panX = e.clientX - this.startMouseX;
    this.panY = e.clientY - this.startMouseY;
  }

  endPan(): void {
    this.isPanning = false;
  }

  selectNode(node: TopologyNode): void {
    if (this.selectedNode?.id !== node.id) {
      this.stopInvestigation();
      this.selectedNode = node;
      this.investigationText = '';
      this.investigationResult = null;
      this.investigationError = null;
    }
  }

  closeDrawer(): void {
    this.stopInvestigation();
    this.selectedNode = null;
    this.investigationText = '';
    this.investigationResult = null;
    this.investigationError = null;
  }

  getResourceHref(node: TopologyNode | null): string[] {
    if (!node || !node.name) return [];
    let kind = (node.kind || '').toLowerCase();
    if (kind === 'pvc') kind = 'persistentvolumeclaim';
    if (kind === 'sa') kind = 'serviceaccount';

    if (kind === 'node' || kind === 'persistentvolume' || kind === 'namespace') {
      return ['/', kind, node.name];
    }
    if (node.namespace) {
      return ['/', kind, node.namespace, node.name];
    }
    return ['/', kind, node.name];
  }

  runRCA(node: TopologyNode): void {
    if (this.isInvestigating) {
      this.stopInvestigation();
      return;
    }

    this.isInvestigating = true;
    this.investigationText = '';
    this.investigationResult = null;
    this.investigationError = null;

    const query = `Investigate health, container logs, error events, and latency for @${node.kind.toLowerCase()}/${node.name}${node.namespace ? ' in namespace ' + node.namespace : ''}`;

    this.cancelInvestigation_ = this.aiService.runAgentStream(
      {
        namespace: node.namespace || undefined,
        query,
      },
      // onStep
      () => {},
      // onToken
      (token: string) => {
        this.investigationText += token;
      },
      // onComplete
      (resp: AgentResponse) => {
        this.isInvestigating = false;
        this.investigationResult = resp;
        if (!this.investigationText || this.investigationText.trim().startsWith('{')) {
          this.investigationText = resp.summary;
        }
      },
      // onError
      (err: any) => {
        this.isInvestigating = false;
        this.investigationError = err?.message || String(err) || 'Investigation failed';
      },
    );
  }

  stopInvestigation(): void {
    if (this.cancelInvestigation_) {
      this.cancelInvestigation_();
      this.cancelInvestigation_ = undefined;
    }
    this.isInvestigating = false;
  }

  getStatusClass(status: string): string {
    const s = (status || '').toLowerCase();
    if (
      s === 'running' ||
      s === 'active' ||
      s === 'completed' ||
      s === 'bound' ||
      s.includes('replicas') ||
      s === 'ready'
    ) {
      return 'status-healthy';
    }
    if (s === 'pending' || s === 'containercreating' || s === 'scaled down' || s === 'notready') {
      return 'status-warning';
    }
    return 'status-error';
  }

  getInboundEdges(nodeId: string): TopologyEdge[] {
    return this.graph?.edges?.filter(e => e.target === nodeId) || [];
  }

  getOutboundEdges(nodeId: string): TopologyEdge[] {
    return this.graph?.edges?.filter(e => e.source === nodeId) || [];
  }

  getPropertiesList(node: TopologyNode): Array<{key: string; value: string}> {
    if (!node.properties) return [];
    return Object.entries(node.properties).map(([k, v]) => ({key: k, value: v}));
  }
}
