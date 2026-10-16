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
import {MatSnackBar} from '@angular/material/snack-bar';
import {Subscription} from 'rxjs';
import {AIService, SecurityFinding, SecurityResponse} from '@common/services/global/ai.service';
import {NamespaceService} from '@common/services/global/namespace';

@Component({
  selector: 'kd-ai-security',
  templateUrl: './template.html',
  styleUrls: ['./style.scss'],
})
export class AISecurityComponent implements OnInit, OnDestroy {
  isScanning = false;
  response: SecurityResponse | null = null;
  error: string | null = null;
  selectedNamespace = '';
  severityFilter = 'ALL';
  private nsSub_?: Subscription;

  constructor(
    private readonly aiService: AIService,
    private readonly namespaceService_: NamespaceService,
    private readonly snackBar: MatSnackBar,
  ) {}

  ngOnInit(): void {
    this.selectedNamespace = this.getActiveNamespace();
    this.nsSub_ = this.namespaceService_.onNamespaceChangeEvent.subscribe(() => {
      this.selectedNamespace = this.getActiveNamespace();
      if (this.response) {
        this.runScan();
      }
    });
  }

  ngOnDestroy(): void {
    this.nsSub_?.unsubscribe();
  }

  getActiveNamespace(): string {
    const ns = this.namespaceService_.current();
    if (!ns || ns === '_all' || this.namespaceService_.areMultipleNamespacesSelected()) {
      return '';
    }
    return ns;
  }

  runScan(): void {
    this.isScanning = true;
    this.error = null;

    this.aiService.runSecurity(this.selectedNamespace || undefined).subscribe({
      next: res => {
        this.response = res;
        this.isScanning = false;
      },
      error: err => {
        this.error = err.error?.message || 'Security scan failed. Please verify AI Settings and cluster permissions.';
        this.isScanning = false;
      },
    });
  }

  get filteredFindings(): SecurityFinding[] {
    if (!this.response?.findings) return [];
    if (this.severityFilter === 'ALL') return this.response.findings;
    return this.response.findings.filter(f => f.severity.toUpperCase() === this.severityFilter);
  }

  get criticalCount(): number {
    return this.response?.findings?.filter(f => f.severity.toUpperCase() === 'CRITICAL').length || 0;
  }

  get highCount(): number {
    return this.response?.findings?.filter(f => f.severity.toUpperCase() === 'HIGH').length || 0;
  }

  get mediumCount(): number {
    return this.response?.findings?.filter(f => f.severity.toUpperCase() === 'MEDIUM').length || 0;
  }

  get lowCount(): number {
    return this.response?.findings?.filter(f => f.severity.toUpperCase() === 'LOW').length || 0;
  }

  getScoreColor(score: number): string {
    if (score >= 80) return '#4caf50';
    if (score >= 60) return '#ff9800';
    return '#f44336';
  }

  copyYaml(patch: string): void {
    navigator.clipboard.writeText(patch).then(() => {
      this.snackBar.open('Hardened YAML patch copied to clipboard', 'Dismiss', {duration: 2500});
    });
  }
}
