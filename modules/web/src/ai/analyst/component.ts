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
import {FormBuilder, FormGroup, Validators} from '@angular/forms';
import {AIService, AnalystRequest, AnalystResponse} from '@common/services/global/ai.service';

@Component({
  selector: 'kd-ai-analyst',
  templateUrl: './template.html',
  styleUrls: ['./style.scss'],
})
export class AIAnalystComponent implements OnInit {
  form: FormGroup;
  isAnalyzing = false;
  result: AnalystResponse | null = null;
  error: string | null = null;

  readonly kinds = ['Pod', 'Deployment', 'StatefulSet', 'DaemonSet', 'ReplicaSet', 'Job', 'CronJob', 'Service', 'Node'];

  constructor(
    private readonly fb: FormBuilder,
    private readonly aiService: AIService,
  ) {}

  ngOnInit(): void {
    this.form = this.fb.group({
      namespace: ['default', Validators.required],
      kind: ['Pod', Validators.required],
      name: ['', Validators.required],
      containerName: [''],
      customQuery: [''],
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
