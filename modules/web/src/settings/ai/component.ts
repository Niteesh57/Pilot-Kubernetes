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
import {AIService, AISettings, VerifyResponse} from '@common/services/global/ai.service';

interface ProviderOption {
  value: string;
  label: string;
  defaultUrl: string;
  defaultModel: string;
}

@Component({
  selector: 'kd-ai-settings',
  templateUrl: './template.html',
  styleUrls: ['./style.scss'],
})
export class AISettingsComponent implements OnInit {
  form: FormGroup;
  isLoading = false;
  isSaving = false;
  isVerifying = false;
  verifyResult: VerifyResponse | null = null;
  availableModels: string[] = [];
  showApiKey = false;

  readonly providers: ProviderOption[] = [
    {value: 'openai', label: 'OpenAI', defaultUrl: 'https://api.openai.com/v1', defaultModel: 'gpt-4o-mini'},
    {value: 'gemini', label: 'Google Gemini', defaultUrl: 'https://generativelanguage.googleapis.com/v1beta', defaultModel: 'gemini-1.5-flash'},
    {value: 'anthropic', label: 'Anthropic Claude', defaultUrl: 'https://api.anthropic.com/v1', defaultModel: 'claude-3-5-sonnet-20241022'},
    {value: 'grok', label: 'Grok (xAI)', defaultUrl: 'https://api.x.ai/v1', defaultModel: 'grok-beta'},
    {value: 'ollama', label: 'Ollama / Local LLM', defaultUrl: 'http://localhost:11434/v1', defaultModel: 'llama3.2'},
    {value: 'custom', label: 'Custom (OpenAI-Compatible)', defaultUrl: '', defaultModel: ''},
  ];

  constructor(
    private readonly fb: FormBuilder,
    private readonly aiService: AIService,
    private readonly snackBar: MatSnackBar,
  ) {}

  ngOnInit(): void {
    this.form = this.fb.group({
      provider: ['openai', Validators.required],
      baseUrl: ['https://api.openai.com/v1', Validators.required],
      apiKey: [''],
      model: ['gpt-4o-mini', Validators.required],
      maxTokens: [2048, [Validators.required, Validators.min(256), Validators.max(32768)]],
      temperature: [0.3],
      falkordbAddr: ['falkordb.kubepilot.svc.cluster.local:6379'],
      falkordbGraphName: ['kubepilot'],
    });

    this.loadSettings();

    // Auto-update defaults when provider changes
    this.form.get('provider').valueChanges.subscribe(provider => {
      const opt = this.providers.find(p => p.value === provider);
      if (opt) {
        if (this.form.get('baseUrl').value === '' ||
            this.providers.some(p => p.defaultUrl === this.form.get('baseUrl').value)) {
          this.form.patchValue({baseUrl: opt.defaultUrl, model: opt.defaultModel}, {emitEvent: false});
        }
      }
      this.verifyResult = null;
      this.availableModels = [];
    });
  }

  loadSettings(): void {
    this.isLoading = true;
    this.aiService.getSettings().subscribe({
      next: settings => {
        this.form.patchValue({
          provider: settings.provider || 'openai',
          baseUrl: settings.baseUrl || '',
          apiKey: '',
          model: settings.model || '',
          maxTokens: settings.maxTokens || 2048,
          temperature: settings.temperature || 0.3,
          falkordbAddr: settings.falkordbAddr || 'falkordb.kubepilot.svc.cluster.local:6379',
          falkordbGraphName: settings.falkordbGraphName || 'kubepilot',
        });
        this.isLoading = false;
      },
      error: () => {
        this.isLoading = false;
      },
    });
  }

  onProviderChange(provider: string): void {
    const opt = this.providers.find(p => p.value === provider);
    if (opt) {
      this.form.patchValue({baseUrl: opt.defaultUrl, model: opt.defaultModel});
    }
    this.verifyResult = null;
    this.availableModels = [];
  }

  verifyConnection(): void {
    this.isVerifying = true;
    this.verifyResult = null;
    const v = this.form.value;
    this.aiService.verifyConnection({
      provider: v.provider,
      baseUrl: v.baseUrl,
      apiKey: v.apiKey,
      model: v.model,
    }).subscribe({
      next: result => {
        this.verifyResult = result;
        this.isVerifying = false;
        if (result.connected && result.models?.length) {
          this.availableModels = result.models;
          // Auto-select current model if in list, else pick first
          if (!result.models.includes(this.form.get('model').value)) {
            this.form.patchValue({model: result.models[0]}, {emitEvent: false});
          }
        }
      },
      error: err => {
        this.isVerifying = false;
        this.verifyResult = {connected: false, latencyMs: 0, models: [], errorMsg: err.message || 'Connection failed'};
      },
    });
  }

  save(): void {
    if (this.form.invalid) return;
    this.isSaving = true;
    const settings: AISettings = this.form.value;
    this.aiService.saveSettings(settings).subscribe({
      next: () => {
        this.isSaving = false;
        this.snackBar.open('AI settings saved successfully', 'Dismiss', {duration: 3000});
      },
      error: () => {
        this.isSaving = false;
        this.snackBar.open('Failed to save settings. Check cluster permissions.', 'Dismiss', {duration: 5000});
      },
    });
  }
}
