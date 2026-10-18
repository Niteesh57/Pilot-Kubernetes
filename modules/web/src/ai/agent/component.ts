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

import {
  AfterViewChecked,
  Component,
  ElementRef,
  OnDestroy,
  OnInit,
  ViewChild,
} from '@angular/core';
import {ActivatedRoute} from '@angular/router';
import {MatSnackBar} from '@angular/material/snack-bar';
import {Subscription} from 'rxjs';
import {
  AIService,
  AgentMessage,
  AgentRequest,
  AgentResponse,
  ResourcesResponse,
  extractStreamingSummary,
} from '@common/services/global/ai.service';
import {NamespaceService} from '@common/services/global/namespace';

export interface ConversationTurn {
  id: string;
  query: string;
  queryTime: Date;
  text: string;
  rawStream?: string;
  response?: AgentResponse;
  stepsExpanded?: boolean;
  isStreaming?: boolean;
}

export interface ChatMessage {
  id: string;
  role: 'user' | 'assistant';
  text: string;
  timestamp: Date;
  response?: AgentResponse;
  stepsExpanded?: boolean;
  isStreaming?: boolean;
}

export interface MentionOption {
  label: string;
  insertValue: string;
  kind: 'deployment' | 'pod' | 'service' | 'category';
  name: string;
  namespace?: string;
  status?: string;
}

@Component({
  selector: 'kd-ai-agent',
  templateUrl: './template.html',
  styleUrls: ['./style.scss'],
})
export class AIAgentComponent implements OnInit, OnDestroy, AfterViewChecked {
  @ViewChild('chatScrollContainer') private chatScrollContainer_?: ElementRef<HTMLDivElement>;
  @ViewChild('chatInput') private chatInput_?: ElementRef<HTMLTextAreaElement>;

  turns: ConversationTurn[] = [];

  get messages(): ChatMessage[] {
    const msgs: ChatMessage[] = [];
    for (const t of this.turns) {
      msgs.push({id: 'usr_' + t.id, role: 'user', text: t.query, timestamp: t.queryTime});
      if (t.text || t.isStreaming) {
        msgs.push({
          id: 'asst_' + t.id,
          role: 'assistant',
          text: t.text,
          timestamp: t.queryTime,
          response: t.response,
          stepsExpanded: t.stepsExpanded,
          isStreaming: t.isStreaming,
        });
      }
    }
    return msgs;
  }
  inputText = '';
  isThinking = false;
  isStreaming = false;
  private cancelStream_?: () => void;
  error: string | null = null;
  selectedNamespace = '';

  // Autocomplete Mentions
  showMentionPopup = false;
  filteredMentionOptions: MentionOption[] = [];
  selectedMentionIndex = 0;
  private currentMentionStartIndex = -1;

  // Cached Resources for @mention
  private resources: ResourcesResponse = {deployments: [], pods: [], services: []};
  private nsSub_?: Subscription;
  private queryParamsSub_?: Subscription;
  private shouldScrollBottom = false;

  readonly quickPrompts = [
    {
      title: 'Analyze Degraded Workloads',
      query: 'Analyze all degraded or crashlooping pods and determine root cause',
      icon: 'warning',
    },
    {
      title: 'Inter-Service Latency',
      query: 'Investigate inter-service communication latency and failure endpoints',
      icon: 'swap_horiz',
    },
    {
      title: 'Audit RBAC Privileges',
      query: 'Audit RBAC permissions for overprivileged ServiceAccounts and cluster-admin bindings',
      icon: 'security',
    },
    {
      title: 'Node Drain Blast Radius',
      query: 'Assess blast radius and dependent workloads if worker node is drained',
      icon: 'device_hub',
    },
  ];

  constructor(
    private readonly aiService: AIService,
    private readonly namespaceService_: NamespaceService,
    private readonly snackBar: MatSnackBar,
    private readonly route: ActivatedRoute,
  ) {}

  ngOnInit(): void {
    this.selectedNamespace = '';
    this.loadResources();

    this.queryParamsSub_ = this.route.queryParams.subscribe(params => {
      if (params['query']) {
        this.inputText = params['query'];
        this.sendMessage();
      }
    });
  }

  ngOnDestroy(): void {
    this.queryParamsSub_?.unsubscribe();
  }

  ngAfterViewChecked(): void {
    if (this.shouldScrollBottom) {
      this.scrollToBottom();
      this.shouldScrollBottom = false;
    }
  }

  loadResources(): void {
    // SRE Agent operates cluster-wide across all namespaces
    this.aiService.getResources().subscribe({
      next: res => {
        this.resources = res;
      },
      error: () => {
        // Fallback gracefully
        this.resources = {deployments: [], pods: [], services: []};
      },
    });
  }

  // ─── Chat Message Actions ──────────────────────────────────────────────────

  sendMessage(customText?: string): void {
    const textToSend = (customText !== undefined ? customText : this.inputText).trim();
    if (!textToSend || this.isThinking) return;

    this.showMentionPopup = false;
    this.inputText = '';
    this.error = null;
    this.isThinking = true;
    this.isStreaming = true;
    this.shouldScrollBottom = true;

    const turn: ConversationTurn = {
      id: 'turn_' + Date.now(),
      query: textToSend,
      queryTime: new Date(),
      text: '',
      response: {
        query: textToSend,
        steps: [],
        summary: '',
      },
      stepsExpanded: false,
      isStreaming: true,
    };
    this.turns.push(turn);

    // Build conversation history from prior completed turns
    const history: AgentMessage[] = [];
    for (const t of this.turns.slice(0, -1)) {
      history.push({role: 'user', content: t.query});
      if (t.text) {
        history.push({role: 'assistant', content: t.text});
      }
    }

    const req: AgentRequest = {
      namespace: this.selectedNamespace || undefined,
      query: textToSend,
      history: history.length > 0 ? history : undefined,
    };

    this.cancelStream_ = this.aiService.runAgentStream(
      req,
      // onStep
      step => {
        if (!turn.response) {
          turn.response = {query: textToSend, steps: [], summary: ''};
        }
        const existingIdx = turn.response.steps.findIndex(s => s.step === step.step);
        if (existingIdx >= 0) {
          turn.response.steps[existingIdx] = step;
        } else {
          turn.response.steps.push(step);
        }
        this.shouldScrollBottom = true;
      },
      // onToken
      token => {
        turn.rawStream = (turn.rawStream || '') + token;
        turn.text = extractStreamingSummary(turn.rawStream);
        this.shouldScrollBottom = true;
      },
      // onComplete
      res => {
        turn.response = res;
        // Always display the extracted clean markdown summary
        if (res.summary) {
          turn.text = res.summary;
        } else if (turn.text) {
          turn.text = extractStreamingSummary(turn.text);
        }
        turn.isStreaming = false;
        this.isThinking = false;
        this.isStreaming = false;
        this.shouldScrollBottom = true;
        this.cancelStream_ = undefined;
      },
      // onError
      err => {
        this.error = err.message || 'SRE Agent encountered an error executing query.';
        turn.isStreaming = false;
        this.isThinking = false;
        this.isStreaming = false;
        this.shouldScrollBottom = true;
        this.cancelStream_ = undefined;
      },
    );
  }

  stopGeneration(): void {
    if (this.cancelStream_) {
      this.cancelStream_();
      this.cancelStream_ = undefined;
    }
    this.isThinking = false;
    this.isStreaming = false;
    if (this.turns.length > 0) {
      const last = this.turns[this.turns.length - 1];
      last.isStreaming = false;
    }
  }

  askFollowUp(question: string): void {
    this.sendMessage(question);
  }

  clearChat(): void {
    this.turns = [];
    this.error = null;
  }

  useQuickPrompt(promptText: string): void {
    this.inputText = promptText;
    if (this.chatInput_) {
      this.chatInput_.nativeElement.focus();
    }
  }

  toggleSteps(item: ConversationTurn | ChatMessage): void {
    item.stepsExpanded = !item.stepsExpanded;
  }

  copyToClipboard(text: string, label: string = 'Copied to clipboard'): void {
    navigator.clipboard.writeText(text).then(() => {
      this.snackBar.open(label, 'Dismiss', {duration: 2500});
    });
  }

  private scrollToBottom(): void {
    try {
      if (this.chatScrollContainer_) {
        this.chatScrollContainer_.nativeElement.scrollTop =
          this.chatScrollContainer_.nativeElement.scrollHeight;
      }
    } catch {
      // ignore
    }
  }

  // ─── @mention Autocomplete Engine ──────────────────────────────────────────

  onInputChange(event: Event): void {
    const input = event.target as HTMLTextAreaElement;
    if (!input) return;

    const cursorPos = input.selectionStart || 0;
    const textBefore = this.inputText.slice(0, cursorPos);

    // Look for last '@' with no space after it
    const lastAtIndex = textBefore.lastIndexOf('@');
    if (lastAtIndex >= 0) {
      const fragment = textBefore.slice(lastAtIndex + 1);
      // If there is a space after the @ symbol, ignore unless it's within a slash token
      if (!fragment.includes(' ')) {
        this.currentMentionStartIndex = lastAtIndex;
        this.filterMentionOptions(fragment);
        return;
      }
    }

    this.showMentionPopup = false;
  }

  onKeyDown(event: KeyboardEvent): void {
    if (this.showMentionPopup && this.filteredMentionOptions.length > 0) {
      if (event.key === 'ArrowDown') {
        event.preventDefault();
        this.selectedMentionIndex =
          (this.selectedMentionIndex + 1) % this.filteredMentionOptions.length;
        return;
      }
      if (event.key === 'ArrowUp') {
        event.preventDefault();
        this.selectedMentionIndex =
          (this.selectedMentionIndex - 1 + this.filteredMentionOptions.length) %
          this.filteredMentionOptions.length;
        return;
      }
      if (event.key === 'Enter' || event.key === 'Tab') {
        event.preventDefault();
        this.selectMentionOption(this.filteredMentionOptions[this.selectedMentionIndex]);
        return;
      }
      if (event.key === 'Escape') {
        event.preventDefault();
        this.showMentionPopup = false;
        return;
      }
    }

    // Submit on Enter (without Shift)
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault();
      this.sendMessage();
    }
  }

  private filterMentionOptions(query: string): void {
    const q = query.toLowerCase().trim();
    const options: MentionOption[] = [];

    const hasDepPrefix = q.startsWith('dep') || q.startsWith('deployment/');
    const hasPodPrefix = q.startsWith('pod') || q.startsWith('pod/');
    const hasSvcPrefix = q.startsWith('svc') || q.startsWith('service') || q.startsWith('service/');

    // 1. If user just typed '@' or '@d' or '@p' or '@s', show category starters first
    if (q === '' || q === 'd' || q === 'dep') {
      options.push({
        label: '@deployment/...',
        insertValue: '@deployment/',
        kind: 'category',
        name: 'Deployments',
        status: `${this.resources.deployments.length} available`,
      });
    }
    if (q === '' || q === 'p' || q === 'pod') {
      options.push({
        label: '@pod/...',
        insertValue: '@pod/',
        kind: 'category',
        name: 'Pods',
        status: `${this.resources.pods.length} available`,
      });
    }
    if (q === '' || q === 's' || q === 'svc' || q === 'service') {
      options.push({
        label: '@service/...',
        insertValue: '@service/',
        kind: 'category',
        name: 'Services',
        status: `${this.resources.services.length} available`,
      });
    }

    // 2. Extract resource-specific search term
    let depSearch = '';
    let podSearch = '';
    let svcSearch = '';

    if (q.includes('/')) {
      const parts = q.split('/');
      const kindPart = parts[0];
      const namePart = parts.slice(1).join('/');
      if (kindPart.startsWith('dep')) depSearch = namePart;
      else if (kindPart.startsWith('pod')) podSearch = namePart;
      else if (kindPart.startsWith('svc') || kindPart.startsWith('service')) svcSearch = namePart;
    } else {
      depSearch = q;
      podSearch = q;
      svcSearch = q;
    }

    // Deployments
    if (hasDepPrefix || (!hasPodPrefix && !hasSvcPrefix)) {
      for (const d of this.resources.deployments) {
        if (!depSearch || d.name.toLowerCase().includes(depSearch)) {
          options.push({
            label: `@deployment/${d.name}`,
            insertValue: `@deployment/${d.name} `,
            kind: 'deployment',
            name: d.name,
            namespace: d.namespace,
            status: d.status,
          });
        }
      }
    }

    // Pods
    if (hasPodPrefix || (!hasDepPrefix && !hasSvcPrefix)) {
      for (const p of this.resources.pods) {
        if (!podSearch || p.name.toLowerCase().includes(podSearch)) {
          options.push({
            label: `@pod/${p.name}`,
            insertValue: `@pod/${p.name} `,
            kind: 'pod',
            name: p.name,
            namespace: p.namespace,
            status: p.status,
          });
        }
      }
    }

    // Services
    if (hasSvcPrefix || (!hasDepPrefix && !hasPodPrefix)) {
      for (const s of this.resources.services) {
        if (!svcSearch || s.name.toLowerCase().includes(svcSearch)) {
          options.push({
            label: `@service/${s.name}`,
            insertValue: `@service/${s.name} `,
            kind: 'service',
            name: s.name,
            namespace: s.namespace,
            status: s.status,
          });
        }
      }
    }

    this.filteredMentionOptions = options.slice(0, 16);
    this.selectedMentionIndex = 0;
    this.showMentionPopup = this.filteredMentionOptions.length > 0;
  }

  selectMentionOption(option: MentionOption): void {
    if (!this.chatInput_ || this.currentMentionStartIndex < 0) return;

    const textarea = this.chatInput_.nativeElement;
    const cursorPos = textarea.selectionStart || 0;

    const prefix = this.inputText.slice(0, this.currentMentionStartIndex);
    const suffix = this.inputText.slice(cursorPos);

    this.inputText = prefix + option.insertValue + suffix;
    const newCursorPos = (prefix + option.insertValue).length;

    this.showMentionPopup = false;
    setTimeout(() => {
      textarea.focus();
      textarea.setSelectionRange(newCursorPos, newCursorPos);
      // If user selected a category e.g. @deployment/, re-open popup showing deployments!
      if (option.kind === 'category') {
        const kindPrefix = option.insertValue.replace('@', '').toLowerCase();
        this.currentMentionStartIndex = this.inputText.lastIndexOf('@', newCursorPos);
        this.filterMentionOptions(kindPrefix);
      }
    }, 0);
  }
}
