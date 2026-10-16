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
import {AIService, ImprovementFinding, ImprovementResponse} from '@common/services/global/ai.service';
import {NamespaceService} from '@common/services/global/namespace';

@Component({
  selector: 'kd-ai-improve',
  templateUrl: './template.html',
  styleUrls: ['./style.scss'],
})
export class AIImproveComponent implements OnInit, OnDestroy {
  isAuditing = false;
  response: ImprovementResponse | null = null;
  error: string | null = null;
  selectedNamespace = '';
  categoryFilter = 'ALL';
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

  runAudit(): void {
    this.isAuditing = true;
    this.error = null;

    // Cluster Reliability Audit is always cluster-wide across all workloads
    this.aiService.runImprovement().subscribe({
      next: res => {
        this.response = res;
        this.isAuditing = false;
      },
      error: err => {
        this.error = err.error?.message || 'Improvement audit failed. Check AI Settings and cluster access.';
        this.isAuditing = false;
      },
    });
  }

  get filteredFindings(): ImprovementFinding[] {
    if (!this.response?.findings) return [];
    if (this.categoryFilter === 'ALL') return this.response.findings;
    return this.response.findings.filter(f => f.category.toLowerCase() === this.categoryFilter.toLowerCase());
  }

  copyPatch(patch: string): void {
    navigator.clipboard.writeText(patch).then(() => {
      this.snackBar.open('Optimization YAML copied to clipboard', 'Dismiss', {duration: 2500});
    });
  }
}
