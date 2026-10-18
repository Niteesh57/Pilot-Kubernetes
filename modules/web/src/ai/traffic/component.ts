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
  CatalogQuery,
  Dashboard,
  GraphQueryResult,
  ServiceDiagnosis,
  TopologyEdge,
  TopologyGraph,
  TopologyNode,
  extractStreamingSummary,
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

export interface GraphBarItem {
  label: string;
  sublabel: string;
  value: number;
  displayValue: string;
  percent: number;
  colorClass: string;
}

export interface MetricSummary {
  hasNumeric: boolean;
  metricColumn: string;
  max: number;
  min: number;
  avg: number;
  bars: GraphBarItem[];
}

export interface NodeInvestigationState {
  isInvestigating: boolean;
  investigationText: string;
  rawTokens?: string;
  investigationResult: AgentResponse | null;
  investigationError: string | null;
  cancelFn?: () => void;
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

  // Inline AI Investigation state (inside drawer) & persistent cache per node across navigation
  isInvestigating = false;
  investigationText = '';
  investigationResult: AgentResponse | null = null;
  investigationError: string | null = null;
  private cancelInvestigation_?: () => void;
  private static readonly globalInvestigationStates = new Map<string, NodeInvestigationState>();
  private static activeInstance: TrafficTopologyComponent | null = null;
  private static lastSelectedNodeId: string | null = null;

  private get investigationStates(): Map<string, NodeInvestigationState> {
    return TrafficTopologyComponent.globalInvestigationStates;
  }

  // Tab navigation: 0 = Topology, 1 = AI Query, 2 = Dashboards
  selectedTabIndex = 0;

  // AI Query state
  nlQuestion = '';
  nlCypher = '';
  queryColumns: string[] = [];
  queryRows: string[][] = [];
  queryError: string | null = null;
  isRunningNL = false;
  isCypherExpanded = true;
  copiedCypher = false;
  saveSuccessMessage: string | null = null;
  catalog: CatalogQuery[] = [];
  selectedCatalogName: string | null = null;
  saveTargetDashboard = 'NEW';
  widgetSaveTitle = '';
  isSavingWidget = false;
  queryViewMode: 'graph' | 'table' | 'both' = 'both';

  // Custom Dashboards state
  dashboards: Dashboard[] = [];
  isLoadingDashboards = false;
  isAddingDashboard = false;
  addingQueryDashboardName: string | null = null;
  newDashboardName = '';
  newDashboardDescription = '';
  newWidgetTitle = '';
  newWidgetCypher = '';
  widgetViewModes: {[key: string]: 'graph' | 'table' | 'both'} = {};
  dashboardResults: {
    [key: string]: {
      columns: string[];
      rows: string[][];
      isLoading: boolean;
      error?: string;
    };
  } = {};

  // Node Inspector Telemetry & Diagnosis state
  serviceDiagnosis: ServiceDiagnosis | null = null;
  isLoadingDiagnosis = false;

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
    TrafficTopologyComponent.activeInstance = this;
    this.refresh();
    this.loadCatalog();
    this.loadDashboards();
  }

  ngOnDestroy(): void {
    if (TrafficTopologyComponent.activeInstance === this) {
      TrafficTopologyComponent.activeInstance = null;
    }
    // Do NOT stop running investigations - background streaming continues into global map
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

    // Restore previously selected node if returning to this view
    if (TrafficTopologyComponent.lastSelectedNodeId && !this.selectedNode && this.graph?.nodes) {
      const prev = this.graph.nodes.find(n => n.id === TrafficTopologyComponent.lastSelectedNodeId);
      if (prev) {
        this.selectNode(prev);
      }
    }
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
      this.selectedNode = node;
      TrafficTopologyComponent.lastSelectedNodeId = node.id;

      // Restore cached investigation state if exists
      const cached = this.investigationStates.get(node.id);
      if (cached) {
        this.isInvestigating = cached.isInvestigating;
        this.investigationText = cached.investigationText;
        this.investigationResult = cached.investigationResult;
        this.investigationError = cached.investigationError;
        this.cancelInvestigation_ = cached.cancelFn;
      } else {
        this.isInvestigating = false;
        this.investigationText = '';
        this.investigationResult = null;
        this.investigationError = null;
        this.cancelInvestigation_ = undefined;
      }

      if (node.kind?.toLowerCase() === 'service' && node.namespace) {
        this.isLoadingDiagnosis = true;
        this.serviceDiagnosis = null;
        this.aiService.diagnoseService(node.namespace, node.name).subscribe({
          next: diag => {
            this.serviceDiagnosis = diag;
            this.isLoadingDiagnosis = false;
          },
          error: () => {
            this.isLoadingDiagnosis = false;
          },
        });
      } else {
        this.serviceDiagnosis = null;
        this.isLoadingDiagnosis = false;
      }
    }
  }

  closeDrawer(): void {
    // Keep investigation running in background, deselect node
    this.selectedNode = null;
    TrafficTopologyComponent.lastSelectedNodeId = null;
    this.serviceDiagnosis = null;
    this.isLoadingDiagnosis = false;
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
    const existing = this.investigationStates.get(node.id);
    if (existing?.isInvestigating) {
      if (existing.cancelFn) existing.cancelFn();
      existing.isInvestigating = false;
      if (this.selectedNode?.id === node.id) {
        this.isInvestigating = false;
      }
      return;
    }

    const state: NodeInvestigationState = {
      isInvestigating: true,
      investigationText: '',
      investigationResult: null,
      investigationError: null,
    };
    this.investigationStates.set(node.id, state);

    if (this.selectedNode?.id === node.id) {
      this.isInvestigating = true;
      this.investigationText = '';
      this.investigationResult = null;
      this.investigationError = null;
    }

    const query = `Investigate health, container logs, error events, and latency for @${node.kind.toLowerCase()}/${node.name}${node.namespace ? ' in namespace ' + node.namespace : ''}`;

    const cancelFn = this.aiService.runAgentStream(
      {
        namespace: node.namespace || undefined,
        query,
      },
      // onStep
      () => {},
      // onToken
      (token: string) => {
        state.rawTokens = (state.rawTokens || '') + token;
        state.investigationText = extractStreamingSummary(state.rawTokens);
        const active = TrafficTopologyComponent.activeInstance;
        if (active && active.selectedNode?.id === node.id) {
          active.investigationText = state.investigationText;
        }
      },
      // onComplete
      (resp: AgentResponse) => {
        state.isInvestigating = false;
        state.investigationResult = resp;
        if (resp.summary) {
          state.investigationText = resp.summary;
        } else if (state.investigationText) {
          state.investigationText = extractStreamingSummary(state.investigationText);
        }
        const active = TrafficTopologyComponent.activeInstance;
        if (active && active.selectedNode?.id === node.id) {
          active.isInvestigating = false;
          active.investigationResult = resp;
          active.investigationText = state.investigationText;
        }
      },
      // onError
      (err: any) => {
        state.isInvestigating = false;
        state.investigationError = err?.message || String(err) || 'Investigation failed';
        const active = TrafficTopologyComponent.activeInstance;
        if (active && active.selectedNode?.id === node.id) {
          active.isInvestigating = false;
          active.investigationError = state.investigationError;
        }
      },
    );
    state.cancelFn = cancelFn;
    this.cancelInvestigation_ = cancelFn;
  }

  stopInvestigation(): void {
    if (this.cancelInvestigation_) {
      this.cancelInvestigation_();
      this.cancelInvestigation_ = undefined;
    }
    this.isInvestigating = false;
  }

  loadCatalog(): void {
    this.aiService.getQueryCatalog().subscribe({
      next: catalog => {
        this.catalog = catalog;
      },
      error: () => {},
    });
  }

  loadDashboards(): void {
    this.isLoadingDashboards = true;
    this.aiService.listDashboards().subscribe({
      next: dashboards => {
        this.dashboards = dashboards || [];
        this.isLoadingDashboards = false;
        // Automatically default to first existing dashboard if none selected or if default 'NEW'
        if (this.dashboards.length > 0 && (this.saveTargetDashboard === 'NEW' || !this.saveTargetDashboard)) {
          this.saveTargetDashboard = this.dashboards[0].name;
        }
      },
      error: () => {
        this.isLoadingDashboards = false;
      },
    });
  }

  onTabChange(index: number): void {
    this.selectedTabIndex = index;
  }

  runNLQuery(): void {
    const q = this.nlQuestion.trim();
    if (!q) return;
    this.selectedCatalogName = null;
    const upper = q.toUpperCase();
    if (upper.startsWith('MATCH') || upper.startsWith('RETURN') || upper.startsWith('CALL')) {
      this.widgetSaveTitle = 'Custom Cypher Query';
      this.runCypherText(q, 'Custom Cypher Query');
      return;
    }
    this.widgetSaveTitle = q;
    this.isRunningNL = true;
    this.queryError = null;
    this.queryColumns = [];
    this.queryRows = [];
    this.saveSuccessMessage = null;
    this.aiService.runNLQuery(q).subscribe({
      next: res => {
        this.nlCypher = res.cypher;
        this.queryColumns = res.columns || [];
        this.queryRows = res.rows || [];
        this.isRunningNL = false;
        this.isCypherExpanded = true;
        this.widgetSaveTitle = q;
      },
      error: err => {
        this.queryError = err.error?.message || err.message || 'Query execution failed.';
        this.isRunningNL = false;
      },
    });
  }

  runCatalogQuery(item: CatalogQuery): void {
    this.selectedCatalogName = item.name;
    this.nlQuestion = item.description || item.name;
    this.nlCypher = item.cypher;
    this.widgetSaveTitle = item.name;
    this.runCypherText(item.cypher, item.name);
  }

  runCypherText(cypher: string, title?: string): void {
    if (!cypher) return;
    this.nlCypher = cypher;
    if (title) {
      this.widgetSaveTitle = title;
    }
    this.isRunningNL = true;
    this.queryError = null;
    this.queryColumns = [];
    this.queryRows = [];
    this.saveSuccessMessage = null;
    this.aiService.executeGraphQuery(cypher).subscribe({
      next: result => {
        this.nlCypher = cypher;
        this.queryColumns = result.columns || [];
        this.queryRows = result.rows || [];
        this.isRunningNL = false;
        this.isCypherExpanded = true;
        if (title) {
          this.widgetSaveTitle = title;
        } else if (!this.widgetSaveTitle) {
          this.widgetSaveTitle = this.nlQuestion.trim() || 'Custom Metric Query';
        }
      },
      error: err => {
        this.queryError = err.error?.message || err.message || 'Cypher query execution failed.';
        this.isRunningNL = false;
      },
    });
  }

  saveCurrentAsDashboard(): void {
    if (this.isSavingWidget) return;

    // Resolve Cypher: use directly from question if it is raw Cypher, otherwise nlCypher
    let cypherToSave = this.nlCypher;
    const qTrim = this.nlQuestion.trim();
    const upper = qTrim.toUpperCase();
    if (upper.startsWith('MATCH') || upper.startsWith('RETURN') || upper.startsWith('CALL')) {
      cypherToSave = qTrim;
    }
    if (!cypherToSave) return;

    const title = this.widgetSaveTitle.trim() || this.nlQuestion.trim() || 'Custom Metric Query';

    this.isSavingWidget = true;
    if (this.saveTargetDashboard === 'NEW') {
      if (!this.newDashboardName.trim()) {
        this.isSavingWidget = false;
        return;
      }
      const dName = this.newDashboardName.trim();
      const newDash: Dashboard = {
        name: dName,
        description: this.newDashboardDescription.trim(),
        queries: [{title, cypher: cypherToSave}],
      };
      // Immediately reflect in local state
      this.dashboards.push(newDash);
      this.saveTargetDashboard = dName;
      this.newDashboardName = '';
      this.newDashboardDescription = '';

      this.aiService.saveDashboard(newDash).subscribe({
        next: () => {
          this.isSavingWidget = false;
          this.saveSuccessMessage = `Dashboard '${dName}' created successfully!`;
          setTimeout(() => (this.saveSuccessMessage = null), 6000);
          this.loadDashboards();
        },
        error: err => {
          this.isSavingWidget = false;
          this.queryError = err.error?.message || 'Failed to save dashboard.';
        },
      });
    } else {
      const existing = this.dashboards.find(d => d.name === this.saveTargetDashboard);
      if (!existing) {
        this.isSavingWidget = false;
        return;
      }
      const newQueries = [...(existing.queries || []), {title, cypher: cypherToSave}];
      const updated: Dashboard = {
        name: existing.name,
        description: existing.description,
        queries: newQueries,
      };
      // Immediately reflect in local state so subsequent saves don't lose queries
      existing.queries = newQueries;

      this.aiService.saveDashboard(updated).subscribe({
        next: () => {
          this.isSavingWidget = false;
          this.saveSuccessMessage = `Query widget added to '${existing.name}'!`;
          setTimeout(() => (this.saveSuccessMessage = null), 6000);
          this.loadDashboards();
        },
        error: err => {
          this.isSavingWidget = false;
          this.queryError = err.error?.message || 'Failed to update dashboard.';
        },
      });
    }
  }

  // Custom Dashboards Tab actions
  openAddDashboard(): void {
    this.isAddingDashboard = true;
    this.newDashboardName = '';
    this.newDashboardDescription = '';
    this.newWidgetTitle = '';
    this.newWidgetCypher = '';
  }

  cancelAddDashboard(): void {
    this.isAddingDashboard = false;
  }

  saveNewDashboard(): void {
    const name = this.newDashboardName.trim();
    if (!name) return;
    const queries = [];
    if (this.newWidgetCypher.trim()) {
      queries.push({
        title: this.newWidgetTitle.trim() || 'Custom Query',
        cypher: this.newWidgetCypher.trim(),
      });
    }
    this.aiService
      .saveDashboard({
        name,
        description: this.newDashboardDescription.trim(),
        queries,
      })
      .subscribe({
        next: () => {
          this.isAddingDashboard = false;
          this.newDashboardName = '';
          this.newDashboardDescription = '';
          this.newWidgetTitle = '';
          this.newWidgetCypher = '';
          this.loadDashboards();
        },
        error: err => {
          this.error = err.error?.message || 'Failed to create dashboard.';
        },
      });
  }

  openAddQuery(dashboardName: string): void {
    this.addingQueryDashboardName = dashboardName;
    this.newWidgetTitle = '';
    this.newWidgetCypher = '';
  }

  cancelAddQuery(): void {
    this.addingQueryDashboardName = null;
    this.newWidgetTitle = '';
    this.newWidgetCypher = '';
  }

  saveQueryToExistingDashboard(d: Dashboard): void {
    const cypher = this.newWidgetCypher.trim();
    if (!cypher) return;
    const title = this.newWidgetTitle.trim() || 'Custom Metric Query';
    const updated: Dashboard = {
      name: d.name,
      description: d.description,
      queries: [...(d.queries || []), {title, cypher}],
    };
    this.aiService.saveDashboard(updated).subscribe({
      next: () => {
        this.addingQueryDashboardName = null;
        this.newWidgetTitle = '';
        this.newWidgetCypher = '';
        this.loadDashboards();
      },
      error: err => {
        this.error = err.error?.message || 'Failed to add query widget.';
      },
    });
  }

  deleteQueryFromDashboard(d: Dashboard, queryIndex: number): void {
    const updatedQueries = (d.queries || []).filter((_, i) => i !== queryIndex);
    const updated: Dashboard = {
      name: d.name,
      description: d.description,
      queries: updatedQueries,
    };
    this.aiService.saveDashboard(updated).subscribe({
      next: () => {
        delete this.dashboardResults[`${d.name}_${queryIndex}`];
        this.loadDashboards();
      },
      error: err => {
        this.error = err.error?.message || 'Failed to remove query.';
      },
    });
  }

  goToTopology(): void {
    this.selectedTabIndex = 0;
  }

  goToAIQuery(): void {
    this.selectedTabIndex = 1;
  }

  goToDashboards(): void {
    this.selectedTabIndex = 2;
  }

  runDashboardWidget(dashboardName: string, queryIndex: number, cypher: string): void {
    const key = `${dashboardName}_${queryIndex}`;
    this.dashboardResults[key] = {columns: [], rows: [], isLoading: true};
    this.aiService.executeGraphQuery(cypher).subscribe({
      next: result => {
        this.dashboardResults[key] = {
          columns: result.columns || [],
          rows: result.rows || [],
          isLoading: false,
        };
      },
      error: err => {
        this.dashboardResults[key] = {
          columns: [],
          rows: [],
          isLoading: false,
          error: err.error?.message || err.message || 'Widget query failed.',
        };
      },
    });
  }

  runAllQueriesInDashboard(d: Dashboard): void {
    if (!d.queries) return;
    d.queries.forEach((q, idx) => {
      this.runDashboardWidget(d.name, idx, q.cypher);
    });
  }

  getMetricSummary(columns: string[], rows: string[][]): MetricSummary {
    if (!columns || columns.length === 0 || !rows || rows.length === 0) {
      return {hasNumeric: false, metricColumn: '', max: 0, min: 0, avg: 0, bars: []};
    }

    // Identify name column (label)
    let nameIdx = columns.findIndex(c => {
      const lower = c.toLowerCase();
      return lower.includes('name') && !lower.includes('nodename');
    });
    if (nameIdx === -1) nameIdx = 0;

    // Identify namespace column
    const nsIdx = columns.findIndex(c => c.toLowerCase().includes('namespace'));

    // Identify numeric column to visualize
    let metricIdx = -1;
    const priorityNames = ['restart', 'cpu', 'mem', 'replica', 'desired', 'ready', 'count'];
    for (const p of priorityNames) {
      const idx = columns.findIndex(c => c.toLowerCase().includes(p));
      if (idx !== -1) {
        metricIdx = idx;
        break;
      }
    }

    if (metricIdx === -1) {
      for (let c = 0; c < columns.length; c++) {
        if (c === nameIdx || c === nsIdx) continue;
        let numCount = 0;
        for (const row of rows) {
          const v = parseFloat(row[c]);
          if (!isNaN(v)) numCount++;
        }
        if (numCount > rows.length * 0.5) {
          metricIdx = c;
          break;
        }
      }
    }

    if (metricIdx === -1) {
      return {hasNumeric: false, metricColumn: '', max: 0, min: 0, avg: 0, bars: []};
    }

    const metricName = columns[metricIdx];
    let max = 0;
    let min = Infinity;
    let sum = 0;
    let validCount = 0;

    const parsedValues: number[] = [];
    for (const row of rows) {
      const raw = row[metricIdx];
      let val = 0;
      if (raw !== undefined && raw !== null && raw !== '') {
        const parsed = parseFloat(String(raw).replace(/[^0-9.-]/g, ''));
        if (!isNaN(parsed)) {
          val = parsed;
        }
      }
      parsedValues.push(val);
      if (val > max) max = val;
      if (val < min) min = val;
      sum += val;
      validCount++;
    }

    if (max === 0) max = 1;
    if (min === Infinity) min = 0;
    const avg = validCount > 0 ? Math.round((sum / validCount) * 10) / 10 : 0;

    const lowerMetric = metricName.toLowerCase();
    const bars: GraphBarItem[] = rows.map((row, rIdx) => {
      const val = parsedValues[rIdx];
      const label = row[nameIdx] || `Item ${rIdx + 1}`;
      const sublabel = nsIdx !== -1 ? row[nsIdx] || '' : '';
      const percent = Math.min(100, Math.max(8, Math.round((val / max) * 100)));

      let displayValue = `${val}`;
      let colorClass = 'bar-blue';

      if (lowerMetric.includes('cpu')) {
        displayValue = `${val}m`;
        colorClass = 'bar-cyan';
      } else if (lowerMetric.includes('mem')) {
        displayValue = `${val}Mi`;
        colorClass = 'bar-purple';
      } else if (lowerMetric.includes('restart')) {
        displayValue = `${val} ${val === 1 ? 'restart' : 'restarts'}`;
        colorClass = val > 5 ? 'bar-red' : (val > 0 ? 'bar-amber' : 'bar-green');
      } else if (lowerMetric.includes('replica') || lowerMetric.includes('ready') || lowerMetric.includes('desired')) {
        displayValue = `${val}`;
        colorClass = 'bar-green';
      }

      return {
        label,
        sublabel,
        value: val,
        displayValue,
        percent,
        colorClass,
      };
    });

    return {
      hasNumeric: true,
      metricColumn: metricName,
      max,
      min,
      avg,
      bars,
    };
  }

  getWidgetViewMode(dashboardName: string, qIdx: number): 'graph' | 'table' | 'both' {
    const key = `${dashboardName}_${qIdx}`;
    return this.widgetViewModes[key] || 'both';
  }

  setWidgetViewMode(dashboardName: string, qIdx: number, mode: 'graph' | 'table' | 'both'): void {
    const key = `${dashboardName}_${qIdx}`;
    this.widgetViewModes[key] = mode;
  }

  copyToClipboard(text: string): void {
    if (!navigator.clipboard) return;
    navigator.clipboard.writeText(text).then(() => {
      this.copiedCypher = true;
      setTimeout(() => (this.copiedCypher = false), 2500);
    });
  }

  hasTelemetry(node: TopologyNode | null): boolean {
    if (!node || !node.properties) return false;
    return !!(node.properties['cpuMilli'] || node.properties['memoryMi']);
  }

  getPodMetricDisplay(node: TopologyNode | null): {cpu?: string; memory?: string} {
    if (!node?.properties) return {};
    const res: {cpu?: string; memory?: string} = {};
    if (node.properties['cpuMilli']) {
      res.cpu = `${node.properties['cpuMilli']}m`;
    }
    if (node.properties['memoryMi']) {
      res.memory = `${node.properties['memoryMi']}Mi`;
    }
    return res;
  }

  deleteDashboard(name: string): void {
    this.aiService.deleteDashboard(name).subscribe({
      next: () => this.loadDashboards(),
      error: () => {},
    });
  }

  stringify(value: unknown): string {
    return JSON.stringify(value, null, 2);
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
