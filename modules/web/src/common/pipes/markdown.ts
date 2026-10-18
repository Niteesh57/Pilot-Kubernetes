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

import {Pipe, PipeTransform} from '@angular/core';
import {DomSanitizer, SafeHtml} from '@angular/platform-browser';

/**
 * Escapes raw HTML characters to prevent XSS.
 */
function escapeHtml(text: string): string {
  const map: Record<string, string> = {
    '&': '&amp;',
    '<': '&lt;',
    '>': '&gt;',
    '"': '&quot;',
    "'": '&#039;',
  };
  return text.replace(/[&<>"']/g, m => map[m]);
}

/**
 * Parses Markdown formatted text into structured HTML with Kubernetes styling.
 */
export function parseMarkdownToHtml(markdown: string | null | undefined): string {
  if (!markdown) {
    return '';
  }

  // Normalize newlines
  let src = markdown.replace(/\r\n/g, '\n').replace(/\r/g, '\n');

  // Extract fenced code blocks first
  const codeBlocks: string[] = [];
  src = src.replace(/```([a-zA-Z0-9_-]*)\n([\s\S]*?)```/g, (_, lang, code) => {
    const escapedCode = escapeHtml(code.trimEnd());
    const langBadge = lang ? `<span class="kd-md-code-lang">${escapeHtml(lang)}</span>` : '';
    const html = `<div class="kd-md-code-block">${langBadge ? `<div class="kd-md-code-header">${langBadge}</div>` : ''}<pre><code class="language-${lang || 'none'}">${escapedCode}</code></pre></div>`;
    codeBlocks.push(html);
    return `@@@KDCODEBLOCK${codeBlocks.length - 1}@@@`;
  });

  // Extract inline code
  const inlineCodes: string[] = [];
  src = src.replace(/`([^`\n]+)`/g, (_, code) => {
    const escaped = escapeHtml(code);
    inlineCodes.push(`<code class="kd-md-inline-code">${escaped}</code>`);
    return `@@@KDINLINECODE${inlineCodes.length - 1}@@@`;
  });

  // Split into lines
  const rawLines = src.split('\n');
  const result: string[] = [];
  let inUl = false;
  let inOl = false;
  let inBlockquote = false;
  let currentParagraph: string[] = [];

  const flushParagraph = () => {
    if (currentParagraph.length > 0) {
      const pText = currentParagraph.join(' ');
      if (pText.trim()) {
        result.push(`<p class="kd-md-p">${processInline(pText)}</p>`);
      }
      currentParagraph = [];
    }
  };

  const closeLists = () => {
    if (inUl) {
      result.push('</ul>');
      inUl = false;
    }
    if (inOl) {
      result.push('</ol>');
      inOl = false;
    }
    if (inBlockquote) {
      result.push('</blockquote>');
      inBlockquote = false;
    }
  };

  function processInline(text: string): string {
    // Links: [text](url)
    text = text.replace(
      /\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g,
      '<a class="kd-md-link" href="$2" target="_blank" rel="noopener noreferrer">$1</a>',
    );

    // Bold: **text** or __text__
    text = text.replace(/\*\*([^*]+)\*\*/g, '<strong class="kd-md-bold">$1</strong>');
    text = text.replace(/__([^_]+)__/g, '<strong class="kd-md-bold">$1</strong>');

    // Italic: *text* or _text_
    text = text.replace(/\*([^*]+)\*/g, '<em class="kd-md-italic">$1</em>');
    text = text.replace(/_([^_]+)_/g, '<em class="kd-md-italic">$1</em>');

    return text;
  }

  for (let i = 0; i < rawLines.length; i++) {
    const line = rawLines[i];
    const trimmed = line.trim();

    // Check code block placeholder
    if (trimmed.startsWith('@@@KDCODEBLOCK') && trimmed.endsWith('@@@')) {
      flushParagraph();
      closeLists();
      result.push(trimmed);
      continue;
    }

    // Empty line separates blocks
    if (trimmed === '') {
      flushParagraph();
      closeLists();
      continue;
    }

    // Horizontal rule: --- or *** or ___
    if (/^(?:---|\*\*\*|___)\s*$/.test(trimmed)) {
      flushParagraph();
      closeLists();
      result.push('<hr class="kd-md-hr" />');
      continue;
    }

    // Headings: #, ##, ###, ####
    const headingMatch = trimmed.match(/^(#{1,6})\s+(.*)$/);
    if (headingMatch) {
      flushParagraph();
      closeLists();
      const level = headingMatch[1].length;
      const hText = processInline(escapeHtml(headingMatch[2]));
      result.push(`<h${level} class="kd-md-h${level}">${hText}</h${level}>`);
      continue;
    }

    // Blockquote: > text
    if (trimmed.startsWith('>')) {
      flushParagraph();
      if (!inBlockquote) {
        if (inUl || inOl) closeLists();
        result.push('<blockquote class="kd-md-blockquote">');
        inBlockquote = true;
      }
      const bText = processInline(escapeHtml(trimmed.substring(1).trim()));
      result.push(`<p class="kd-md-blockquote-p">${bText}</p>`);
      continue;
    } else if (inBlockquote) {
      result.push('</blockquote>');
      inBlockquote = false;
    }

    // Unordered List: - item, * item, + item
    const ulMatch = trimmed.match(/^[-*+]\s+(.*)$/);
    if (ulMatch) {
      flushParagraph();
      if (inOl) {
        result.push('</ol>');
        inOl = false;
      }
      if (!inUl) {
        result.push('<ul class="kd-md-ul">');
        inUl = true;
      }
      const itemText = processInline(escapeHtml(ulMatch[1]));
      result.push(`<li class="kd-md-li">${itemText}</li>`);
      continue;
    }

    // Ordered List: 1. item
    const olMatch = trimmed.match(/^(\d+)\.\s+(.*)$/);
    if (olMatch) {
      flushParagraph();
      if (inUl) {
        result.push('</ul>');
        inUl = false;
      }
      if (!inOl) {
        result.push('<ol class="kd-md-ol">');
        inOl = true;
      }
      const itemText = processInline(escapeHtml(olMatch[2]));
      result.push(`<li class="kd-md-li">${itemText}</li>`);
      continue;
    }

    // Normal paragraph line
    closeLists();
    currentParagraph.push(escapeHtml(trimmed));
  }

  flushParagraph();
  closeLists();

  let finalHtml = result.join('\n');

  // Restore inline codes
  finalHtml = finalHtml.replace(/@@@KDINLINECODE(\d+)@@@/g, (_, idx) => {
    return inlineCodes[parseInt(idx, 10)] || '';
  });

  // Restore code blocks
  finalHtml = finalHtml.replace(/@@@KDCODEBLOCK(\d+)@@@/g, (_, idx) => {
    return codeBlocks[parseInt(idx, 10)] || '';
  });

  return finalHtml;
}

@Pipe({name: 'kdMarkdown'})
export class MarkdownPipe implements PipeTransform {
  constructor(private readonly sanitizer_: DomSanitizer) {}

  transform(value: string | null | undefined): SafeHtml {
    if (!value) return '';
    const html = parseMarkdownToHtml(value);
    return this.sanitizer_.bypassSecurityTrustHtml(html);
  }
}
