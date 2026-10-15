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
import {Observable} from 'rxjs';

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
  aiSummary?: string;
}

export interface AgentRequest {
  namespace?: string;
  query: string;
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
  labels?: {[key: string]: string};
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
  timestamp: number;
}

@Injectable({providedIn: 'root'})
export class AIService {
  private readonly apiBase = 'api/v1';

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
    return this.http.post<SecurityResponse>(url, {});
  }

  runAgent(req: AgentRequest): Observable<AgentResponse> {
    return this.http.post<AgentResponse>(`${this.apiBase}/ai/agent`, req);
  }

  getGraphStatus(): Observable<GraphStatus> {
    return this.http.get<GraphStatus>(`${this.apiBase}/graph/status`);
  }

  executeGraphQuery(cypher: string): Observable<unknown> {
    return this.http.post<unknown>(`${this.apiBase}/graph/query`, {cypher});
  }

  getBlastRadius(kind: string, namespace: string, name: string): Observable<unknown> {
    return this.http.get<unknown>(`${this.apiBase}/graph/blastradius/${kind}/${namespace}/${name}`);
  }

  getTopology(namespace?: string): Observable<TopologyGraph> {
    const url = namespace
      ? `${this.apiBase}/traffic/${namespace}`
      : `${this.apiBase}/traffic`;
    return this.http.get<TopologyGraph>(url);
  }
}
