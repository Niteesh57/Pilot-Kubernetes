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

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode"

	restful "github.com/emicklei/go-restful/v3"

	kubernetes "k8s.io/client-go/kubernetes"
	"k8s.io/dashboard/api/pkg/ai"
	"k8s.io/dashboard/api/pkg/graph"
	"k8s.io/dashboard/api/pkg/traffic"
	"k8s.io/dashboard/client"
	"k8s.io/dashboard/errors"
	"k8s.io/klog/v2"
)

type nlQueryRequest struct {
	Question string `json:"question"`
}

type nlQueryResponse struct {
	Cypher  string      `json:"cypher"`
	Columns []string    `json:"columns"`
	Rows    [][]string  `json:"rows"`
	Result  interface{} `json:"result,omitempty"`
}

const cypherSchemaPrompt = `You are a FalkorDB (Cypher) query generator for a Kubernetes knowledge graph.
Graph labels & properties:
- Pod: name, namespace, status, phase, nodeName, restarts, cpuMilli, memoryMi
- Deployment: name, namespace, desired, ready, available, replicas, status
- StatefulSet / DaemonSet / Job: name, namespace, desired, ready, replicas, status
- Service: name, namespace, type, clusterIP, status
- Node: name, ready, status
Relationships:
- (Pod)-[:RUNS_ON]->(Node)
- (Pod)-[:IN_NAMESPACE]->(Namespace)
- (Pod)-[:MANAGED_BY]->(Deployment|StatefulSet|DaemonSet)
- (Service)-[:SELECTS]->(Pod)
- (Service)-[:EXPOSES]->(Deployment)
- (Ingress)-[:ROUTES_TO]->(Service)
- (Pod)-[:USES_ACCOUNT]->(ServiceAccount)
- (Pod)-[:MOUNTS_CONFIG]->(ConfigMap)
- (Pod)-[:MOUNTS_SECRET]->(Secret)
- (Pod)-[:CLAIMS_STORAGE]->(PersistentVolumeClaim)
- (ServiceAccount)-[:BOUND_BY]->(RoleBinding|ClusterRoleBinding)
- (RoleBinding|ClusterRoleBinding)-[:GRANTS]->(Role|ClusterRole)
- (Pod)-[:EMITTED]->(LogEvent)

Important Guidelines:
1. Always return a valid, read-only Cypher query (never CREATE, DELETE, MERGE, SET, DROP, REMOVE).
2. For top CPU pods: MATCH (p:Pod) RETURN p.name, p.namespace, COALESCE(p.cpuMilli, 0) AS cpu_milli, COALESCE(p.memoryMi, 0) AS mem_mi ORDER BY cpu_milli DESC LIMIT 10
3. For finding replicas of workloads: MATCH (d:Deployment) RETURN d.name, d.namespace, d.replicas, d.desired, d.available, d.status
4. For which pod consumed the most CPU and find replicas: OPTIONAL MATCH (p:Pod)-[:MANAGED_BY]->(d:Deployment) RETURN p.name, p.namespace, COALESCE(p.cpuMilli, 0) AS cpu_milli, d.name AS deployment, d.replicas AS replicas ORDER BY cpu_milli DESC LIMIT 10
5. Do NOT include markdown fences, comments, or explanations. Output ONLY the raw Cypher query.`

// installAIRoutes registers all AI Platform routes on the given WebService.
// Call this from CreateHTTPAPIHandler after wsContainer.Add(apiV1Ws).
func (apiHandler *APIHandler) installAIRoutes(ws *restful.WebService) {

	// ─── AI Settings ──────────────────────────────────────────────
	ws.Route(ws.GET("/ai/settings").To(apiHandler.handleGetAISettings).
		Doc("Returns the current AI provider settings (API key masked)").
		Returns(http.StatusOK, "OK", ai.AISettingsResponse{}))

	ws.Route(ws.PUT("/ai/settings").To(apiHandler.handleSaveAISettings).
		Doc("Saves AI provider configuration to the cluster Secret").
		Reads(ai.AISettings{}).
		Returns(http.StatusOK, "OK", nil))

	ws.Route(ws.POST("/ai/verify").To(apiHandler.handleVerifyAI).
		Doc("Tests connectivity to an AI provider endpoint").
		Reads(ai.VerifyRequest{}).
		Returns(http.StatusOK, "OK", ai.VerifyResponse{}))

	// ─── AI Analyst ───────────────────────────────────────────────
	ws.Route(ws.POST("/ai/analyst").To(apiHandler.handleAnalyst).
		Doc("Performs root cause analysis on the specified Kubernetes resource").
		Reads(ai.AnalystRequest{}).
		Returns(http.StatusOK, "OK", ai.AnalystResponse{}))

	// ─── AI Improvement ───────────────────────────────────────────
	ws.Route(ws.POST("/ai/improvement").To(apiHandler.handleImprovement).
		Doc("Audits the cluster for reliability, resource, and health issues").
		Returns(http.StatusOK, "OK", ai.ImprovementResponse{}))

	ws.Route(ws.GET("/ai/improvement/{namespace}").To(apiHandler.handleImprovement).
		Doc("Audits a specific namespace for reliability and resource issues").
		Param(ws.PathParameter("namespace", "namespace to audit")).
		Returns(http.StatusOK, "OK", ai.ImprovementResponse{}))
	ws.Route(ws.POST("/ai/improvement/{namespace}").To(apiHandler.handleImprovement).
		Doc("Audits a specific namespace for reliability and resource issues").
		Param(ws.PathParameter("namespace", "namespace to audit")).
		Returns(http.StatusOK, "OK", ai.ImprovementResponse{}))

	// ─── AI Security ──────────────────────────────────────────────
	ws.Route(ws.POST("/ai/security").To(apiHandler.handleSecurity).
		Doc("Runs RBAC and workload security audit for the cluster").
		Returns(http.StatusOK, "OK", ai.SecurityResponse{}))

	ws.Route(ws.GET("/ai/security/{namespace}").To(apiHandler.handleSecurity).
		Doc("Runs security audit for a specific namespace").
		Param(ws.PathParameter("namespace", "namespace to audit")).
		Returns(http.StatusOK, "OK", ai.SecurityResponse{}))
	ws.Route(ws.POST("/ai/security/{namespace}").To(apiHandler.handleSecurity).
		Doc("Runs security audit for a specific namespace").
		Param(ws.PathParameter("namespace", "namespace to audit")).
		Returns(http.StatusOK, "OK", ai.SecurityResponse{}))

	// ─── SRE Agent ────────────────────────────────────────────────
	ws.Route(ws.POST("/ai/agent").To(apiHandler.handleAgent).
		Doc("Executes the autonomous SRE problem-solving agent").
		Reads(ai.AgentRequest{}).
		Returns(http.StatusOK, "OK", ai.AgentResponse{}))

	ws.Route(ws.POST("/ai/agent/stream").To(apiHandler.handleAgentStream).
		Doc("Executes the autonomous SRE problem-solving agent with real-time SSE streaming").
		Reads(ai.AgentRequest{}).
		Produces("text/event-stream").
		Returns(http.StatusOK, "OK", nil))

	ws.Route(ws.GET("/ai/resources").To(apiHandler.handleAIResources).
		Doc("Returns available deployments, pods, and services for @mention autocomplete").
		Returns(http.StatusOK, "OK", ai.ResourcesResponse{}))

	ws.Route(ws.GET("/ai/resources/{namespace}").To(apiHandler.handleAIResources).
		Doc("Returns available deployments, pods, and services in a namespace for @mention autocomplete").
		Param(ws.PathParameter("namespace", "target namespace")).
		Returns(http.StatusOK, "OK", ai.ResourcesResponse{}))

	// ─── FalkorDB Graph ───────────────────────────────────────────
	ws.Route(ws.GET("/graph/status").To(apiHandler.handleGraphStatus).
		Doc("Returns FalkorDB connection status and graph statistics").
		Returns(http.StatusOK, "OK", graph.FalkorDBStatus{}))

	ws.Route(ws.POST("/graph/query").To(apiHandler.handleGraphQuery).
		Doc("Executes a read-only Cypher query on the cluster knowledge graph").
		Returns(http.StatusOK, "OK", nil))

	ws.Route(ws.GET("/graph/blastradius/{kind}/{namespace}/{name}").To(apiHandler.handleBlastRadius).
		Doc("Computes the blast radius (dependents and dependencies) for the given resource").
		Param(ws.PathParameter("kind", "resource kind (Pod, Service, Deployment, etc.)")).
		Param(ws.PathParameter("namespace", "namespace of the resource")).
		Param(ws.PathParameter("name", "name of the resource")).
		Returns(http.StatusOK, "OK", graph.BlastRadiusResult{}))

	ws.Route(ws.GET("/graph/topology").To(apiHandler.handleGraphTopology).
		Doc("Returns the full typed cluster knowledge graph from FalkorDB").
		Returns(http.StatusOK, "OK", graph.GraphResult{}))

	ws.Route(ws.GET("/graph/explore/{kind}/{namespace}/{name}").To(apiHandler.handleGraphExplore).
		Doc("Returns the typed neighborhood subgraph for the given resource").
		Param(ws.PathParameter("kind", "resource kind")).
		Param(ws.PathParameter("namespace", "namespace of the resource")).
		Param(ws.PathParameter("name", "name of the resource")).
		Returns(http.StatusOK, "OK", graph.GraphResult{}))

	ws.Route(ws.GET("/graph/attackpaths").To(apiHandler.handleAttackPaths).
		Doc("Returns privilege-escalation attack paths across all namespaces").
		Returns(http.StatusOK, "OK", []graph.AttackPath{}))

	ws.Route(ws.GET("/graph/attackpaths/{namespace}").To(apiHandler.handleAttackPaths).
		Doc("Returns privilege-escalation attack paths for a namespace").
		Param(ws.PathParameter("namespace", "namespace to inspect")).
		Returns(http.StatusOK, "OK", []graph.AttackPath{}))

	ws.Route(ws.POST("/graph/sync").To(apiHandler.handleGraphSync).
		Doc("Triggers an immediate cluster-to-graph synchronization cycle").
		Returns(http.StatusOK, "OK", nil))

	ws.Route(ws.GET("/graph/catalog").To(apiHandler.handleGraphCatalog).
		Doc("Returns the curated Cypher query catalog").
		Returns(http.StatusOK, "OK", []graph.CatalogQuery{}))

	ws.Route(ws.POST("/graph/nl-query").To(apiHandler.handleNLQuery).
		Doc("Translates a natural-language question into a read-only Cypher query and runs it").
		Reads(nlQueryRequest{}).
		Returns(http.StatusOK, "OK", nlQueryResponse{}))

	ws.Route(ws.GET("/graph/dashboards").To(apiHandler.handleListDashboards).
		Doc("Returns saved custom dashboards").
		Returns(http.StatusOK, "OK", []graph.Dashboard{}))

	ws.Route(ws.POST("/graph/dashboards").To(apiHandler.handleSaveDashboard).
		Doc("Saves a custom dashboard to the graph").
		Reads(graph.Dashboard{}).
		Returns(http.StatusOK, "OK", nil))

	ws.Route(ws.DELETE("/graph/dashboards/{name}").To(apiHandler.handleDeleteDashboard).
		Doc("Deletes a saved custom dashboard").
		Param(ws.PathParameter("name", "dashboard name")).
		Returns(http.StatusOK, "OK", nil))

	ws.Route(ws.GET("/graph/diagnose/{namespace}/{name}").To(apiHandler.handleDiagnoseService).
		Doc("Correlates a service with pods, metrics, and recent logs").
		Param(ws.PathParameter("namespace", "namespace of the service")).
		Param(ws.PathParameter("name", "name of the service")).
		Returns(http.StatusOK, "OK", graph.ServiceDiagnosis{}))

	// ─── Traffic Topology ─────────────────────────────────────────
	ws.Route(ws.GET("/traffic").To(apiHandler.handleTraffic).
		Doc("Returns live service interconnection topology for all namespaces").
		Returns(http.StatusOK, "OK", traffic.TopologyGraph{}))

	ws.Route(ws.GET("/traffic/{namespace}").To(apiHandler.handleTraffic).
		Doc("Returns service topology for a specific namespace").
		Param(ws.PathParameter("namespace", "namespace to inspect")).
		Returns(http.StatusOK, "OK", traffic.TopologyGraph{}))
}

// getK8sClient returns a Kubernetes client for the request. If an authorization token is present,
// it uses the cached/user client; otherwise it falls back to the in-cluster service account client.
func getK8sClient(req *http.Request) (kubernetes.Interface, error) {
	token := client.GetBearerToken(req)
	if token != "" {
		c, err := client.Client(req)
		if err == nil && c != nil {
			return c, nil
		}
	}
	if inc := client.InClusterClient(); inc != nil {
		return inc, nil
	}
	return client.Client(req)
}

// ─── AI Settings Handlers ─────────────────────────────────────────────────────

func (apiHandler *APIHandler) handleGetAISettings(request *restful.Request, response *restful.Response) {
	k8sClient, err := getK8sClient(request.Request)
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}
	settings := ai.LoadAISettings(k8sClient)
	_ = response.WriteHeaderAndEntity(http.StatusOK, ai.MaskSettings(settings))
}

func (apiHandler *APIHandler) handleSaveAISettings(request *restful.Request, response *restful.Response) {
	k8sClient, err := getK8sClient(request.Request)
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}

	settings := new(ai.AISettings)
	if err := request.ReadEntity(settings); err != nil {
		errors.HandleInternalError(response, err)
		return
	}

	// If API key is empty or masked (dots), preserve the existing configured key
	if settings.APIKey == "" || isAPIKeyMasked(settings.APIKey) {
		existing := ai.LoadAISettings(k8sClient)
		if existing != nil && existing.APIKey != "" {
			settings.APIKey = existing.APIKey
		}
	}

	if err := ai.SaveAISettings(k8sClient, settings); err != nil {
		errors.HandleInternalError(response, err)
		return
	}

	// Reinitialise graph client/syncer if FalkorDB settings changed.
	if settings.FalkorDBAddr != "" {
		if manager := graph.GetManager(); manager != nil {
			manager.ReloadClient(settings.FalkorDBAddr, settings.FalkorDBPassword, settings.FalkorDBGraphName)
		} else {
			graph.SetDefaultClient(graph.NewClient(settings.FalkorDBAddr, settings.FalkorDBPassword, settings.FalkorDBGraphName))
		}
	}

	response.WriteHeader(http.StatusOK)
}

func (apiHandler *APIHandler) handleVerifyAI(request *restful.Request, response *restful.Response) {
	verifyReq := new(ai.VerifyRequest)
	if err := request.ReadEntity(verifyReq); err != nil {
		errors.HandleInternalError(response, err)
		return
	}
	result := ai.VerifyConnection(request.Request.Context(), verifyReq)
	_ = response.WriteHeaderAndEntity(http.StatusOK, result)
}

// ─── AI Analyst Handler ───────────────────────────────────────────────────────

func (apiHandler *APIHandler) handleAnalyst(request *restful.Request, response *restful.Response) {
	k8sClient, err := getK8sClient(request.Request)
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}

	req := new(ai.AnalystRequest)
	if err := request.ReadEntity(req); err != nil {
		errors.HandleInternalError(response, err)
		return
	}

	settings := ai.LoadAISettings(k8sClient)
	result, err := ai.RunAnalysis(request.Request.Context(), k8sClient, settings, req, newGraphContextProvider(request))
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}
	_ = response.WriteHeaderAndEntity(http.StatusOK, result)
}

// ─── AI Improvement Handler ───────────────────────────────────────────────────

func (apiHandler *APIHandler) handleImprovement(request *restful.Request, response *restful.Response) {
	k8sClient, err := getK8sClient(request.Request)
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}

	namespace := request.PathParameter("namespace")

	settings := ai.LoadAISettings(k8sClient)
	result, err := ai.RunImprovementAudit(request.Request.Context(), k8sClient, settings, namespace)
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}
	_ = response.WriteHeaderAndEntity(http.StatusOK, result)
}

// ─── AI Security Handler ──────────────────────────────────────────────────────

func (apiHandler *APIHandler) handleSecurity(request *restful.Request, response *restful.Response) {
	k8sClient, err := getK8sClient(request.Request)
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}

	namespace := request.PathParameter("namespace")

	settings := ai.LoadAISettings(k8sClient)
	result, err := ai.RunSecurityAudit(request.Request.Context(), k8sClient, settings, namespace)
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}
	enrichSecurityAttackPaths(request, namespace, result)
	_ = response.WriteHeaderAndEntity(http.StatusOK, result)
}

// ─── SRE Agent Handler ────────────────────────────────────────────────────────

func (apiHandler *APIHandler) handleAgent(request *restful.Request, response *restful.Response) {
	k8sClient, err := getK8sClient(request.Request)
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}

	req := new(ai.AgentRequest)
	if err := request.ReadEntity(req); err != nil {
		errors.HandleInternalError(response, err)
		return
	}

	settings := ai.LoadAISettings(k8sClient)
	result, err := ai.RunAgentQuery(request.Request.Context(), k8sClient, settings, req, newGraphContextProvider(request))
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}
	_ = response.WriteHeaderAndEntity(http.StatusOK, result)
}

func (apiHandler *APIHandler) handleAgentStream(request *restful.Request, response *restful.Response) {
	k8sClient, err := getK8sClient(request.Request)
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}

	req := new(ai.AgentRequest)
	if err := request.ReadEntity(req); err != nil {
		errors.HandleInternalError(response, err)
		return
	}

	w := response.ResponseWriter
	flusher, ok := w.(http.Flusher)
	if !ok {
		apiHandler.handleAgent(request, response)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sendSSE := func(event string, data interface{}) {
		b, err := json.Marshal(data)
		if err == nil {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(b))
			flusher.Flush()
		}
	}

	settings := ai.LoadAISettings(k8sClient)
	result, err := ai.RunAgentQueryWithStream(
		request.Request.Context(),
		k8sClient,
		settings,
		req,
		newGraphContextProvider(request),
		func(step ai.AgentStep) {
			sendSSE("step", step)
		},
		func(chunk string) {
			sendSSE("token", chunk)
		},
	)

	if err != nil {
		sendSSE("error", err.Error())
		return
	}

	sendSSE("done", result)
}

func (apiHandler *APIHandler) handleAIResources(request *restful.Request, response *restful.Response) {
	k8sClient, err := getK8sClient(request.Request)
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}

	namespace := request.PathParameter("namespace")
	res := ai.GetResourcesForAutocomplete(request.Request.Context(), k8sClient, namespace)
	_ = response.WriteHeaderAndEntity(http.StatusOK, res)
}

// ─── Graph Handlers ───────────────────────────────────────────────────────────

func (apiHandler *APIHandler) handleGraphStatus(request *restful.Request, response *restful.Response) {
	gc := getGraphClient(request)
	if gc == nil {
		_ = response.WriteHeaderAndEntity(http.StatusOK, &graph.FalkorDBStatus{
			Connected: false,
			Error:     "FalkorDB not configured. Please set FalkorDB address in AI Settings.",
		})
		return
	}
	status := gc.Status(request.Request.Context())
	_ = response.WriteHeaderAndEntity(http.StatusOK, status)
}

func (apiHandler *APIHandler) handleGraphQuery(request *restful.Request, response *restful.Response) {
	gc := getGraphClient(request)
	if gc == nil {
		errors.HandleInternalError(response, errGraphNotConfigured())
		return
	}

	var body struct {
		Cypher string `json:"cypher"`
	}
	if err := request.ReadEntity(&body); err != nil {
		errors.HandleInternalError(response, err)
		return
	}
	if err := validateReadOnlyCypher(body.Cypher); err != nil {
		errors.HandleInternalError(response, err)
		return
	}

	tabular, err := gc.QueryTabular(request.Request.Context(), body.Cypher)
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}
	_ = response.WriteHeaderAndEntity(http.StatusOK, tabular)
}

func (apiHandler *APIHandler) handleBlastRadius(request *restful.Request, response *restful.Response) {
	gc := getGraphClient(request)
	if gc == nil {
		errors.HandleInternalError(response, errGraphNotConfigured())
		return
	}

	kind := request.PathParameter("kind")
	namespace := request.PathParameter("namespace")
	name := request.PathParameter("name")

	result, err := gc.BlastRadius(request.Request.Context(), kind, name, namespace)
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}
	_ = response.WriteHeaderAndEntity(http.StatusOK, result)
}

func (apiHandler *APIHandler) handleGraphTopology(request *restful.Request, response *restful.Response) {
	gc := getGraphClient(request)
	if gc == nil {
		errors.HandleInternalError(response, errGraphNotConfigured())
		return
	}

	result, err := gc.Topology(request.Request.Context())
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}
	_ = response.WriteHeaderAndEntity(http.StatusOK, result)
}

func (apiHandler *APIHandler) handleGraphExplore(request *restful.Request, response *restful.Response) {
	gc := getGraphClient(request)
	if gc == nil {
		errors.HandleInternalError(response, errGraphNotConfigured())
		return
	}

	kind := request.PathParameter("kind")
	namespace := request.PathParameter("namespace")
	name := request.PathParameter("name")

	result, err := gc.Explore(request.Request.Context(), kind, name, namespace)
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}
	_ = response.WriteHeaderAndEntity(http.StatusOK, result)
}

func (apiHandler *APIHandler) handleAttackPaths(request *restful.Request, response *restful.Response) {
	gc := getGraphClient(request)
	if gc == nil {
		errors.HandleInternalError(response, errGraphNotConfigured())
		return
	}

	namespace := request.PathParameter("namespace")
	result, err := gc.AttackPaths(request.Request.Context(), namespace)
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}
	_ = response.WriteHeaderAndEntity(http.StatusOK, result)
}

func (apiHandler *APIHandler) handleGraphSync(request *restful.Request, response *restful.Response) {
	manager := graph.GetManager()
	if manager == nil {
		errors.HandleInternalError(response, errGraphNotConfigured())
		return
	}
	if err := manager.SyncNow(request.Request.Context()); err != nil {
		errors.HandleInternalError(response, err)
		return
	}
	response.WriteHeader(http.StatusOK)
}

func (apiHandler *APIHandler) handleGraphCatalog(request *restful.Request, response *restful.Response) {
	gc := getGraphClient(request)
	if gc == nil {
		errors.HandleInternalError(response, errGraphNotConfigured())
		return
	}
	_ = response.WriteHeaderAndEntity(http.StatusOK, gc.QueryCatalog())
}

func (apiHandler *APIHandler) handleNLQuery(request *restful.Request, response *restful.Response) {
	gc := getGraphClient(request)
	if gc == nil {
		errors.HandleInternalError(response, errGraphNotConfigured())
		return
	}

	body := new(nlQueryRequest)
	if err := request.ReadEntity(body); err != nil {
		errors.HandleInternalError(response, err)
		return
	}

	k8sClient, err := getK8sClient(request.Request)
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}
	q := strings.TrimSpace(body.Question)
	upper := strings.ToUpper(q)
	var cypher string

	if strings.HasPrefix(upper, "MATCH") || strings.HasPrefix(upper, "RETURN") || strings.HasPrefix(upper, "CALL") {
		// User provided a direct Cypher query
		cypher = cleanCypher(q)
	} else {
		settings := ai.LoadAISettings(k8sClient)
		adapter, err := ai.NewAdapter(settings)
		if err != nil {
			errors.HandleInternalError(response, err)
			return
		}

		rawQuery, err := adapter.Chat(request.Request.Context(), cypherSchemaPrompt, q)
		if err != nil {
			errors.HandleInternalError(response, err)
			return
		}
		cypher = cleanCypher(rawQuery)
	}

	if err := validateReadOnlyCypher(cypher); err != nil {
		errors.HandleInternalError(response, err)
		return
	}

	tabular, err := gc.QueryTabular(request.Request.Context(), cypher)
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}
	_ = response.WriteHeaderAndEntity(http.StatusOK, nlQueryResponse{
		Cypher:  cypher,
		Columns: tabular.Columns,
		Rows:    tabular.Rows,
	})
}

func (apiHandler *APIHandler) handleListDashboards(request *restful.Request, response *restful.Response) {
	gc := getGraphClient(request)
	if gc == nil {
		errors.HandleInternalError(response, errGraphNotConfigured())
		return
	}
	dashboards, err := gc.ListDashboards(request.Request.Context())
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}
	_ = response.WriteHeaderAndEntity(http.StatusOK, dashboards)
}

func (apiHandler *APIHandler) handleSaveDashboard(request *restful.Request, response *restful.Response) {
	gc := getGraphClient(request)
	if gc == nil {
		errors.HandleInternalError(response, errGraphNotConfigured())
		return
	}
	dashboard := new(graph.Dashboard)
	if err := request.ReadEntity(dashboard); err != nil {
		errors.HandleInternalError(response, err)
		return
	}
	if err := gc.SaveDashboard(request.Request.Context(), *dashboard); err != nil {
		errors.HandleInternalError(response, err)
		return
	}
	response.WriteHeader(http.StatusOK)
}

func (apiHandler *APIHandler) handleDeleteDashboard(request *restful.Request, response *restful.Response) {
	gc := getGraphClient(request)
	if gc == nil {
		errors.HandleInternalError(response, errGraphNotConfigured())
		return
	}
	name := request.PathParameter("name")
	if err := gc.DeleteDashboard(request.Request.Context(), name); err != nil {
		errors.HandleInternalError(response, err)
		return
	}
	response.WriteHeader(http.StatusOK)
}

func (apiHandler *APIHandler) handleDiagnoseService(request *restful.Request, response *restful.Response) {
	gc := getGraphClient(request)
	if gc == nil {
		errors.HandleInternalError(response, errGraphNotConfigured())
		return
	}
	namespace := request.PathParameter("namespace")
	name := request.PathParameter("name")
	result, err := gc.DiagnoseService(request.Request.Context(), namespace, name)
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}
	_ = response.WriteHeaderAndEntity(http.StatusOK, result)
}

// ─── Traffic Handler ──────────────────────────────────────────────────────────

func (apiHandler *APIHandler) handleTraffic(request *restful.Request, response *restful.Response) {
	k8sClient, err := getK8sClient(request.Request)
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}

	namespace := request.PathParameter("namespace")
	topology, err := traffic.GetTopology(request.Request.Context(), k8sClient, namespace)
	if err != nil {
		errors.HandleInternalError(response, err)
		return
	}
	_ = response.WriteHeaderAndEntity(http.StatusOK, topology)
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func getGraphClient(request *restful.Request) *graph.Client {
	if gc := graph.DefaultClient(); gc != nil {
		return gc
	}
	k8sClient, err := getK8sClient(request.Request)
	if err != nil {
		return nil
	}
	settings := ai.LoadAISettings(k8sClient)
	if settings.FalkorDBAddr == "" {
		return nil
	}
	gc := graph.NewClient(settings.FalkorDBAddr, settings.FalkorDBPassword, settings.FalkorDBGraphName)
	graph.SetDefaultClient(gc)
	klog.InfoS("FalkorDB client initialised", "addr", settings.FalkorDBAddr, "graph", settings.FalkorDBGraphName)
	return gc
}

type graphContextProvider struct {
	client *graph.Client
}

func (p *graphContextProvider) ContextForResource(ctx context.Context, kind, name, namespace string) (string, error) {
	result, err := p.client.Explore(ctx, kind, name, namespace)
	if err != nil {
		return "", err
	}
	return result.Describe(), nil
}

func (p *graphContextProvider) BlastRadiusSummary(ctx context.Context, kind, name, namespace string) (string, error) {
	result, err := p.client.BlastRadius(ctx, kind, name, namespace)
	if err != nil {
		return "", err
	}
	return result.Describe(), nil
}

func (p *graphContextProvider) DiagnoseService(ctx context.Context, namespace, name string) (string, error) {
	result, err := p.client.DiagnoseService(ctx, namespace, name)
	if err != nil {
		return "", err
	}
	return result.Describe(), nil
}

func (p *graphContextProvider) AttackPathsSummary(ctx context.Context, namespace string) (string, error) {
	paths, err := p.client.AttackPaths(ctx, namespace)
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "No critical RBAC attack paths detected in namespace.", nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d sensitive RBAC attack paths in FalkorDB:\n", len(paths)))
	for _, path := range paths {
		sb.WriteString(fmt.Sprintf("- ServiceAccount '%s' -> %s '%s' -> %s '%s' [%s severity]\n",
			path.ServiceAccount, path.BindingKind, path.Binding, path.RoleKind, path.Role, path.Severity))
	}
	return sb.String(), nil
}

func newGraphContextProvider(request *restful.Request) ai.GraphContextProvider {
	gc := getGraphClient(request)
	if gc == nil {
		return nil
	}
	return &graphContextProvider{client: gc}
}

func enrichSecurityAttackPaths(request *restful.Request, namespace string, result *ai.SecurityResponse) {
	gc := getGraphClient(request)
	if gc == nil {
		return
	}
	paths, err := gc.AttackPaths(request.Request.Context(), namespace)
	if err != nil {
		return
	}
	seen := make(map[string]bool)
	for _, p := range paths {
		if p.ServiceAccount == "" || p.Role == "" {
			continue
		}
		ns := p.Namespace
		if ns == "" {
			ns = "cluster"
		}
		key := fmt.Sprintf("%s:%s:%s", p.ServiceAccount, p.Binding, p.Role)
		if seen[key] {
			continue
		}
		seen[key] = true

		severity := strings.ToLower(p.Severity)
		if severity == "" {
			severity = "high"
		}

		result.AttackPaths = append(result.AttackPaths, fmt.Sprintf(
			"%s (%s) → %s → %s [%s]", p.ServiceAccount, ns, p.Binding, p.Role, strings.ToUpper(severity)))

		// Also merge into unified findings list
		kind := p.BindingKind
		if kind == "" {
			kind = "ClusterRoleBinding"
		}
		result.Findings = append(result.Findings, ai.SecurityFinding{
			Kind:      "ServiceAccount",
			Name:      p.ServiceAccount,
			Namespace: p.Namespace,
			Category:  "privilege-escalation",
			Severity:  severity,
			Issue:     fmt.Sprintf("ServiceAccount '%s' grants administrative '%s' access via '%s'.", p.ServiceAccount, p.Role, p.Binding),
			Details:   fmt.Sprintf("RBAC privilege-escalation pathway: ServiceAccount '%s' in namespace '%s' is bound to '%s' via %s '%s'.", p.ServiceAccount, ns, p.Role, kind, p.Binding),
			YAMLPatch: fmt.Sprintf("kubectl delete %s %s", strings.ToLower(kind), p.Binding),
		})
	}
}

// cleanCypher strips markdown fences and whitespace from a model-generated query.
func cleanCypher(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```cypher")
	s = strings.TrimPrefix(s, "```sql")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// validateReadOnlyCypher rejects queries containing mutating Cypher clauses.
func validateReadOnlyCypher(cypher string) error {
	tokens := strings.FieldsFunc(strings.ToUpper(cypher), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, token := range tokens {
		switch token {
		case "CREATE", "DELETE", "MERGE", "SET", "DROP", "REMOVE":
			return fmt.Errorf("read-only query rejected: '%s' clause is not allowed", token)
		}
	}
	return nil
}

func isAPIKeyMasked(key string) bool {
	for _, c := range key {
		if c == '•' || c == '*' {
			return true
		}
	}
	return false
}

func errGraphNotConfigured() error {
	return &graphNotConfiguredError{}
}

type graphNotConfiguredError struct{}

func (e *graphNotConfiguredError) Error() string {
	return "FalkorDB is not configured. Set FalkorDB address in AI Settings."
}
