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

import {Injectable} from '@angular/core';
import {HttpClient} from '@angular/common/http';
import {Observable, tap} from 'rxjs';

export interface AISettings {
  provider: string;
  baseUrl: string;
  apiKey?: string;
  apiKeyMasked?: string;
  model: string;
  maxTokens: number;
  temperature: number;
  falkordbAddr: string;
  falkordbGraphName: string;
}

export interface VerifyRequest {
  provider: string;
  baseUrl: string;
  apiKey: string;
  model: string;
}

export interface VerifyResponse {
  connected: boolean;
  latencyMs: number;
  models: string[];
  errorMsg?: string;
}

export interface AnalystRequest {
  namespace: string;
  kind: string;
  name: string;
  containerName?: string;
  customQuery?: string;
}

export interface AnalystResponse {
  summary: string;
  rootCause: string;
  evidence: string;
  remediation: string[];
  rawAnalysis: string;
}

export interface ImprovementFinding {
  namespace: string;
  kind: string;
  name: string;
  category: string;
  severity: string;
  issue: string;
  yamlPatch?: string;
}

export interface ImprovementResponse {
  totalScanned: number;
  findings: ImprovementFinding[];
  aiSummary?: string;
}

export interface SecurityFinding {
  kind: string;
  name: string;
  namespace: string;
  category: string;
  severity: string;
  issue: string;
  details: string;
  yamlPatch?: string;
}

export interface SecurityResponse {
  score: number;
  findings: SecurityFinding[];
  attackPaths?: string[];
  aiSummary?: string;
}

export interface AgentMessage {
  role: 'user' | 'assistant';
  content: string;
}

export interface AgentRequest {
  namespace?: string;
  query: string;
  history?: AgentMessage[];
}

export interface ResourceItem {
  kind: string; // 'Deployment' | 'Pod' | 'Service'
  name: string;
  namespace: string;
  status?: string;
}

export interface ResourcesResponse {
  deployments: ResourceItem[];
  pods: ResourceItem[];
  services: ResourceItem[];
}

export interface AgentStep {
  step: string;
  description: string;
  output?: string;
}

export interface AgentResponse {
  query: string;
  steps: AgentStep[];
  summary: string;
  rootCause?: string;
  commands?: string[];
  yamlPatches?: string[];
  followUps?: string[];
  cypherQueries?: string[];
  graphContext?: string;
}

export interface GraphStatus {
  connected: boolean;
  graphName: string;
  nodeCount: number;
  edgeCount: number;
  error?: string;
}

export interface TopologyNode {
  id: string;
  name: string;
  namespace: string;
  kind: string;
  status: string;
  group?: string;
  labels?: {[key: string]: string};
  properties?: {[key: string]: string};
}

export interface TopologyEdge {
  id: string;
  source: string;
  target: string;
  type: string;
  port?: string;
  protocol?: string;
}

export interface TopologyGraph {
  nodes: TopologyNode[];
  edges: TopologyEdge[];
  groups?: string[];
  timestamp: number;
}

export interface GraphNode {
  id: string;
  name: string;
  namespace: string;
  kind: string;
  status?: string;
  properties?: {[key: string]: string};
}

export interface GraphEdge {
  id: string;
  source: string;
  target: string;
  type: string;
}

export interface GraphResult {
  nodes: GraphNode[];
  edges: GraphEdge[];
}

export interface BlastRadiusResult {
  target: GraphNode;
  dependents: GraphNode[];
  dependencies: GraphNode[];
}

export interface AttackPath {
  serviceAccount: string;
  namespace: string;
  binding: string;
  bindingKind: string;
  role: string;
  roleKind: string;
  severity: string;
}

@Injectable({providedIn: 'root'})
export class AIService {
  private readonly apiBase = 'api/v1';

  // Global Security Audit State (persisted across page navigation)
  isAnalyzingSecurity = false;
  securityResponse: SecurityResponse | null = null;
  securityError: string | null = null;
  securityTargetNamespace = '';

  constructor(private readonly http: HttpClient) {}

  getSettings(): Observable<AISettings> {
    return this.http.get<AISettings>(`${this.apiBase}/ai/settings`);
  }

  saveSettings(settings: AISettings): Observable<void> {
    return this.http.put<void>(`${this.apiBase}/ai/settings`, settings);
  }

  verifyConnection(req: VerifyRequest): Observable<VerifyResponse> {
    return this.http.post<VerifyResponse>(`${this.apiBase}/ai/verify`, req);
  }

  runAnalysis(req: AnalystRequest): Observable<AnalystResponse> {
    return this.http.post<AnalystResponse>(`${this.apiBase}/ai/analyst`, req);
  }

  runImprovement(namespace?: string): Observable<ImprovementResponse> {
    const url = namespace
      ? `${this.apiBase}/ai/improvement/${namespace}`
      : `${this.apiBase}/ai/improvement`;
    return this.http.post<ImprovementResponse>(url, {});
  }

  runSecurity(namespace?: string): Observable<SecurityResponse> {
    const url = namespace
      ? `${this.apiBase}/ai/security/${namespace}`
      : `${this.apiBase}/ai/security`;
    this.isAnalyzingSecurity = true;
    this.securityError = null;
    this.securityTargetNamespace = namespace || '';
    return this.http.post<SecurityResponse>(url, {}).pipe(
      tap({
        next: res => {
          this.securityResponse = res;
          this.isAnalyzingSecurity = false;
        },
        error: err => {
          this.securityError =
            err.error?.message || 'Security scan failed. Please verify AI Settings and cluster permissions.';
          this.isAnalyzingSecurity = false;
        },
      }),
    );
  }

  runAgent(req: AgentRequest): Observable<AgentResponse> {
    return this.http.post<AgentResponse>(`${this.apiBase}/ai/agent`, req);
  }

  runAgentStream(
    req: AgentRequest,
    onStep: (step: AgentStep) => void,
    onToken: (token: string) => void,
    onComplete: (res: AgentResponse) => void,
    onError: (err: any) => void,
  ): () => void {
    const controller = new AbortController();
    fetch(`${this.apiBase}/ai/agent/stream`, {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify(req),
      signal: controller.signal,
    })
      .then(async response => {
        if (!response.ok) {
          const errText = await response.text();
          throw new Error(errText || `HTTP ${response.status}`);
        }
        const reader = response.body?.getReader();
        if (!reader) throw new Error('ReadableStream not supported');
        const decoder = new TextDecoder();
        let buffer = '';

        while (true) {
          const {done, value} = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, {stream: true});
          const blocks = buffer.split('\n\n');
          buffer = blocks.pop() || '';

          for (const block of blocks) {
            const trimmed = block.trim();
            if (!trimmed) continue;
            let event = 'message';
            let data = '';
            for (const line of trimmed.split('\n')) {
              if (line.startsWith('event: ')) {
                event = line.substring(7).trim();
              } else if (line.startsWith('data: ')) {
                data = line.substring(6).trim();
              }
            }
            if (!data) continue;

            try {
              const parsed = JSON.parse(data);
              if (event === 'step') {
                onStep(parsed as AgentStep);
              } else if (event === 'token') {
                onToken(parsed as string);
              } else if (event === 'done') {
                onComplete(parsed as AgentResponse);
              } else if (event === 'error') {
                onError(new Error(typeof parsed === 'string' ? parsed : JSON.stringify(parsed)));
              }
            } catch {
              if (event === 'token') {
                onToken(data);
              }
            }
          }
        }
      })
      .catch(err => {
        if (err.name !== 'AbortError') {
          onError(err);
        }
      });

    return () => controller.abort();
  }

  getResources(namespace?: string): Observable<ResourcesResponse> {
    const url = namespace
      ? `${this.apiBase}/ai/resources/${namespace}`
      : `${this.apiBase}/ai/resources`;
    return this.http.get<ResourcesResponse>(url);
  }

  getGraphStatus(): Observable<GraphStatus> {
    return this.http.get<GraphStatus>(`${this.apiBase}/graph/status`);
  }

  executeGraphQuery(cypher: string): Observable<unknown> {
    return this.http.post<unknown>(`${this.apiBase}/graph/query`, {cypher});
  }

  getBlastRadius(kind: string, namespace: string, name: string): Observable<BlastRadiusResult> {
    return this.http.get<BlastRadiusResult>(`${this.apiBase}/graph/blastradius/${kind}/${namespace}/${name}`);
  }

  getTopology(namespace?: string): Observable<TopologyGraph> {
    const url = namespace
      ? `${this.apiBase}/traffic/${namespace}`
      : `${this.apiBase}/traffic`;
    return this.http.get<TopologyGraph>(url);
  }

  getGraphTopology(): Observable<GraphResult> {
    return this.http.get<GraphResult>(`${this.apiBase}/graph/topology`);
  }

  exploreGraph(kind: string, namespace: string, name: string): Observable<GraphResult> {
    return this.http.get<GraphResult>(`${this.apiBase}/graph/explore/${kind}/${namespace}/${name}`);
  }

  getAttackPaths(namespace?: string): Observable<AttackPath[]> {
    const url = namespace
      ? `${this.apiBase}/graph/attackpaths/${namespace}`
      : `${this.apiBase}/graph/attackpaths`;
    return this.http.get<AttackPath[]>(url);
  }

  syncGraph(): Observable<void> {
    return this.http.post<void>(`${this.apiBase}/graph/sync`, {});
  }
}
