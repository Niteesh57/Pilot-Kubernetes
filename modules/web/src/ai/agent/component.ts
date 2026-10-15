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
import {MatSnackBar} from '@angular/material/snack-bar';
import {AIService, AgentRequest, AgentResponse} from '@common/services/global/ai.service';

@Component({
  selector: 'kd-ai-agent',
  templateUrl: './template.html',
  styleUrls: ['./style.scss'],
})
export class AIAgentComponent implements OnInit {
  form: FormGroup;
  isRunning = false;
  response: AgentResponse | null = null;
  error: string | null = null;

  readonly samplePrompts = [
    'Analyze all degraded or crashlooping pods and determine root cause',
    'Audit RBAC permissions for overprivileged ServiceAccounts',
    'Investigate inter-service communication latency and failed endpoints',
    'Generate least-privilege NetworkPolicy for backend services',
    'Assess blast radius and dependent workloads if worker node is drained',
  ];

  constructor(
    private readonly fb: FormBuilder,
    private readonly aiService: AIService,
    private readonly snackBar: MatSnackBar,
  ) {}

  ngOnInit(): void {
    this.form = this.fb.group({
      namespace: [''],
      query: ['', [Validators.required, Validators.minLength(3)]],
    });
  }

  useSample(prompt: string): void {
    this.form.patchValue({query: prompt});
    this.submit();
  }

  submit(): void {
    if (this.form.invalid || this.isRunning) return;

    this.isRunning = true;
    this.error = null;
    this.response = null;

    const req: AgentRequest = {
      namespace: this.form.value.namespace || undefined,
      query: this.form.value.query.trim(),
    };

    this.aiService.runAgent(req).subscribe({
      next: res => {
        this.response = res;
        this.isRunning = false;
      },
      error: err => {
        this.error = err.error?.message || 'Agent query failed. Please verify AI Settings and cluster connectivity.';
        this.isRunning = false;
      },
    });
  }

  copyToClipboard(text: string): void {
    navigator.clipboard.writeText(text).then(() => {
      this.snackBar.open('Copied to clipboard', 'Dismiss', {duration: 2500});
    });
  }
}
