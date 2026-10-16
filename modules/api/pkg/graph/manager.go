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

package graph

import (
	"context"
	"sync"

	"k8s.io/client-go/kubernetes"
)

var (
	managerMu sync.RWMutex
	manager   *Manager
)

// SetManager sets the process-wide graph manager used by handlers.
func SetManager(m *Manager) {
	managerMu.Lock()
	defer managerMu.Unlock()
	manager = m
}

// GetManager returns the process-wide graph manager, or nil when not started.
func GetManager() *Manager {
	managerMu.RLock()
	defer managerMu.RUnlock()
	return manager
}

// Manager owns the process-wide FalkorDB client and the cluster-to-graph syncer.
type Manager struct {
	mu       sync.RWMutex
	client   *Client
	syncer   *Syncer
	k8s      kubernetes.Interface
	interval int
	parent   context.Context
	cancel   context.CancelFunc
}

// NewManager creates a Manager with a FalkorDB client, but does not start syncing.
func NewManager(addr, password, graphName string, intervalSeconds int, k8s kubernetes.Interface) *Manager {
	m := &Manager{
		client:   NewClient(addr, password, graphName),
		k8s:      k8s,
		interval: intervalSeconds,
	}
	SetDefaultClient(m.client)
	return m
}

// clientProvider returns the current client safely, allowing the syncer to pick up reloads.
func (m *Manager) clientProvider() func() *Client {
	return func() *Client {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return m.client
	}
}

// Start launches the syncer in a background goroutine.
func (m *Manager) Start(parent context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.parent = parent
	m.startSyncerLocked()
}

// ReloadClient swaps the FalkorDB client and restarts the syncer with the new settings.
func (m *Manager) ReloadClient(addr, password, graphName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	old := m.client
	m.client = NewClient(addr, password, graphName)
	SetDefaultClient(m.client)
	if old != nil {
		_ = old.Close()
	}

	if m.parent != nil {
		m.startSyncerLocked()
	}
}

// SyncNow triggers an immediate sync cycle.
func (m *Manager) SyncNow(ctx context.Context) error {
	m.mu.RLock()
	syncer := m.syncer
	m.mu.RUnlock()
	if syncer == nil {
		return nil
	}
	return syncer.sync(ctx)
}

// Client returns the current FalkorDB client.
func (m *Manager) Client() *Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.client
}

// Close stops the syncer and closes the FalkorDB client.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
	}
	if m.client != nil {
		_ = m.client.Close()
	}
}

func (m *Manager) startSyncerLocked() {
	if m.cancel != nil {
		m.cancel()
	}
	ctx, cancel := context.WithCancel(m.parent)
	m.cancel = cancel
	m.syncer = NewSyncer(m.k8s, m.clientProvider(), m.interval)
	go m.syncer.Start(ctx)
}
