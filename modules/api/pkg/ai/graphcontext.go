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

package ai

import "context"

// GraphContextProvider supplies graph-grounded context for AI reasoning. The graph
// package (or a handler adapter) implements this so the ai package stays decoupled.
type GraphContextProvider interface {
	ContextForResource(ctx context.Context, kind, name, namespace string) (string, error)
	BlastRadiusSummary(ctx context.Context, kind, name, namespace string) (string, error)
}
