package main

import (
	"context"
	"flag"
	"fmt"
	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	tlsinspector "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/tls_inspector/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcpproxy "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"github.com/golang/protobuf/ptypes"
	"github.com/yookoala/realpath"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"path"
	"sync"
	"time"

	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"

	router "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
)

var (
	debug       bool
	onlyLogging bool
	withALS     bool

	localhost = "127.0.0.1"

	port        uint
	gatewayPort uint
	alsPort     uint

	mode string

	cache cachev3.SnapshotCache
)

type Callbacks struct {
	Signal    chan struct{}
	Debug     bool
	Fetches   int
	Requests  int
	Responses []*discoverygrpc.DiscoveryResponse
	mu        sync.Mutex
}

const (
	XdsCluster        = "xds_cluster"
	Ads               = "ads"
	Xds               = "xds"
	Rest              = "rest"
	exitCodeErr       = 1
	exitCodeInterrupt = 2
)

func init() {
	flag.BoolVar(&debug, "debug", true, "Use debug logging")
	flag.UintVar(&port, "port", 18000, "Management server port")
	flag.UintVar(&gatewayPort, "gateway", 18001, "Management server port for HTTP gateway")
	flag.StringVar(&mode, "ads", Ads, "Management server type (ads only now)")
}

func (c *SyncEnvoyConfigCmd) Run(p *ProgramCtx) error {
	flag.Parse()

	backendsByTrafficType, err := fetchAllBackendMetadata(p.DiscoveryURL)
	if err != nil {
		return err
	}

	certBundle, err := fetchCertficates(p.DiscoveryURL)
	if err != nil {
		return err
	}

	realPath, _ := realpath.Realpath(p.OutputDir)
	certPaths, err := writeCertificates(path.Join(realPath, "certs"), certBundle)
	if err != nil {
		return err
	}
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt)
	defer func() {
		signal.Stop(signalChan)
		cancel()
	}()
	go func() {
		select {
		case <-signalChan: // first signal, cancel context
			cancel()
		case <-ctx.Done():
		}
		<-signalChan // second signal, hard exit
		os.Exit(exitCodeInterrupt)
	}()

	signal := make(chan struct{})
	cb := &Callbacks{
		Signal:   signal,
		Fetches:  0,
		Requests: 0,
	}

	cache := cachev3.NewSnapshotCache(true, cachev3.IDHash{}, nil)
	srv := serverv3.NewServer(ctx, cache, cb)

	// start the xDS server
	go RunManagementServer(ctx, srv, port)
	<-signal
	fmt.Printf("Envoy Connected")

	nodeId := cache.GetStatusKeys()[0]

	var listeners, clusters []types.Resource

	var httpVirtualHosts, httpsVirtualHosts []*route.VirtualHost
	var passthroughFilterChains []*listenerv3.FilterChain

	// Create the HTTPS Transport Socket to describe HTTPS Termination
	// Will get used on Edge, Reencrypt Listeners and Reencrypt clusters (backends)
	commonHttpsTlsContext := &tlsv3.CommonTlsContext{
		TlsCertificates: []*tlsv3.TlsCertificate{
			{
				CertificateChain: &core.DataSource{
					Specifier: &core.DataSource_Filename{
						Filename: certPaths.TLSCertFile,
					},
				},
				PrivateKey: &core.DataSource{
					Specifier: &core.DataSource_Filename{
						Filename: certPaths.TLSKeyFile,
					},
				},
			},
		},
		ValidationContextType: &tlsv3.CommonTlsContext_ValidationContext{
			ValidationContext: &tlsv3.CertificateValidationContext{
				TrustedCa: &core.DataSource{
					Specifier: &core.DataSource_Filename{
						Filename: certPaths.RootCAFile,
					},
				},
			},
		},
	}

	for t, backends := range backendsByTrafficType {
		for _, b := range backends {
			if t == HTTPTraffic {
				httpVirtualHost := &route.VirtualHost{
					Name:    b.Name,
					Domains: []string{b.Name},
					Routes: []*route.Route{
						{
							Match: &route.RouteMatch{
								PathSpecifier: &route.RouteMatch_Prefix{
									Prefix: "/",
								},
							},
							Action: &route.Route_Route{
								Route: &route.RouteAction{
									ClusterSpecifier: &route.RouteAction_Cluster{
										Cluster: b.Name,
									},
								},
							},
						},
					},
				}
				httpVirtualHosts = append(httpVirtualHosts, httpVirtualHost)
			} else if t == EdgeTraffic || t == ReencryptTraffic {
				httpsVirtualHost := &route.VirtualHost{
					Name:    b.Name,
					Domains: []string{b.Name},
					Routes: []*route.Route{
						{
							Match: &route.RouteMatch{
								PathSpecifier: &route.RouteMatch_Prefix{
									Prefix: "/",
								},
							},
							Action: &route.Route_Route{
								Route: &route.RouteAction{
									ClusterSpecifier: &route.RouteAction_Cluster{
										Cluster: b.Name,
									},
								},
							},
						},
					},
				}
				httpsVirtualHosts = append(httpsVirtualHosts, httpsVirtualHost)
			} else if t == PassthroughTraffic {
				tcpProxy := &tcpproxy.TcpProxy{
					StatPrefix: "ingress_http",
					ClusterSpecifier: &tcpproxy.TcpProxy_Cluster{
						Cluster: b.Name,
					},
				}
				tcpProxyPb, err := anypb.New(tcpProxy)
				if err != nil {
					log.Fatal(err)
				}

				passthroughFilterChain := &listenerv3.FilterChain{
					Filters: []*listenerv3.Filter{{
						Name: b.Name,
						ConfigType: &listenerv3.Filter_TypedConfig{
							TypedConfig: tcpProxyPb,
						},
					}},
					FilterChainMatch: &listenerv3.FilterChainMatch{
						ServerNames: []string{b.Name},
					},
				}
				passthroughFilterChains = append(passthroughFilterChains, passthroughFilterChain)
			}

			cluster := &cluster.Cluster{
				Name:                 b.Name,
				ConnectTimeout:       ptypes.DurationProto(2 * time.Second),
				ClusterDiscoveryType: &cluster.Cluster_Type{Type: cluster.Cluster_LOGICAL_DNS},
				DnsLookupFamily:      cluster.Cluster_V4_ONLY,
				LbPolicy:             cluster.Cluster_ROUND_ROBIN,
				LoadAssignment: &endpoint.ClusterLoadAssignment{
					ClusterName: b.Name,
					Endpoints: []*endpoint.LocalityLbEndpoints{{
						LbEndpoints: []*endpoint.LbEndpoint{
							{
								HostIdentifier: &endpoint.LbEndpoint_Endpoint{
									Endpoint: &endpoint.Endpoint{
										Address: &core.Address{
											Address: &core.Address_SocketAddress{
												SocketAddress: &core.SocketAddress{
													Address:  b.ListenAddress,
													Protocol: core.SocketAddress_TCP,
													PortSpecifier: &core.SocketAddress_PortValue{
														PortValue: uint32(b.Port),
													},
												},
											},
										},
									},
								},
							},
						},
					}},
				},
			}
			if t == ReencryptTraffic {
				// Turns on termination for reencrypt clusters (backends) with the same certs used in the frontend
				// termination.
				upstreamTlsContext := &tlsv3.UpstreamTlsContext{
					CommonTlsContext: commonHttpsTlsContext,
				}
				upstreamTlsContextPb, err := anypb.New(upstreamTlsContext)
				if err != nil {
					log.Fatal(err)
				}
				cluster.TransportSocket = &core.TransportSocket{
					Name: wellknown.TransportSocketTLS,
					ConfigType: &core.TransportSocket_TypedConfig{
						TypedConfig: upstreamTlsContextPb,
					},
				}
			}

			clusters = append(clusters, cluster)
		}
	}

	httpRoute := &route.RouteConfiguration{
		Name:         "local_http_route",
		VirtualHosts: httpVirtualHosts,
	}

	httpManager := &hcm.HttpConnectionManager{
		CodecType:  hcm.HttpConnectionManager_AUTO,
		StatPrefix: "ingress_http",
		RouteSpecifier: &hcm.HttpConnectionManager_RouteConfig{
			RouteConfig: httpRoute,
		},
		HttpFilters: []*hcm.HttpFilter{{
			Name: wellknown.Router,
			ConfigType: &hcm.HttpFilter_TypedConfig{
				TypedConfig: messageToAny(&router.Router{}),
			},
		}},
	}
	httpManagerPb, err := anypb.New(httpManager)
	if err != nil {
		log.Fatal(err)
	}

	listenerHttp := listenerv3.Listener{
		Name: "listener_http",
		Address: &core.Address{
			Address: &core.Address_SocketAddress{
				SocketAddress: &core.SocketAddress{
					Protocol: core.SocketAddress_TCP,
					Address:  localhost,
					PortSpecifier: &core.SocketAddress_PortValue{
						PortValue: 8080,
					},
				},
			},
		},
		FilterChains: []*listenerv3.FilterChain{
			{
				Filters: []*listenerv3.Filter{{
					Name: wellknown.HTTPConnectionManager,
					ConfigType: &listenerv3.Filter_TypedConfig{
						TypedConfig: httpManagerPb,
					},
				}},
			},
		},
	}

	httpsRoute := &route.RouteConfiguration{
		Name:         "local_https_route",
		VirtualHosts: httpsVirtualHosts,
	}

	httpsManager := &hcm.HttpConnectionManager{
		CodecType:  hcm.HttpConnectionManager_AUTO,
		StatPrefix: "ingress_http",
		RouteSpecifier: &hcm.HttpConnectionManager_RouteConfig{
			RouteConfig: httpsRoute,
		},
		HttpFilters: []*hcm.HttpFilter{{
			Name: wellknown.Router,
			ConfigType: &hcm.HttpFilter_TypedConfig{
				TypedConfig: messageToAny(&router.Router{}),
			},
		}},
	}
	httpsManagerPb, err := anypb.New(httpsManager)
	if err != nil {
		log.Fatal(err)
	}
	downstreamTlsContext := &tlsv3.DownstreamTlsContext{
		CommonTlsContext: commonHttpsTlsContext,
	}
	downstreamTlsContextPb, err := anypb.New(downstreamTlsContext)
	if err != nil {
		log.Fatal(err)
	}

	// Edge and reencrypt are one their own filter chain inside the 8443 listener
	// Traffic gets routed by matching the hostname under the Virtual Host object (just like our 8080 http listener)
	edgeReencryptFilterChain := &listenerv3.FilterChain{
		Filters: []*listenerv3.Filter{{
			Name: wellknown.HTTPConnectionManager,
			ConfigType: &listenerv3.Filter_TypedConfig{
				TypedConfig: httpsManagerPb,
			},
		}},
		TransportSocket: &core.TransportSocket{
			Name: wellknown.TransportSocketTLS,
			ConfigType: &core.TransportSocket_TypedConfig{
				TypedConfig: downstreamTlsContextPb,
			},
		},
	}
	var httpsFilterChains []*listenerv3.FilterChain
	httpsFilterChains = append(httpsFilterChains, passthroughFilterChains...)
	httpsFilterChains = append(httpsFilterChains, edgeReencryptFilterChain)

	tlsInspector := &tlsinspector.TlsInspector{}
	tlsInspectorPb, err := anypb.New(tlsInspector)
	if err != nil {
		log.Fatal(err)
	}

	listenerHttps := listenerv3.Listener{
		Name: "listener_https",
		Address: &core.Address{
			Address: &core.Address_SocketAddress{
				SocketAddress: &core.SocketAddress{
					Protocol: core.SocketAddress_TCP,
					Address:  localhost,
					PortSpecifier: &core.SocketAddress_PortValue{
						PortValue: 8443,
					},
				},
			},
		},
		FilterChains: httpsFilterChains,
		ListenerFilters: []*listenerv3.ListenerFilter{
			{
				Name: "tls-inspector",
				ConfigType: &listenerv3.ListenerFilter_TypedConfig{
					TypedConfig: tlsInspectorPb,
				},
			},
		},
	}

	listeners = append(listeners, &listenerHttp, &listenerHttps)

	var seededRand *rand.Rand = rand.New(
		rand.NewSource(time.Now().UnixNano()))

	version := seededRand.Int31()
	log.Printf(">>>>>>>>>>>>>>>>>>> creating snapshot Version " + fmt.Sprint(version))

	resources := make(map[string][]types.Resource, 3)

	resources[resource.ClusterType] = clusters
	resources[resource.ListenerType] = listeners
	snap, err := cachev3.NewSnapshot(fmt.Sprint(version), resources)
	if err != nil {
		log.Fatalf("Could not set snapshot %v", err)
	}
	if err := snap.Consistent(); err != nil {
		log.Printf("snapshot inconsistency: %+v\n%+v", snap, err)
		os.Exit(1)
	}
	err = cache.SetSnapshot(ctx, nodeId, snap)
	if err != nil {
		log.Fatalf("Could not set snapshot %v", err)
	}

	for !cb.allResponsesSent() {
		fmt.Printf("Waiting for Envoy to sync...")
		time.Sleep(1 * time.Second)
	}

	return nil
}

func (cb *Callbacks) allResponsesSent() bool {
	clusterResponse := false
	listenerResponse := false
	for _, resp := range cb.Responses {
		if resp.TypeUrl == "type.googleapis.com/envoy.config.cluster.v3.Cluster" {
			clusterResponse = true
		} else if resp.TypeUrl == "type.googleapis.com/envoy.config.listener.v3.Listener" {
			listenerResponse = true
		}
	}
	return clusterResponse && listenerResponse
}

const grpcMaxConcurrentStreams = 1000000

// RunManagementServer starts an xDS server at the given port.
func RunManagementServer(ctx context.Context, server serverv3.Server, port uint) {
	var grpcOptions []grpc.ServerOption
	grpcOptions = append(grpcOptions, grpc.MaxConcurrentStreams(grpcMaxConcurrentStreams))
	grpcServer := grpc.NewServer(grpcOptions...)

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Printf("failed to listen")
		os.Exit(1)
	}

	// register services
	discoverygrpc.RegisterAggregatedDiscoveryServiceServer(grpcServer, server)

	fmt.Printf("Envoy Control Plan Listening on Port %d\n", port)
	fmt.Printf("Waiting for Envoy to connect...\n")
	go func() {
		if err = grpcServer.Serve(lis); err != nil {
			log.Printf("%v", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()

	grpcServer.GracefulStop()
}

func (cb *Callbacks) Report() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	log.Printf("fetches: %d, requests: %d", cb.Fetches, cb.Requests)
}
func (cb *Callbacks) OnStreamOpen(_ context.Context, id int64, typ string) error {
	log.Printf("OnStreamOpen %d open for %s", id, typ)
	return nil
}
func (cb *Callbacks) OnStreamClosed(id int64) {
	log.Printf("OnStreamClosed %d closed", id)
}
func (cb *Callbacks) OnStreamRequest(id int64, r *discoverygrpc.DiscoveryRequest) error {
	log.Printf("OnStreamRequest %v", r.TypeUrl)
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.Requests++
	if cb.Signal != nil {
		close(cb.Signal)
		cb.Signal = nil
	}
	return nil
}
func (cb *Callbacks) OnStreamResponse(ctx context.Context, id int64, req *discoverygrpc.DiscoveryRequest, resp *discoverygrpc.DiscoveryResponse) {
	log.Printf("OnStreamResponse... %d   Request [%v],  Response[%v]", id, req.TypeUrl, resp.TypeUrl)
	cb.Responses = append(cb.Responses, resp)
	cb.Report()
}

func (cb *Callbacks) OnFetchRequest(ctx context.Context, req *discoverygrpc.DiscoveryRequest) error {
	log.Printf("OnFetchRequest... Request [%v]", req.TypeUrl)
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.Fetches++
	if cb.Signal != nil {
		close(cb.Signal)
		cb.Signal = nil
	}
	return nil
}
func (cb *Callbacks) OnFetchResponse(req *discoverygrpc.DiscoveryRequest, resp *discoverygrpc.DiscoveryResponse) {
	log.Printf("OnFetchResponse... Resquest[%v],  Response[%v]", req.TypeUrl, resp.TypeUrl)
}

func (cb *Callbacks) OnDeltaStreamClosed(id int64) {
	log.Printf("OnDeltaStreamClosed... %v", id)
}

func (cb *Callbacks) OnDeltaStreamOpen(ctx context.Context, id int64, typ string) error {
	log.Printf("OnDeltaStreamOpen... %v  of type %s", id, typ)
	return nil
}

func (c *Callbacks) OnStreamDeltaRequest(i int64, request *discoverygrpc.DeltaDiscoveryRequest) error {
	log.Printf("OnStreamDeltaRequest... %v  of type %s", i, request)
	return nil
}

func (c *Callbacks) OnStreamDeltaResponse(i int64, request *discoverygrpc.DeltaDiscoveryRequest, response *discoverygrpc.DeltaDiscoveryResponse) {
	log.Printf("OnStreamDeltaResponse... %v  of type %s", i, request)
}

// taken from https://github.com/istio/istio/blob/master/pilot/pkg/networking/util/util.go
func messageToAnyWithError(msg proto.Message) (*anypb.Any, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
	if err != nil {
		return nil, err
	}
	return &anypb.Any{
		// nolint: staticcheck
		TypeUrl: "type.googleapis.com/" + string(msg.ProtoReflect().Descriptor().FullName()),
		Value:   b,
	}, nil
}

// MessageToAny converts from proto message to proto Any
func messageToAny(msg proto.Message) *anypb.Any {
	out, err := messageToAnyWithError(msg)
	if err != nil {
		log.Printf("error marshaling Any %s: %v", prototext.Format(msg), err)
		return nil
	}
	return out
}
