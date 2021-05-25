// Copyright Istio Authors
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

package xds

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/http/pprof"
	"sort"
	"strings"
	"time"

	adminapi "github.com/envoyproxy/go-control-plane/envoy/admin/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	tls "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes/any"
	"google.golang.org/protobuf/types/known/anypb"

	"istio.io/istio/pilot/pkg/config/kube/crd"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pilot/pkg/serviceregistry"
	"istio.io/istio/pilot/pkg/serviceregistry/aggregate"
	"istio.io/istio/pilot/pkg/serviceregistry/memory"
	v3 "istio.io/istio/pilot/pkg/xds/v3"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/schema/collection"
	istiolog "istio.io/pkg/log"
)

var indexTmpl = template.Must(template.New("index").Parse(`<html>
<head>
<title>Pilot Debug Console</title>
</head>
<style>
#endpoints {
  font-family: "Trebuchet MS", Arial, Helvetica, sans-serif;
  border-collapse: collapse;
}

#endpoints td, #endpoints th {
  border: 1px solid #ddd;
  padding: 8px;
}

#endpoints tr:nth-child(even){background-color: #f2f2f2;}

#endpoints tr:hover {background-color: #ddd;}

#endpoints th {
  padding-top: 12px;
  padding-bottom: 12px;
  text-align: left;
  background-color: black;
  color: white;
}
</style>
<body>
<br/>
<table id="endpoints">
<tr><th>Endpoint</th><th>Description</th></tr>
{{range .}}
	<tr>
	<td><a href='{{.Href}}'>{{.Name}}</a></td><td>{{.Help}}</td>
	</tr>
{{end}}
</table>
<br/>
</body>
</html>
`))

// AdsClient defines the data that is displayed on "/adsz" endpoint.
type AdsClient struct {
	ConnectionID string              `json:"connectionId"`
	ConnectedAt  time.Time           `json:"connectedAt"`
	PeerAddress  string              `json:"address"`
	Watches      map[string][]string `json:"watches,omitempty"`
}

// AdsClients is collection of AdsClient connected to this Istiod.
type AdsClients struct {
	Total     int         `json:"totalClients"`
	Connected []AdsClient `json:"clients,omitempty"`
}

// SyncStatus is the synchronization status between Pilot and a given Envoy
type SyncStatus struct {
	ProxyID       string `json:"proxy,omitempty"`
	ProxyVersion  string `json:"proxy_version,omitempty"`
	IstioVersion  string `json:"istio_version,omitempty"`
	ClusterSent   string `json:"cluster_sent,omitempty"`
	ClusterAcked  string `json:"cluster_acked,omitempty"`
	ListenerSent  string `json:"listener_sent,omitempty"`
	ListenerAcked string `json:"listener_acked,omitempty"`
	RouteSent     string `json:"route_sent,omitempty"`
	RouteAcked    string `json:"route_acked,omitempty"`
	EndpointSent  string `json:"endpoint_sent,omitempty"`
	EndpointAcked string `json:"endpoint_acked,omitempty"`
}

// SyncedVersions shows what resourceVersion of a given resource has been acked by Envoy.
type SyncedVersions struct {
	ProxyID         string `json:"proxy,omitempty"`
	ClusterVersion  string `json:"cluster_acked,omitempty"`
	ListenerVersion string `json:"listener_acked,omitempty"`
	RouteVersion    string `json:"route_acked,omitempty"`
}

// InitDebug initializes the debug handlers and adds a debug in-memory registry.
func (s *DiscoveryServer) InitDebug(mux *http.ServeMux, sctl *aggregate.Controller, enableProfiling bool,
	fetchWebhook func() map[string]string) {
	// For debugging and load testing v2 we add an memory registry.
	s.MemRegistry = memory.NewServiceDiscovery(nil)
	s.MemRegistry.EDSUpdater = s
	s.MemRegistry.ClusterID = "v2-debug"

	sctl.AddRegistry(serviceregistry.Simple{
		ClusterID:        "v2-debug",
		ProviderID:       serviceregistry.Mock,
		ServiceDiscovery: s.MemRegistry,
		Controller:       s.MemRegistry.Controller,
	})
	internalMux := http.NewServeMux()
	s.AddDebugHandlers(mux, internalMux, enableProfiling, fetchWebhook)
	debugGen, ok := (s.Generators[TypeDebug]).(*DebugGen)
	if ok {
		debugGen.DebugMux = internalMux
	}
}

func (s *DiscoveryServer) AddDebugHandlers(mux, internalMux *http.ServeMux, enableProfiling bool, webhook func() map[string]string) {
	// Debug handlers on HTTP ports are added for backward compatibility.
	// They will be exposed on XDS-over-TLS in future releases.
	if !features.EnableDebugOnHTTP {
		return
	}

	if enableProfiling {
		s.addDebugHandler(mux, internalMux, "/debug/pprof/", "Displays pprof index", pprof.Index)
		s.addDebugHandler(mux, internalMux, "/debug/pprof/cmdline", "The command line invocation of the current program", pprof.Cmdline)
		s.addDebugHandler(mux, internalMux, "/debug/pprof/profile", "CPU profile", pprof.Profile)
		s.addDebugHandler(mux, internalMux, "/debug/pprof/symbol", "Symbol looks up the program counters listed in the request", pprof.Symbol)
		s.addDebugHandler(mux, internalMux, "/debug/pprof/trace", "A trace of execution of the current program.", pprof.Trace)
	}

	mux.HandleFunc("/debug", s.Debug)

	if features.EnableUnsafeAdminEndpoints {
		s.addDebugHandler(mux, internalMux, "/debug/force_disconnect", "Disconnects a proxy from this Pilot", s.ForceDisconnect)
	}

	s.addDebugHandler(mux, internalMux, "/debug/edsz", "Status and debug interface for EDS", s.Edsz)
	s.addDebugHandler(mux, internalMux, "/debug/ndsz", "Status and debug interface for NDS", s.Ndsz)
	s.addDebugHandler(mux, internalMux, "/debug/adsz", "Status and debug interface for ADS", s.adsz)
	s.addDebugHandler(mux, internalMux, "/debug/adsz?push=true", "Initiates push of the current state to all connected endpoints", s.adsz)

	s.addDebugHandler(mux, internalMux, "/debug/syncz", "Synchronization status of all Envoys connected to this Pilot instance", s.Syncz)
	s.addDebugHandler(mux, internalMux, "/debug/config_distribution", "Version status of all Envoys connected to this Pilot instance", s.distributedVersions)

	s.addDebugHandler(mux, internalMux, "/debug/registryz", "Debug support for registry", s.registryz)
	s.addDebugHandler(mux, internalMux, "/debug/endpointz", "Debug support for endpoints", s.endpointz)
	s.addDebugHandler(mux, internalMux, "/debug/endpointShardz", "Info about the endpoint shards", s.endpointShardz)
	s.addDebugHandler(mux, internalMux, "/debug/cachez", "Info about the internal XDS caches", s.cachez)
	s.addDebugHandler(mux, internalMux, "/debug/configz", "Debug support for config", s.configz)
	s.addDebugHandler(mux, internalMux, "/debug/sidecarz", "Debug sidecar scope for a proxy", s.sidecarz)
	s.addDebugHandler(mux, internalMux, "/debug/resourcesz", "Debug support for watched resources", s.resourcez)
	s.addDebugHandler(mux, internalMux, "/debug/instancesz", "Debug support for service instances", s.instancesz)

	s.addDebugHandler(mux, internalMux, "/debug/authorizationz", "Internal authorization policies", s.Authorizationz)
	s.addDebugHandler(mux, internalMux, "/debug/telemetryz", "Debug Telemetry configuration", s.telemetryz)
	s.addDebugHandler(mux, internalMux, "/debug/config_dump", "ConfigDump in the form of the Envoy admin config dump API for passed in proxyID", s.ConfigDump)
	s.addDebugHandler(mux, internalMux, "/debug/push_status", "Last PushContext Details", s.PushStatusHandler)
	s.addDebugHandler(mux, internalMux, "/debug/pushcontext", "Debug support for current push context", s.PushContextHandler)
	s.addDebugHandler(mux, internalMux, "/debug/connections", "Info about the connected XDS clients", s.ConnectionsHandler)

	s.addDebugHandler(mux, internalMux, "/debug/inject", "Active inject template", s.InjectTemplateHandler(webhook))
	s.addDebugHandler(mux, internalMux, "/debug/mesh", "Active mesh config", s.MeshHandler)
	s.addDebugHandler(mux, internalMux, "/debug/networkz", "List cross-network gateways", s.networkz)

	s.addDebugHandler(mux, internalMux, "/debug/list", "List all supported debug commands in json", s.List)
}

func (s *DiscoveryServer) addDebugHandler(mux *http.ServeMux, internalMux *http.ServeMux,
	path string, help string, handler func(http.ResponseWriter, *http.Request)) {
	s.debugHandlers[path] = help
	// Add handler without auth. This mux is never exposed on an HTTP server and only used internally
	if internalMux != nil {
		internalMux.HandleFunc(path, handler)
	}
	// Add handler with auth; this is expose on an HTTP server
	mux.HandleFunc(path, s.allowAuthenticatedOrLocalhost(http.HandlerFunc(handler)))
}

func (s *DiscoveryServer) allowAuthenticatedOrLocalhost(next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		// Request is from localhost, no need to authenticate
		if isRequestFromLocalhost(req) {
			next.ServeHTTP(w, req)
			return
		}
		// Authenticate request with the same method as XDS
		authFailMsgs := []string{}
		var ids []string
		for _, authn := range s.Authenticators {
			u, err := authn.AuthenticateRequest(req)
			// If one authenticator passes, return
			if u != nil && u.Identities != nil && err == nil {
				ids = u.Identities
				break
			}
			authFailMsgs = append(authFailMsgs, fmt.Sprintf("Authenticator %s: %v", authn.AuthenticatorType(), err))
		}
		if ids == nil {
			istiolog.Errorf("Failed to authenticate %s %v", req.URL, authFailMsgs)
			// Not including detailed info in the response, XDS doesn't either (returns a generic "authentication failure).
			w.WriteHeader(401)
			return
		}
		// TODO: Check that the identity contains istio-system namespace, else block or restrict to only info that
		// is visible to the authenticated SA. Will require changes in docs and istioctl too.
		next.ServeHTTP(w, req)
	}
}

func isRequestFromLocalhost(r *http.Request) bool {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}

	userIP := net.ParseIP(ip)
	return userIP.IsLoopback()
}

// Syncz dumps the synchronization status of all Envoys connected to this Pilot instance
func (s *DiscoveryServer) Syncz(w http.ResponseWriter, _ *http.Request) {
	syncz := make([]SyncStatus, 0)
	for _, con := range s.Clients() {
		node := con.proxy
		if node != nil {
			syncz = append(syncz, SyncStatus{
				ProxyID:       node.ID,
				IstioVersion:  node.Metadata.IstioVersion,
				ClusterSent:   con.NonceSent(v3.ClusterType),
				ClusterAcked:  con.NonceAcked(v3.ClusterType),
				ListenerSent:  con.NonceSent(v3.ListenerType),
				ListenerAcked: con.NonceAcked(v3.ListenerType),
				RouteSent:     con.NonceSent(v3.RouteType),
				RouteAcked:    con.NonceAcked(v3.RouteType),
				EndpointSent:  con.NonceSent(v3.EndpointType),
				EndpointAcked: con.NonceAcked(v3.EndpointType),
			})
		}
	}
	writeJSON(w, syncz)
}

// registryz providees debug support for registry - adding and listing model items.
// Can be combined with the push debug interface to reproduce changes.
func (s *DiscoveryServer) registryz(w http.ResponseWriter, req *http.Request) {
	all, err := s.Env.ServiceDiscovery.Services()
	if err != nil {
		return
	}
	writeJSON(w, all)
}

// Dumps info about the endpoint shards, tracked using the new direct interface.
// Legacy registry provides are synced to the new data structure as well, during
// the full push.
func (s *DiscoveryServer) endpointShardz(w http.ResponseWriter, req *http.Request) {
	w.Header().Add("Content-Type", "application/json")
	s.mutex.RLock()
	out, _ := json.MarshalIndent(s.EndpointShardsByService, " ", " ")
	s.mutex.RUnlock()
	_, _ = w.Write(out)
}

func (s *DiscoveryServer) cachez(w http.ResponseWriter, req *http.Request) {
	keys := s.Cache.Keys()
	sort.Strings(keys)
	writeJSON(w, keys)
}

type endpointzResponse struct {
	Service   string                   `json:"svc"`
	Endpoints []*model.ServiceInstance `json:"ep"`
}

// Endpoint debugging
func (s *DiscoveryServer) endpointz(w http.ResponseWriter, req *http.Request) {
	if _, f := req.URL.Query()["brief"]; f {
		svc, _ := s.Env.ServiceDiscovery.Services()
		for _, ss := range svc {
			for _, p := range ss.Ports {
				all := s.Env.ServiceDiscovery.InstancesByPort(ss, p.Port, nil)
				for _, svc := range all {
					_, _ = fmt.Fprintf(w, "%s:%s %s:%d %v %s\n", ss.Hostname,
						p.Name, svc.Endpoint.Address, svc.Endpoint.EndpointPort, svc.Endpoint.Labels,
						svc.Endpoint.ServiceAccount)
				}
			}
		}
		return
	}

	svc, _ := s.Env.ServiceDiscovery.Services()
	resp := []endpointzResponse{}
	for _, ss := range svc {
		for _, p := range ss.Ports {
			all := s.Env.ServiceDiscovery.InstancesByPort(ss, p.Port, nil)
			resp = append(resp, endpointzResponse{
				Service:   fmt.Sprintf("%s:%s", ss.Hostname, p.Name),
				Endpoints: all,
			})
		}
	}
	writeJSON(w, resp)
}

func (s *DiscoveryServer) distributedVersions(w http.ResponseWriter, req *http.Request) {
	if !features.EnableDistributionTracking {
		w.WriteHeader(http.StatusConflict)
		_, _ = fmt.Fprint(w, "Pilot Version tracking is disabled.  Please set the "+
			"PILOT_ENABLE_CONFIG_DISTRIBUTION_TRACKING environment variable to true to enable.")
		return
	}
	if resourceID := req.URL.Query().Get("resource"); resourceID != "" {
		proxyNamespace := req.URL.Query().Get("proxy_namespace")
		knownVersions := make(map[string]string)
		var results []SyncedVersions
		for _, con := range s.Clients() {
			// wrap this in independent scope so that panic's don't bypass Unlock...
			con.proxy.RLock()

			if con.proxy != nil && (proxyNamespace == "" || proxyNamespace == con.proxy.ConfigNamespace) {
				// read nonces from our statusreporter to allow for skipped nonces, etc.
				results = append(results, SyncedVersions{
					ProxyID: con.proxy.ID,
					ClusterVersion: s.getResourceVersion(s.StatusReporter.QueryLastNonce(con.ConID, v3.ClusterType),
						resourceID, knownVersions),
					ListenerVersion: s.getResourceVersion(s.StatusReporter.QueryLastNonce(con.ConID, v3.ListenerType),
						resourceID, knownVersions),
					RouteVersion: s.getResourceVersion(s.StatusReporter.QueryLastNonce(con.ConID, v3.RouteType),
						resourceID, knownVersions),
				})
			}
			con.proxy.RUnlock()
		}

		writeJSON(w, results)
	} else {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = fmt.Fprintf(w, "querystring parameter 'resource' is required\n")
	}
}

// The Config Version is only used as the nonce prefix, but we can reconstruct it because is is a
// b64 encoding of a 64 bit array, which will always be 12 chars in length.
// len = ceil(bitlength/(2^6))+1
const VersionLen = 12

func (s *DiscoveryServer) getResourceVersion(nonce, key string, cache map[string]string) string {
	if len(nonce) < VersionLen {
		return ""
	}
	configVersion := nonce[:VersionLen]
	result, ok := cache[configVersion]
	if !ok {
		lookupResult, err := s.Env.GetLedger().GetPreviousValue(configVersion, key)
		if err != nil {
			istiolog.Errorf("Unable to retrieve resource %s at version %s: %v", key, configVersion, err)
			lookupResult = ""
		}
		// update the cache even on an error, because errors will not resolve themselves, and we don't want to
		// repeat the same error for many s.adsClients.
		cache[configVersion] = lookupResult
		return lookupResult
	}
	return result
}

// kubernetesConfig wraps a config.Config with a custom marshaling method that matches a Kubernetes
// object structure.
type kubernetesConfig struct {
	config.Config
}

func (k kubernetesConfig) MarshalJSON() ([]byte, error) {
	cfg, err := crd.ConvertConfig(k.Config)
	if err != nil {
		return nil, err
	}
	return json.Marshal(cfg)
}

// Config debugging.
func (s *DiscoveryServer) configz(w http.ResponseWriter, req *http.Request) {
	configs := []kubernetesConfig{}
	s.Env.IstioConfigStore.Schemas().ForEach(func(schema collection.Schema) bool {
		cfg, _ := s.Env.IstioConfigStore.List(schema.Resource().GroupVersionKind(), "")
		for _, c := range cfg {
			configs = append(configs, kubernetesConfig{c})
		}
		return false
	})
	writeJSON(w, configs)
}

// SidecarScope debugging
func (s *DiscoveryServer) sidecarz(w http.ResponseWriter, req *http.Request) {
	con := s.getDebugConnection(w, req)
	if con == nil {
		return
	}
	writeJSON(w, con.proxy.SidecarScope)
}

// Resource debugging.
func (s *DiscoveryServer) resourcez(w http.ResponseWriter, _ *http.Request) {
	schemas := []config.GroupVersionKind{}
	s.Env.Schemas().ForEach(func(schema collection.Schema) bool {
		schemas = append(schemas, schema.Resource().GroupVersionKind())
		return false
	})

	writeJSON(w, schemas)
}

// AuthorizationDebug holds debug information for authorization policy.
type AuthorizationDebug struct {
	AuthorizationPolicies *model.AuthorizationPolicies `json:"authorization_policies"`
}

// Authorizationz dumps the internal authorization policies.
func (s *DiscoveryServer) Authorizationz(w http.ResponseWriter, req *http.Request) {
	info := AuthorizationDebug{
		AuthorizationPolicies: s.globalPushContext().AuthzPolicies,
	}
	writeJSON(w, info)
}

func (s *DiscoveryServer) telemetryz(w http.ResponseWriter, req *http.Request) {
	writeJSON(w, s.globalPushContext().Telemetry)
}

// ConnectionsHandler implements interface for displaying current connections.
// It is mapped to /debug/connections.
func (s *DiscoveryServer) ConnectionsHandler(w http.ResponseWriter, req *http.Request) {
	adsClients := &AdsClients{}
	connections := s.Clients()
	adsClients.Total = len(connections)

	for _, c := range connections {
		adsClient := AdsClient{
			ConnectionID: c.ConID,
			ConnectedAt:  c.Connect,
			PeerAddress:  c.PeerAddr,
		}
		adsClients.Connected = append(adsClients.Connected, adsClient)
	}
	writeJSON(w, adsClients)
}

// adsz implements a status and debug interface for ADS.
// It is mapped to /debug/adsz
func (s *DiscoveryServer) adsz(w http.ResponseWriter, req *http.Request) {
	if s.handlePushRequest(w, req) {
		return
	}

	adsClients := &AdsClients{}
	connections := s.Clients()
	adsClients.Total = len(connections)
	for _, c := range s.Clients() {
		adsClient := AdsClient{
			ConnectionID: c.ConID,
			ConnectedAt:  c.Connect,
			PeerAddress:  c.PeerAddr,
			Watches:      map[string][]string{},
		}
		c.proxy.RLock()
		for k, wr := range c.proxy.WatchedResources {
			r := wr.ResourceNames
			if r == nil {
				r = []string{}
			}
			adsClient.Watches[k] = r
		}
		c.proxy.RUnlock()
		adsClients.Connected = append(adsClients.Connected, adsClient)
	}
	writeJSON(w, adsClients)
}

// ConfigDump returns information in the form of the Envoy admin API config dump for the specified proxy
// The dump will only contain dynamic listeners/clusters/routes and can be used to compare what an Envoy instance
// should look like according to Pilot vs what it currently does look like.
func (s *DiscoveryServer) ConfigDump(w http.ResponseWriter, req *http.Request) {
	con := s.getDebugConnection(w, req)
	if con == nil {
		return
	}
	dump, err := s.configDump(con)
	if err != nil {
		handleHTTPError(w, err)
		return
	}
	writeJSONProto(w, dump)
}

// configDump converts the connection internal state into an Envoy Admin API config dump proto
// It is used in debugging to create a consistent object for comparison between Envoy and Pilot outputs
func (s *DiscoveryServer) configDump(conn *Connection) (*adminapi.ConfigDump, error) {
	dynamicActiveClusters := make([]*adminapi.ClustersConfigDump_DynamicCluster, 0)
	clusters := s.ConfigGenerator.BuildClusters(conn.proxy, s.globalPushContext())

	for _, cs := range clusters {
		cluster, err := anypb.New(cs)
		if err != nil {
			return nil, err
		}
		dynamicActiveClusters = append(dynamicActiveClusters, &adminapi.ClustersConfigDump_DynamicCluster{Cluster: cluster})
	}
	clustersAny, err := util.MessageToAnyWithError(&adminapi.ClustersConfigDump{
		VersionInfo:           versionInfo(),
		DynamicActiveClusters: dynamicActiveClusters,
	})
	if err != nil {
		return nil, err
	}

	dynamicActiveListeners := make([]*adminapi.ListenersConfigDump_DynamicListener, 0)
	listeners := s.ConfigGenerator.BuildListeners(conn.proxy, s.globalPushContext())
	for _, cs := range listeners {
		listener, err := anypb.New(cs)
		if err != nil {
			return nil, err
		}
		dynamicActiveListeners = append(dynamicActiveListeners, &adminapi.ListenersConfigDump_DynamicListener{
			Name:        cs.Name,
			ActiveState: &adminapi.ListenersConfigDump_DynamicListenerState{Listener: listener},
		})
	}
	listenersAny, err := util.MessageToAnyWithError(&adminapi.ListenersConfigDump{
		VersionInfo:      versionInfo(),
		DynamicListeners: dynamicActiveListeners,
	})
	if err != nil {
		return nil, err
	}

	routes := s.ConfigGenerator.BuildHTTPRoutes(conn.proxy, s.globalPushContext(), conn.Routes())
	routeConfigAny := util.MessageToAny(&adminapi.RoutesConfigDump{})
	if len(routes) > 0 {
		dynamicRouteConfig := make([]*adminapi.RoutesConfigDump_DynamicRouteConfig, 0)
		for _, rs := range routes {
			route, err := anypb.New(rs)
			if err != nil {
				return nil, err
			}
			dynamicRouteConfig = append(dynamicRouteConfig, &adminapi.RoutesConfigDump_DynamicRouteConfig{RouteConfig: route})
		}
		routeConfigAny, err = util.MessageToAnyWithError(&adminapi.RoutesConfigDump{DynamicRouteConfigs: dynamicRouteConfig})
		if err != nil {
			return nil, err
		}
	}

	secretsDump := &adminapi.SecretsConfigDump{}
	if s.Generators[v3.SecretType] != nil {
		secrets, _, _ := s.Generators[v3.SecretType].Generate(conn.proxy, s.globalPushContext(), conn.Watched(v3.SecretType), nil)
		if len(secrets) > 0 {
			for _, secretAny := range secrets {
				secret := &tls.Secret{}
				if err := secretAny.GetResource().UnmarshalTo(secret); err != nil {
					istiolog.Warnf("failed to unmarshal secret: %v", err)
				}
				if secret.GetTlsCertificate() != nil {
					secret.GetTlsCertificate().PrivateKey = &core.DataSource{
						Specifier: &core.DataSource_InlineBytes{
							InlineBytes: []byte("[redacted]"),
						},
					}
				}
				secretsDump.DynamicActiveSecrets = append(secretsDump.DynamicActiveSecrets, &adminapi.SecretsConfigDump_DynamicSecret{
					Name:   secret.Name,
					Secret: util.MessageToAny(secret),
				})
			}
		}
	}

	bootstrapAny := util.MessageToAny(&adminapi.BootstrapConfigDump{})
	scopedRoutesAny := util.MessageToAny(&adminapi.ScopedRoutesConfigDump{})
	// The config dump must have all configs with connections specified in
	// https://www.envoyproxy.io/docs/envoy/latest/api-v2/admin/v2alpha/config_dump.proto
	configDump := &adminapi.ConfigDump{
		Configs: []*any.Any{
			bootstrapAny,
			clustersAny, listenersAny,
			scopedRoutesAny,
			routeConfigAny,
			util.MessageToAny(secretsDump),
		},
	}
	return configDump, nil
}

// InjectTemplateHandler dumps the injection template
// Replaces dumping the template at startup.
func (s *DiscoveryServer) InjectTemplateHandler(webhook func() map[string]string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		// TODO: we should split the inject template into smaller modules (separate one for dump core, etc),
		// and allow pods to select which patches will be selected. When this happen, this should return
		// all inject templates or take a param to select one.
		if webhook == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		writeJSON(w, webhook())
	}
}

// MeshHandler dumps the mesh config
func (s *DiscoveryServer) MeshHandler(w http.ResponseWriter, r *http.Request) {
	writeJSONProto(w, s.Env.Mesh())
}

// PushStatusHandler dumps the last PushContext
func (s *DiscoveryServer) PushStatusHandler(w http.ResponseWriter, req *http.Request) {
	if model.LastPushStatus == nil {
		return
	}
	out, err := model.LastPushStatus.StatusJSON()
	if err != nil {
		handleHTTPError(w, err)
		return
	}
	w.Header().Add("Content-Type", "application/json")

	_, _ = w.Write(out)
}

// PushContextDebug holds debug information for push context.
type PushContextDebug struct {
	AuthorizationPolicies *model.AuthorizationPolicies
	NetworkGateways       map[string][]*model.Gateway
}

// PushContextHandler dumps the current PushContext
func (s *DiscoveryServer) PushContextHandler(w http.ResponseWriter, req *http.Request) {
	push := PushContextDebug{
		AuthorizationPolicies: s.globalPushContext().AuthzPolicies,
		NetworkGateways:       s.globalPushContext().NetworkGateways(),
	}

	writeJSON(w, push)
}

// lists all the supported debug endpoints.
func (s *DiscoveryServer) Debug(w http.ResponseWriter, req *http.Request) {
	type debugEndpoint struct {
		Name string
		Href string
		Help string
	}
	var deps []debugEndpoint

	for k, v := range s.debugHandlers {
		deps = append(deps, debugEndpoint{
			Name: k,
			Href: k,
			Help: v,
		})
	}

	sort.Slice(deps, func(i, j int) bool {
		return deps[i].Name < deps[j].Name
	})

	if err := indexTmpl.Execute(w, deps); err != nil {
		istiolog.Errorf("Error in rendering index template %v", err)
		w.WriteHeader(500)
	}
}

// lists all the supported debug commands in json.
func (s *DiscoveryServer) List(w http.ResponseWriter, req *http.Request) {
	var cmdNames []string
	for k := range s.debugHandlers {
		key := strings.Replace(k, "/debug/", "", -1)
		// exclude current list command
		if key == "list" {
			continue
		}
		// can not support pprof commands
		if strings.Contains(key, "pprof") {
			continue
		}
		cmdNames = append(cmdNames, key)
	}
	sort.Strings(cmdNames)
	writeJSON(w, cmdNames)
}

// Ndsz implements a status and debug interface for NDS.
// It is mapped to /debug/Ndsz on the monitor port (15014).
func (s *DiscoveryServer) Ndsz(w http.ResponseWriter, req *http.Request) {
	if s.handlePushRequest(w, req) {
		return
	}

	con := s.getDebugConnection(w, req)
	if con == nil {
		return
	}

	if s.Generators[v3.NameTableType] != nil {
		nds, _, _ := s.Generators[v3.NameTableType].Generate(con.proxy, s.globalPushContext(), nil, nil)
		if len(nds) == 0 {
			return
		}
		writeJSONProto(w, nds[0])
	}
}

// Edsz implements a status and debug interface for EDS.
// It is mapped to /debug/edsz on the monitor port (15014).
func (s *DiscoveryServer) Edsz(w http.ResponseWriter, req *http.Request) {
	if s.handlePushRequest(w, req) {
		return
	}

	con := s.getDebugConnection(w, req)
	if con == nil {
		return
	}

	clusters := con.Clusters()
	eps := make([]jsonMarshalProto, 0, len(clusters))
	for _, clusterName := range clusters {
		eps = append(eps, jsonMarshalProto{s.generateEndpoints(NewEndpointBuilder(clusterName, con.proxy, s.globalPushContext()))})
	}
	writeJSON(w, eps)
}

func (s *DiscoveryServer) ForceDisconnect(w http.ResponseWriter, req *http.Request) {
	con := s.getDebugConnection(w, req)
	if con == nil {
		return
	}
	con.Stop()
	_, _ = w.Write([]byte("OK"))
}

func (s *DiscoveryServer) getProxyConnection(proxyID string) *Connection {
	for _, con := range s.Clients() {
		if strings.Contains(con.ConID, proxyID) {
			return con
		}
	}

	return nil
}

func (s *DiscoveryServer) instancesz(w http.ResponseWriter, req *http.Request) {
	instances := map[string][]*model.ServiceInstance{}
	for _, con := range s.Clients() {
		con.proxy.RLock()
		if con.proxy != nil {
			instances[con.proxy.ID] = con.proxy.ServiceInstances
		}
		con.proxy.RUnlock()
	}
	writeJSON(w, instances)
}

func (s *DiscoveryServer) networkz(w http.ResponseWriter, req *http.Request) {
	writeJSON(w, s.Env.NetworkGateways())
}

// handlePushRequest handles a ?push=true query param and triggers a push.
// A boolean response is returned to indicate if the caller should continue
func (s *DiscoveryServer) handlePushRequest(w http.ResponseWriter, req *http.Request) bool {
	if err := req.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("Failed to parse request\n"))
		return true
	}
	if req.Form.Get("push") != "" {
		AdsPushAll(s)
		_, _ = fmt.Fprintf(w, "Pushed to %d servers\n", s.adsClientCount())
		return true
	}
	return false
}

// getDebugConnection fetches the Connection requested
func (s *DiscoveryServer) getDebugConnection(w http.ResponseWriter, req *http.Request) *Connection {
	var con *Connection
	if err := req.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("Failed to parse request\n"))
		return nil
	}
	if proxyID := req.URL.Query().Get("proxyID"); proxyID != "" {
		con = s.getProxyConnection(proxyID)
		// We can't guarantee the Pilot we are connected to has a connection to the proxy we requested
		// There isn't a great way around this, but for debugging purposes its suitable to have the caller retry.
		if con == nil {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("Proxy not connected to this Pilot instance. It may be connected to another instance.\n"))
			return nil
		}
	} else {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("You must provide a proxyID in the query string\n"))
		return nil
	}
	return con
}

// jsonMarshalProto wraps a proto.Message so it can be marshaled with the standard encoding/json library
type jsonMarshalProto struct {
	proto.Message
}

func (p jsonMarshalProto) MarshalJSON() ([]byte, error) {
	buf := bytes.NewBuffer(nil)
	if err := (&jsonpb.Marshaler{}).Marshal(buf, p.Message); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeJSON writes a json payload, handling content type, marshaling, and errors
func writeJSON(w http.ResponseWriter, obj interface{}) {
	w.Header().Set("Content-Type", "application/json")
	by, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(err.Error()))
		return
	}
	_, err = w.Write(by)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// writeJSONProto writes a protobuf to a json payload, handling content type, marshaling, and errors
func writeJSONProto(w http.ResponseWriter, obj proto.Message) {
	w.Header().Set("Content-Type", "application/json")
	buf := bytes.NewBuffer(nil)
	err := (&jsonpb.Marshaler{Indent: "  "}).Marshal(buf, obj)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(err.Error()))
		return
	}
	_, err = w.Write(buf.Bytes())
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// handleHTTPError writes an error message to the response
func handleHTTPError(w http.ResponseWriter, err error) {
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(err.Error()))
}
