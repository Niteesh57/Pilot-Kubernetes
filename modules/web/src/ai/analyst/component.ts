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

import {HttpClient} from '@angular/common/http';
import {Component, OnDestroy, OnInit} from '@angular/core';
import {FormBuilder, FormGroup, Validators} from '@angular/forms';
import {Subscription} from 'rxjs';
import {AIService, AnalystRequest, AnalystResponse} from '@common/services/global/ai.service';
import {NamespaceService} from '@common/services/global/namespace';

interface ResourceListResponse {
  items?: Array<{
    objectMeta?: {name: string};
    typeMeta?: {kind: string};
  }>;
}

@Component({
  selector: 'kd-ai-analyst',
  templateUrl: './template.html',
  styleUrls: ['./style.scss'],
})
export class AIAnalystComponent implements OnInit, OnDestroy {
  form: FormGroup;
  isAnalyzing = false;
  result: AnalystResponse | null = null;
  error: string | null = null;
  discoveredResources: string[] = [];
  isLoadingResources = false;

  readonly kinds = ['Pod', 'Deployment', 'StatefulSet', 'DaemonSet', 'ReplicaSet', 'Job', 'CronJob', 'Service', 'Node'];
  private nsSub_?: Subscription;

  constructor(
    private readonly fb: FormBuilder,
    private readonly aiService: AIService,
    private readonly namespaceService_: NamespaceService,
    private readonly http: HttpClient,
  ) {}

  ngOnInit(): void {
    const activeNs = this.getActiveNamespace() || 'default';
    this.form = this.fb.group({
      namespace: [activeNs, Validators.required],
      kind: ['Pod', Validators.required],
      name: ['', Validators.required],
      containerName: [''],
      customQuery: [''],
    });

    this.loadDiscoveredResources();

    this.form.get('kind')?.valueChanges.subscribe(() => {
      this.loadDiscoveredResources();
    });

    this.nsSub_ = this.namespaceService_.onNamespaceChangeEvent.subscribe(() => {
      const newNs = this.getActiveNamespace() || 'default';
      this.form.patchValue({namespace: newNs});
      this.loadDiscoveredResources();
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

  loadDiscoveredResources(): void {
    const kind = this.form?.get('kind')?.value?.toLowerCase();
    const ns = this.form?.get('namespace')?.value || this.getActiveNamespace() || 'default';
    if (!kind) return;

    this.isLoadingResources = true;
    const endpoint = kind === 'node' ? 'api/v1/node' : `api/v1/${kind}/${ns}`;

    this.http.get<ResourceListResponse>(endpoint).subscribe({
      next: res => {
        this.isLoadingResources = false;
        if (res?.items) {
          this.discoveredResources = res.items
            .map(i => i.objectMeta?.name)
            .filter((name): name is string => !!name);
        } else {
          this.discoveredResources = [];
        }
      },
      error: () => {
        this.isLoadingResources = false;
        this.discoveredResources = [];
      },
    });
  }

  analyze(): void {
    if (this.form.invalid) return;
    this.isAnalyzing = true;
    this.result = null;
    this.error = null;

    const req: AnalystRequest = this.form.value;
    this.aiService.runAnalysis(req).subscribe({
      next: result => {
        this.result = result;
        this.isAnalyzing = false;
      },
      error: err => {
        this.error = err.error?.message || 'AI analysis failed. Ensure AI Settings are configured.';
        this.isAnalyzing = false;
      },
    });
  }
}
