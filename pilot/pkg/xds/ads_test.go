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
package xds_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"google.golang.org/genproto/googleapis/rpc/status"

	mesh "istio.io/api/mesh/v1alpha1"
	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/util/sets"
	"istio.io/istio/pilot/pkg/xds"
	v3 "istio.io/istio/pilot/pkg/xds/v3"
	"istio.io/istio/pilot/test/xdstest"
	"istio.io/istio/pkg/adsc"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/config/schema/collections"
	"istio.io/istio/pkg/config/schema/gvk"
	istioagent "istio.io/istio/pkg/istio-agent"
	"istio.io/istio/pkg/security"
	"istio.io/istio/pkg/spiffe"
	"istio.io/istio/tests/util"
	"istio.io/istio/tests/util/leak"
	"istio.io/pkg/log"
)

const (
	routeA = "http.80"
	routeB = "https.443.https.my-gateway.testns"
)

type clientSecrets struct {
	security.SecretItem
}

func (sc *clientSecrets) GenerateSecret(ctx context.Context, connectionID, resourceName, token string) (*security.SecretItem, error) {
	return &sc.SecretItem, nil
}

// TODO: must fix SDS, it uses existence to detect it's an ACK !!
func (sc *clientSecrets) SecretExist(connectionID, resourceName, token, version string) bool {
	return false
}

// DeleteSecret deletes a secret by its key from cache.
func (sc *clientSecrets) DeleteSecret(connectionID, resourceName string) {
}

// TestAgent will start istiod with TLS enabled, use the istio-agent to connect, and then
// use the ADSC to connect to the agent proxy.
func TestAgent(t *testing.T) {
	// TODO: fix leak and add leak.Check(t)
	// Start Istiod
	bs, tearDown := initLocalPilotTestEnv(t)
	defer tearDown()

	// TODO: when authz is implemented, verify labels are checked.
	cert, key, err := bs.CA.GenKeyCert([]string{spiffe.Identity{"cluster.local", "test", "sa"}.String()}, 1*time.Hour, true)
	if err != nil {
		t.Fatal(err)
	}

	creds := &clientSecrets{
		security.SecretItem{
			PrivateKey:       key,
			CertificateChain: cert,
			RootCert:         bs.CA.GetCAKeyCertBundle().GetRootCertPem(),
		},
	}

	t.Run("agentProxy", func(t *testing.T) {
		// Start the istio-agent (proxy and SDS part) - will connect to XDS
		sa := istioagent.NewAgent(&mesh.ProxyConfig{
			DiscoveryAddress:       util.MockPilotSGrpcAddr,
			ControlPlaneAuthPolicy: mesh.AuthenticationPolicy_MUTUAL_TLS,
		}, &istioagent.AgentConfig{
			// Enable proxy - off by default, will be XDS_LOCAL env in install.
			LocalXDSGeneratorListenAddress: "127.0.0.1:15002",
		}, &security.Options{
			PilotCertProvider: "custom",
			ClusterID:         "kubernetes",
		})

		// Override agent auth - start will use this instead of a gRPC
		// TODO: add a test for cert-based config.
		// TODO: add a test for JWT-based ( using some mock OIDC in Istiod)
		sa.WorkloadSecrets = creds
		sa.RootCert = creds.RootCert
		_, err = sa.Start(true, "test")
		if err != nil {
			t.Fatal(err)
		}

		// connect to the local XDS proxy - it's using a transient port.
		ldsr, err := adsc.New(sa.GetLocalXDSGeneratorListener().Addr().String(),
			&adsc.Config{
				IP:        "10.11.10.1",
				Namespace: "test",
				RootCert:  creds.RootCert,
				InitialDiscoveryRequests: []*discovery.DiscoveryRequest{
					{TypeUrl: v3.ClusterType},
					{TypeUrl: collections.IstioNetworkingV1Alpha3Serviceentries.Resource().GroupVersionKind().String()},
				},
			})
		if err != nil {
			t.Fatal("Failed to connect", err)
		}
		defer ldsr.Close()
		if err := ldsr.Run(); err != nil {
			t.Fatal("ADSC: failed running ", err)
		}

		r, err := ldsr.WaitVersion(5*time.Second, collections.IstioNetworkingV1Alpha3Serviceentries.Resource().GroupVersionKind().String(), "")
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Resources) == 0 {
			t.Fatalf("Got no resources")
		}
	})
	t.Run("adscTLSDirect", func(t *testing.T) {
		testAdscTLS(t, creds)
	})
}

// testAdscTLS tests that ADSC helper can connect using TLS to Istiod
func testAdscTLS(t *testing.T, creds security.SecretManager) {
	// connect to the local XDS proxy - it's using a transient port.
	ldsr, err := adsc.New(util.MockPilotSGrpcAddr,
		&adsc.Config{
			IP:            "10.11.10.1",
			Namespace:     "test",
			SecretManager: creds,
			InitialDiscoveryRequests: []*discovery.DiscoveryRequest{
				{TypeUrl: v3.ClusterType},
				{TypeUrl: xds.TypeURLConnect},
				{TypeUrl: collections.IstioNetworkingV1Alpha3Serviceentries.Resource().GroupVersionKind().String()},
			},
		})
	if err != nil {
		t.Fatal("Failed to connect", err)
	}
	defer ldsr.Close()
}

func TestStatusEvents(t *testing.T) {
	leak.Check(t)
	s := xds.NewFakeDiscoveryServer(t, xds.FakeOptions{})

	ads := s.Connect(
		&model.Proxy{
			Metadata: &model.NodeMetadata{
				Generator: "event",
			},
		},
		[]string{xds.TypeURLConnect},
		[]string{},
	)
	defer ads.Close()

	dr, err := ads.WaitVersion(5*time.Second, xds.TypeURLConnect, "")
	if err != nil {
		t.Fatal(err)
	}

	if dr.Resources == nil || len(dr.Resources) == 0 {
		t.Error("Expected connections, but not found")
	}

	// Create a second connection - we should get an event.
	ads2 := s.Connect(nil, nil, nil)
	defer ads2.Close()

	dr, err = ads.WaitVersion(5*time.Second, xds.TypeURLConnect,
		dr.VersionInfo)
	if err != nil {
		t.Fatal(err)
	}
	if dr.Resources == nil || len(dr.Resources) == 0 {
		t.Error("Expected connections, but not found")
	}
}

func TestAdsReconnectAfterRestart(t *testing.T) {
	s := xds.NewFakeDiscoveryServer(t, xds.FakeOptions{})

	ads := s.ConnectADS().WithType(v3.EndpointType)
	res := ads.RequestResponseAck(&discovery.DiscoveryRequest{ResourceNames: []string{"fake-cluster"}})
	// Close the connection and reconnect
	ads.Cleanup()

	ads = s.ConnectADS().WithType(v3.EndpointType)

	// Reconnect with the same resources
	ads.RequestResponseAck(&discovery.DiscoveryRequest{
		ResourceNames: []string{"fake-cluster"},
		ResponseNonce: res.Nonce,
		VersionInfo:   res.VersionInfo})
}

func TestAdsUnsubscribe(t *testing.T) {
	s := xds.NewFakeDiscoveryServer(t, xds.FakeOptions{})

	ads := s.ConnectADS().WithType(v3.EndpointType)
	res := ads.RequestResponseAck(&discovery.DiscoveryRequest{ResourceNames: []string{"fake-cluster"}})

	ads.Request(&discovery.DiscoveryRequest{
		ResourceNames: nil,
		ResponseNonce: res.Nonce,
		VersionInfo:   res.VersionInfo})
	ads.ExpectNoResponse()
}

// Regression for envoy restart and overlapping connections
func TestAdsReconnect(t *testing.T) {
	leak.Check(t)
	s := xds.NewFakeDiscoveryServer(t, xds.FakeOptions{})
	ads := s.ConnectADS().WithType(v3.ClusterType)
	ads.RequestResponseAck(nil)

	// envoy restarts and reconnects
	ads2 := s.ConnectADS().WithType(v3.ClusterType)
	ads2.RequestResponseAck(nil)

	// closes old process
	ads.Cleanup()

	// event happens, expect push to the remaining connection
	xds.AdsPushAll(s.Discovery)
	ads2.ExpectResponse()
}

func TestAdsClusterUpdate(t *testing.T) {
	s := xds.NewFakeDiscoveryServer(t, xds.FakeOptions{})
	ads := s.ConnectADS().WithType(v3.EndpointType)

	version := ""
	nonce := ""
	var sendEDSReqAndVerify = func(clusterName string) {
		res := ads.RequestResponseAck(&discovery.DiscoveryRequest{
			ResourceNames: []string{clusterName},
			VersionInfo:   version,
			ResponseNonce: nonce,
		})
		version = res.VersionInfo
		nonce = res.Nonce
		got := xdstest.MapKeys(xdstest.ExtractLoadAssignments(xdstest.UnmarshalClusterLoadAssignment(t, res.Resources)))
		if len(got) != 1 {
			t.Fatalf("expected 1 response, got %v", len(got))
		}
		if got[0] != clusterName {
			t.Fatalf("expected cluster %v got %v", clusterName, got[0])
		}
	}

	cluster1 := "outbound|80||local.default.svc.cluster.local"
	sendEDSReqAndVerify(cluster1)
	cluster2 := "outbound|80||hello.default.svc.cluster.local"
	sendEDSReqAndVerify(cluster2)
}

// nolint: lll
func TestAdsPushScoping(t *testing.T) {
	leak.Check(t)
	s := xds.NewFakeDiscoveryServer(t, xds.FakeOptions{})

	const (
		svcSuffix = ".testPushScoping.com"
		ns1       = "ns1"
	)

	removeServiceByNames := func(ns string, names ...string) {
		configsUpdated := map[model.ConfigKey]struct{}{}

		for _, name := range names {
			hostname := host.Name(name)
			s.Discovery.MemRegistry.RemoveService(hostname)
			configsUpdated[model.ConfigKey{
				Kind:      gvk.ServiceEntry,
				Name:      string(hostname),
				Namespace: ns,
			}] = struct{}{}
		}

		s.Discovery.ConfigUpdate(&model.PushRequest{Full: true, ConfigsUpdated: configsUpdated})

	}
	removeService := func(ns string, indexes ...int) {
		var names []string

		for _, i := range indexes {
			names = append(names, fmt.Sprintf("svc%d%s", i, svcSuffix))
		}

		removeServiceByNames(ns, names...)
	}
	addServiceByNames := func(ns string, names ...string) {
		configsUpdated := map[model.ConfigKey]struct{}{}

		for _, name := range names {
			hostname := host.Name(name)
			configsUpdated[model.ConfigKey{
				Kind:      gvk.ServiceEntry,
				Name:      string(hostname),
				Namespace: ns,
			}] = struct{}{}

			s.Discovery.MemRegistry.AddService(hostname, &model.Service{
				Hostname: hostname,
				Address:  "10.11.0.1",
				Ports: []*model.Port{
					{
						Name:     "http-main",
						Port:     2080,
						Protocol: protocol.HTTP,
					},
				},
				Attributes: model.ServiceAttributes{
					Namespace: ns,
				},
			})
		}

		s.Discovery.ConfigUpdate(&model.PushRequest{Full: true, ConfigsUpdated: configsUpdated})
	}
	addService := func(ns string, indexes ...int) {
		var hostnames []string
		for _, i := range indexes {
			hostnames = append(hostnames, fmt.Sprintf("svc%d%s", i, svcSuffix))
		}
		addServiceByNames(ns, hostnames...)
	}

	addServiceInstance := func(hostname host.Name, indexes ...int) {
		for _, i := range indexes {
			s.Discovery.MemRegistry.AddEndpoint(hostname, "http-main", 2080, "192.168.1.10", i)
		}

		s.Discovery.ConfigUpdate(&model.PushRequest{Full: false, ConfigsUpdated: map[model.ConfigKey]struct{}{
			{Kind: gvk.ServiceEntry, Name: string(hostname), Namespace: model.IstioDefaultConfigNamespace}: {},
		}})
	}

	addVirtualService := func(i int, hosts []string, dest string) {
		if _, err := s.Store().Create(config.Config{
			Meta: config.Meta{
				GroupVersionKind: gvk.VirtualService,
				Name:             fmt.Sprintf("vs%d", i), Namespace: model.IstioDefaultConfigNamespace},
			Spec: &networking.VirtualService{
				Hosts: hosts,
				Http: []*networking.HTTPRoute{{
					Name: "dest-foo",
					Route: []*networking.HTTPRouteDestination{{
						Destination: &networking.Destination{
							Host: dest,
						},
					}},
				}},
				ExportTo: nil,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	removeVirtualService := func(i int) {
		s.Store().Delete(gvk.VirtualService, fmt.Sprintf("vs%d", i), model.IstioDefaultConfigNamespace, nil)
	}

	addDelegateVirtualService := func(i int, hosts []string, dest string) {
		if _, err := s.Store().Create(config.Config{
			Meta: config.Meta{
				GroupVersionKind: gvk.VirtualService,
				Name:             fmt.Sprintf("rootvs%d", i), Namespace: model.IstioDefaultConfigNamespace},
			Spec: &networking.VirtualService{
				Hosts: hosts,

				Http: []*networking.HTTPRoute{{
					Name: "dest-foo",
					Delegate: &networking.Delegate{
						Name:      fmt.Sprintf("delegatevs%d", i),
						Namespace: model.IstioDefaultConfigNamespace,
					},
				}},
				ExportTo: nil,
			},
		}); err != nil {
			t.Fatal(err)
		}

		if _, err := s.Store().Create(config.Config{
			Meta: config.Meta{
				GroupVersionKind: gvk.VirtualService,
				Name:             fmt.Sprintf("delegatevs%d", i), Namespace: model.IstioDefaultConfigNamespace},
			Spec: &networking.VirtualService{
				Http: []*networking.HTTPRoute{{
					Name: "dest-foo",
					Route: []*networking.HTTPRouteDestination{{
						Destination: &networking.Destination{
							Host: dest,
						},
					}},
				}},
				ExportTo: nil,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	updateDelegateVirtualService := func(i int, dest string) {
		if _, err := s.Store().Update(config.Config{
			Meta: config.Meta{
				GroupVersionKind: gvk.VirtualService,
				Name:             fmt.Sprintf("delegatevs%d", i), Namespace: model.IstioDefaultConfigNamespace},
			Spec: &networking.VirtualService{
				Http: []*networking.HTTPRoute{{
					Name: "dest-foo",
					Headers: &networking.Headers{
						Request: &networking.Headers_HeaderOperations{
							Remove: []string{"any-string"},
						},
					},
					Route: []*networking.HTTPRouteDestination{
						{
							Destination: &networking.Destination{
								Host: dest,
							},
						},
					},
				}},
				ExportTo: nil,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	removeDelegateVirtualService := func(i int) {
		s.Store().Delete(gvk.VirtualService, fmt.Sprintf("rootvs%d", i), model.IstioDefaultConfigNamespace, nil)
		s.Store().Delete(gvk.VirtualService, fmt.Sprintf("delegatevs%d", i), model.IstioDefaultConfigNamespace, nil)
	}

	addDestinationRule := func(i int, host string) {
		if _, err := s.Store().Create(config.Config{
			Meta: config.Meta{
				GroupVersionKind: gvk.DestinationRule,
				Name:             fmt.Sprintf("dr%d", i), Namespace: model.IstioDefaultConfigNamespace},
			Spec: &networking.DestinationRule{
				Host:     host,
				ExportTo: nil,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	removeDestinationRule := func(i int) {
		s.Store().Delete(gvk.DestinationRule, fmt.Sprintf("dr%d", i), model.IstioDefaultConfigNamespace, nil)
	}

	sc := &networking.Sidecar{
		Egress: []*networking.IstioEgressListener{
			{
				Hosts: []string{model.IstioDefaultConfigNamespace + "/*" + svcSuffix},
			},
		},
	}
	if _, err := s.Store().Create(config.Config{
		Meta: config.Meta{
			GroupVersionKind: gvk.Sidecar,
			Name:             "sc", Namespace: model.IstioDefaultConfigNamespace},
		Spec: sc,
	}); err != nil {
		t.Fatal(err)
	}
	addService(model.IstioDefaultConfigNamespace, 1, 2, 3)

	adscConn := s.Connect(nil, nil, nil)
	defer adscConn.Close()
	type svcCase struct {
		desc string

		ev          model.Event
		svcIndexes  []int
		svcNames    []string
		ns          string
		instIndexes []struct {
			name    string
			indexes []int
		}
		vsIndexes []struct {
			index int
			hosts []string
			dest  string
		}
		delegatevsIndexes []struct {
			index int
			hosts []string
			dest  string
		}
		drIndexes []struct {
			index int
			host  string
		}

		expectUpdates   []string
		unexpectUpdates []string
	}
	svcCases := []svcCase{
		{
			desc:          "Add a scoped service",
			ev:            model.EventAdd,
			svcIndexes:    []int{4},
			ns:            model.IstioDefaultConfigNamespace,
			expectUpdates: []string{v3.ListenerType},
		}, // then: default 1,2,3,4
		{
			desc: "Add instances to a scoped service",
			ev:   model.EventAdd,
			instIndexes: []struct {
				name    string
				indexes []int
			}{{fmt.Sprintf("svc%d%s", 4, svcSuffix), []int{1, 2}}},
			ns:            model.IstioDefaultConfigNamespace,
			expectUpdates: []string{v3.EndpointType},
		}, // then: default 1,2,3,4
		{
			desc: "Add virtual service to a scoped service",
			ev:   model.EventAdd,
			vsIndexes: []struct {
				index int
				hosts []string
				dest  string
			}{{index: 4, hosts: []string{fmt.Sprintf("svc%d%s", 4, svcSuffix)}, dest: "unknown-svc"}},
			expectUpdates: []string{v3.ListenerType},
		},
		{
			desc: "Delete virtual service of a scoped service",
			ev:   model.EventDelete,
			vsIndexes: []struct {
				index int
				hosts []string
				dest  string
			}{{index: 4}},
			expectUpdates: []string{v3.ListenerType},
		},
		{
			desc: "Add destination rule to a scoped service",
			ev:   model.EventAdd,
			drIndexes: []struct {
				index int
				host  string
			}{{4, fmt.Sprintf("svc%d%s", 4, svcSuffix)}},
			expectUpdates: []string{v3.ClusterType},
		},
		{
			desc: "Delete destination rule of a scoped service",
			ev:   model.EventDelete,
			drIndexes: []struct {
				index int
				host  string
			}{{index: 4}},
			expectUpdates: []string{v3.ClusterType},
		},
		{
			desc:            "Add a unscoped(name not match) service",
			ev:              model.EventAdd,
			svcNames:        []string{"foo.com"},
			ns:              model.IstioDefaultConfigNamespace,
			unexpectUpdates: []string{v3.ClusterType},
		}, // then: default 1,2,3,4, foo.com; ns1: 11
		{
			desc: "Add instances to an unscoped service",
			ev:   model.EventAdd,
			instIndexes: []struct {
				name    string
				indexes []int
			}{{"foo.com", []int{1, 2}}},
			ns:              model.IstioDefaultConfigNamespace,
			unexpectUpdates: []string{v3.EndpointType},
		}, // then: default 1,2,3,4
		{
			desc:            "Add a unscoped(ns not match) service",
			ev:              model.EventAdd,
			svcIndexes:      []int{11},
			ns:              ns1,
			unexpectUpdates: []string{v3.ClusterType},
		}, // then: default 1,2,3,4, foo.com; ns1: 11
		{
			desc: "Add virtual service to an unscoped service",
			ev:   model.EventAdd,
			vsIndexes: []struct {
				index int
				hosts []string
				dest  string
			}{{index: 0, hosts: []string{"foo.com"}, dest: "unknown-service"}},
			unexpectUpdates: []string{v3.ClusterType},
		},
		{
			desc: "Delete virtual service of a unscoped service",
			ev:   model.EventDelete,
			vsIndexes: []struct {
				index int
				hosts []string
				dest  string
			}{{index: 0}},
			unexpectUpdates: []string{v3.ClusterType},
		},
		{
			desc: "Add destination rule to an unscoped service",
			ev:   model.EventAdd,
			drIndexes: []struct {
				index int
				host  string
			}{{0, "foo.com"}},
			unexpectUpdates: []string{v3.ClusterType},
		},
		{
			desc: "Delete destination rule of a unscoped service",
			ev:   model.EventDelete,
			drIndexes: []struct {
				index int
				host  string
			}{{index: 0}},
			unexpectUpdates: []string{v3.ClusterType},
		},
		{
			desc: "Add virtual service for scoped service with transitively scoped dest svc",
			ev:   model.EventAdd,
			vsIndexes: []struct {
				index int
				hosts []string
				dest  string
			}{{index: 4, hosts: []string{fmt.Sprintf("svc%d%s", 4, svcSuffix)}, dest: "foo.com"}},
			expectUpdates: []string{v3.ClusterType, v3.EndpointType},
		},
		{
			desc: "Add instances for transitively scoped svc",
			ev:   model.EventAdd,
			instIndexes: []struct {
				name    string
				indexes []int
			}{{"foo.com", []int{1, 2}}},
			ns:            model.IstioDefaultConfigNamespace,
			expectUpdates: []string{v3.EndpointType},
		},
		{
			desc: "Delete virtual service for scoped service with transitively scoped dest svc",
			ev:   model.EventDelete,
			vsIndexes: []struct {
				index int
				hosts []string
				dest  string
			}{{index: 4}},
			expectUpdates: []string{v3.ClusterType},
		},
		{
			desc: "Add delegation virtual service for scoped service with transitively scoped dest svc",
			ev:   model.EventAdd,
			delegatevsIndexes: []struct {
				index int
				hosts []string
				dest  string
			}{{index: 4, hosts: []string{fmt.Sprintf("svc%d%s", 4, svcSuffix)}, dest: "foo.com"}},
			expectUpdates: []string{v3.ListenerType, v3.RouteType, v3.ClusterType, v3.EndpointType},
		},
		{
			desc: "Update delegate virtual service should trigger full push",
			ev:   model.EventUpdate,
			delegatevsIndexes: []struct {
				index int
				hosts []string
				dest  string
			}{{index: 4, hosts: []string{fmt.Sprintf("svc%d%s", 4, svcSuffix)}, dest: "foo.com"}},
			expectUpdates: []string{v3.ListenerType, v3.RouteType, v3.ClusterType},
		},
		{
			desc: "Delete delegate virtual service for scoped service with transitively scoped dest svc",
			ev:   model.EventDelete,
			delegatevsIndexes: []struct {
				index int
				hosts []string
				dest  string
			}{{index: 4}},
			expectUpdates: []string{v3.ListenerType, v3.RouteType, v3.ClusterType},
		},
		{
			desc:          "Remove a scoped service",
			ev:            model.EventDelete,
			svcIndexes:    []int{4},
			ns:            model.IstioDefaultConfigNamespace,
			expectUpdates: []string{v3.ListenerType},
		}, // then: default 1,2,3, foo.com; ns: 11
		{
			desc:            "Remove a unscoped(name not match) service",
			ev:              model.EventDelete,
			svcNames:        []string{"foo.com"},
			ns:              model.IstioDefaultConfigNamespace,
			unexpectUpdates: []string{v3.ClusterType},
		}, // then: default 1,2,3; ns1: 11
		{
			desc:            "Remove a unscoped(ns not match) service",
			ev:              model.EventDelete,
			svcIndexes:      []int{11},
			ns:              ns1,
			unexpectUpdates: []string{v3.ClusterType},
		}, // then: default 1,2,3
	}

	for _, c := range svcCases {
		t.Run(c.desc, func(t *testing.T) {
			// Let events from previous tests complete
			time.Sleep(time.Millisecond * 100)
			adscConn.WaitClear()
			var wantUpdates []string
			wantUpdates = append(wantUpdates, c.expectUpdates...)
			wantUpdates = append(wantUpdates, c.unexpectUpdates...)

			switch c.ev {
			case model.EventAdd:
				if len(c.svcIndexes) > 0 {
					addService(c.ns, c.svcIndexes...)
				}
				if len(c.svcNames) > 0 {
					addServiceByNames(c.ns, c.svcNames...)
				}
				if len(c.instIndexes) > 0 {
					for _, instIndex := range c.instIndexes {
						addServiceInstance(host.Name(instIndex.name), instIndex.indexes...)
					}
				}
				if len(c.vsIndexes) > 0 {
					for _, vsIndex := range c.vsIndexes {
						addVirtualService(vsIndex.index, vsIndex.hosts, vsIndex.dest)
					}
				}
				if len(c.delegatevsIndexes) > 0 {
					for _, vsIndex := range c.delegatevsIndexes {
						addDelegateVirtualService(vsIndex.index, vsIndex.hosts, vsIndex.dest)
					}
				}
				if len(c.drIndexes) > 0 {
					for _, drIndex := range c.drIndexes {
						addDestinationRule(drIndex.index, drIndex.host)
					}
				}
			case model.EventUpdate:
				if len(c.delegatevsIndexes) > 0 {
					for _, vsIndex := range c.delegatevsIndexes {
						updateDelegateVirtualService(vsIndex.index, vsIndex.dest)
					}
				}
			case model.EventDelete:
				if len(c.svcIndexes) > 0 {
					removeService(c.ns, c.svcIndexes...)
				}
				if len(c.svcNames) > 0 {
					removeServiceByNames(c.ns, c.svcNames...)
				}
				if len(c.vsIndexes) > 0 {
					for _, vsIndex := range c.vsIndexes {
						removeVirtualService(vsIndex.index)
					}
				}
				if len(c.delegatevsIndexes) > 0 {
					for _, vsIndex := range c.delegatevsIndexes {
						removeDelegateVirtualService(vsIndex.index)
					}
				}
				if len(c.drIndexes) > 0 {
					for _, drIndex := range c.drIndexes {
						removeDestinationRule(drIndex.index)
					}
				}
			default:
				t.Fatalf("wrong event for case %v", c)
			}

			timeout := time.Second
			upd, _ := adscConn.Wait(timeout, wantUpdates...) // XXX slow for unexpect ...
			for _, expect := range c.expectUpdates {
				if !contains(upd, expect) {
					t.Fatalf("expected update %s not in updates %v", expect, upd)
				}
			}
			for _, unexpect := range c.unexpectUpdates {
				if contains(upd, unexpect) {
					t.Fatalf("expected to not get update %s, but it is in updates %v", unexpect, upd)
				}
			}
		})
	}
}

func TestAdsUpdate(t *testing.T) {
	s := xds.NewFakeDiscoveryServer(t, xds.FakeOptions{})
	ads := s.ConnectADS()

	s.Discovery.MemRegistry.AddService("adsupdate.default.svc.cluster.local", &model.Service{
		Hostname: "adsupdate.default.svc.cluster.local",
		Address:  "10.11.0.1",
		Ports: []*model.Port{
			{
				Name:     "http-main",
				Port:     2080,
				Protocol: protocol.HTTP,
			},
		},
		Attributes: model.ServiceAttributes{
			Name:      "adsupdate",
			Namespace: "default",
		},
	})
	s.Discovery.ConfigUpdate(&model.PushRequest{Full: true})
	time.Sleep(time.Millisecond * 200)
	s.Discovery.MemRegistry.SetEndpoints("adsupdate.default.svc.cluster.local", "default",
		newEndpointWithAccount("10.2.0.1", "hello-sa", "v1"))

	cluster := "outbound|2080||adsupdate.default.svc.cluster.local"
	res := ads.RequestResponseAck(&discovery.DiscoveryRequest{
		ResourceNames: []string{cluster},
		TypeUrl:       v3.EndpointType,
	})
	eps, f := xdstest.ExtractLoadAssignments(xdstest.UnmarshalClusterLoadAssignment(t, res.GetResources()))[cluster]
	if !f {
		t.Fatalf("did not find cluster %v", cluster)
	}
	if !reflect.DeepEqual(eps, []string{"10.2.0.1:80"}) {
		t.Fatalf("expected endpoints [10.2.0.1:80] got %v", eps)
	}

	_ = s.Discovery.MemRegistry.AddEndpoint("adsupdate.default.svc.cluster.local",
		"http-main", 2080, "10.1.7.1", 1080)

	// will trigger recompute and push for all clients - including some that may be closing
	// This reproduced the 'push on closed connection' bug.
	xds.AdsPushAll(s.Discovery)
	res1 := ads.ExpectResponse()
	xdstest.UnmarshalClusterLoadAssignment(t, res1.GetResources())
}

func TestEnvoyRDSProtocolError(t *testing.T) {
	s := xds.NewFakeDiscoveryServer(t, xds.FakeOptions{})
	ads := s.ConnectADS().WithType(v3.RouteType)
	ads.RequestResponseAck(&discovery.DiscoveryRequest{ResourceNames: []string{routeA}})

	xds.AdsPushAll(s.Discovery)
	res := ads.ExpectResponse()

	// send empty response and validate no response is returned.
	ads.Request(&discovery.DiscoveryRequest{
		ResourceNames: nil,
		VersionInfo:   res.VersionInfo,
		ResponseNonce: res.Nonce,
	})
	ads.ExpectNoResponse()

	// Refresh routes
	ads.Request(&discovery.DiscoveryRequest{
		ResourceNames: []string{routeA, routeB},
		VersionInfo:   res.VersionInfo,
		ResponseNonce: res.Nonce,
	})
}

func TestBlockedPush(t *testing.T) {
	original := features.EnableFlowControl
	t.Cleanup(func() {
		features.EnableFlowControl = original
	})
	t.Run("flow control enabled", func(t *testing.T) {
		features.EnableFlowControl = true
		s := xds.NewFakeDiscoveryServer(t, xds.FakeOptions{})
		ads := s.ConnectADS().WithType(v3.ClusterType)
		ads.RequestResponseAck(nil)
		// Send push, get a response but do not ACK it
		xds.AdsPushAll(s.Discovery)
		res := ads.ExpectResponse()

		// Another push results in no response as we are blocked
		xds.AdsPushAll(s.Discovery)
		ads.ExpectNoResponse()

		// ACK, unblocking the previous push
		ads.Request(&discovery.DiscoveryRequest{ResponseNonce: res.Nonce})
		res = ads.ExpectResponse()

		// ACK again, ensure we do not response
		ads.Request(&discovery.DiscoveryRequest{ResponseNonce: res.Nonce})
		ads.ExpectNoResponse()

		// request new resources, expect response
		ads.Request(&discovery.DiscoveryRequest{ResponseNonce: res.Nonce, ResourceNames: []string{"foo"}})
		res = ads.ExpectResponse()
		// request new resources, expect response, even without explicit ACK
		ads.Request(&discovery.DiscoveryRequest{ResponseNonce: res.Nonce, ResourceNames: []string{"foo", "bar"}})
		ads.ExpectResponse()
	})
	t.Run("flow control enabled NACK", func(t *testing.T) {
		log.FindScope("ads").SetOutputLevel(log.DebugLevel)
		features.EnableFlowControl = true
		s := xds.NewFakeDiscoveryServer(t, xds.FakeOptions{})
		ads := s.ConnectADS().WithType(v3.ClusterType)
		ads.RequestResponseAck(nil)

		// Send push, get a response and NACK it
		xds.AdsPushAll(s.Discovery)
		res := ads.ExpectResponse()
		ads.Request(&discovery.DiscoveryRequest{ResponseNonce: res.Nonce, ErrorDetail: &status.Status{Message: "Test request NACK"}})

		// Another push results in a response as we are not blocked (NACK unblocks)
		xds.AdsPushAll(s.Discovery)
		ads.ExpectResponse()

		// ACK should not get push
		ads.Request(&discovery.DiscoveryRequest{ResponseNonce: res.Nonce})
		ads.ExpectNoResponse()
	})
	t.Run("flow control disabled", func(t *testing.T) {
		features.EnableFlowControl = false
		s := xds.NewFakeDiscoveryServer(t, xds.FakeOptions{})
		ads := s.ConnectADS().WithType(v3.ClusterType)
		ads.RequestResponseAck(nil)
		// Send push, get a response but do not ACK it
		xds.AdsPushAll(s.Discovery)
		res := ads.ExpectResponse()

		// Another push results in response as we do not care that we are blocked
		xds.AdsPushAll(s.Discovery)
		ads.ExpectResponse()

		// ACK gets no response as we don't have flow control enabled
		ads.Request(&discovery.DiscoveryRequest{ResponseNonce: res.Nonce})
		ads.ExpectNoResponse()
	})
}

func TestEnvoyRDSUpdatedRouteRequest(t *testing.T) {
	expectRoutes := func(resp *discovery.DiscoveryResponse, expected ...string) {
		t.Helper()
		got := xdstest.MapKeys(xdstest.ExtractRouteConfigurations(xdstest.UnmarshalRouteConfiguration(t, resp.Resources)))
		if !reflect.DeepEqual(expected, got) {
			t.Fatalf("expected routes %v got %v", expected, got)
		}
	}
	s := xds.NewFakeDiscoveryServer(t, xds.FakeOptions{})
	ads := s.ConnectADS().WithType(v3.RouteType)
	resp := ads.RequestResponseAck(&discovery.DiscoveryRequest{ResourceNames: []string{routeA}})
	expectRoutes(resp, routeA)

	xds.AdsPushAll(s.Discovery)
	resp = ads.ExpectResponse()
	expectRoutes(resp, routeA)

	// Test update from A -> B
	resp = ads.RequestResponseAck(&discovery.DiscoveryRequest{ResourceNames: []string{routeB}})
	expectRoutes(resp, routeB)

	// Test update from B -> A, B
	resp = ads.RequestResponseAck(&discovery.DiscoveryRequest{ResourceNames: []string{routeA, routeB}})
	expectRoutes(resp, routeA, routeB)

	// Test update from B, B -> A
	resp = ads.RequestResponseAck(&discovery.DiscoveryRequest{ResourceNames: []string{routeA}})
	expectRoutes(resp, routeA)
}

func TestXdsCache(t *testing.T) {
	makeEndpoint := func(addr []*networking.WorkloadEntry) config.Config {
		return config.Config{
			Meta: config.Meta{
				Name:             "service",
				Namespace:        "default",
				GroupVersionKind: gvk.ServiceEntry,
			},
			Spec: &networking.ServiceEntry{
				Hosts: []string{"foo.com"},
				Ports: []*networking.Port{{
					Number:   80,
					Protocol: "HTTP",
					Name:     "http",
				}},
				Resolution: networking.ServiceEntry_STATIC,
				Endpoints:  addr,
			},
		}
	}
	assertEndpoints := func(a *adsc.ADSC, addr ...string) {
		t.Helper()
		got := sets.NewSet(xdstest.ExtractEndpoints(a.GetEndpoints()["outbound|80||foo.com"])...)
		want := sets.NewSet(addr...)

		if !got.Equals(want) {
			t.Fatalf("invalid endpoints, got %v want %v", got, addr)
		}
	}

	s := xds.NewFakeDiscoveryServer(t, xds.FakeOptions{
		Configs: []config.Config{
			makeEndpoint([]*networking.WorkloadEntry{
				{Address: "1.2.3.4", Locality: "region/zone"},
				{Address: "1.2.3.5", Locality: "notmatch"},
			}),
		},
	})
	ads := s.Connect(&model.Proxy{Locality: &core.Locality{Region: "region"}}, nil, watchAll)

	assertEndpoints(ads, "1.2.3.4:80", "1.2.3.5:80")
	t.Logf("endpoints: %+v", ads.GetEndpoints())

	if _, err := s.Store().Update(makeEndpoint([]*networking.WorkloadEntry{
		{Address: "1.2.3.6", Locality: "region/zone"},
		{Address: "1.2.3.5", Locality: "notmatch"},
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := ads.Wait(time.Second*5, v3.EndpointType); err != nil {
		t.Fatal(err)
	}
	assertEndpoints(ads, "1.2.3.6:80", "1.2.3.5:80")
	t.Logf("endpoints: %+v", ads.GetEndpoints())

	ads.WaitClear()
	if _, err := s.Store().Create(config.Config{
		Meta: config.Meta{
			Name:             "service",
			Namespace:        "default",
			GroupVersionKind: gvk.DestinationRule,
		},
		Spec: &networking.DestinationRule{
			Host: "foo.com",
			TrafficPolicy: &networking.TrafficPolicy{
				OutlierDetection: &networking.OutlierDetection{},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ads.Wait(time.Second*5, v3.EndpointType); err != nil {
		t.Fatal(err)
	}
	assertEndpoints(ads, "1.2.3.6:80", "1.2.3.5:80")
	found := false
	for _, ep := range ads.GetEndpoints()["outbound|80||foo.com"].Endpoints {
		if ep.Priority == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("locality did not update")
	}
	t.Logf("endpoints: %+v", ads.GetEndpoints())
	ads.WaitClear()

	ep := makeEndpoint([]*networking.WorkloadEntry{{Address: "1.2.3.6", Locality: "region/zone"}, {Address: "1.2.3.5", Locality: "notmatch"}})
	ep.Spec.(*networking.ServiceEntry).Resolution = networking.ServiceEntry_DNS
	if _, err := s.Store().Update(ep); err != nil {
		t.Fatal(err)
	}
	if _, err := ads.Wait(time.Second*5, v3.EndpointType); err != nil {
		t.Fatal(err)
	}
	assertEndpoints(ads)
	t.Logf("endpoints: %+v", ads.GetEndpoints())
}
