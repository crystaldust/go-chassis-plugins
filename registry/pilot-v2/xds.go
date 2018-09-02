package pilotv2

import (
	"context"
	"crypto/tls"
	"fmt"

	apiv2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	apiv2core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	"github.com/envoyproxy/go-control-plane/envoy/service/discovery/v2"
	"github.com/gogo/protobuf/proto"
	"google.golang.org/grpc"
)

type XdsClient struct {
	PilotAddr   string
	GrpcConn    *grpc.ClientConn
	ReqCaches   map[XdsType]*XdsReqCache
	nodeInfo    *NodeInfo
	NodeID      string
	NodeCluster string
}

type XdsType string

const (
	TypeCds XdsType = "cds"
	TypeEds XdsType = "eds"
	TypeLds XdsType = "lds"
	TypeRds XdsType = "rds"
)

type XdsReqCache struct {
	Nonce       string
	VersionInfo string
}

type NodeInfo struct {
	PodName    string
	Namespace  string
	InstanceIP string
}

func NewXdsClient(pilotAddr string, tlsConfig *tls.Config, nodeInfo *NodeInfo) (*XdsClient, error) {
	// TODO Handle the array
	xdsClient := &XdsClient{
		PilotAddr: pilotAddr,
		nodeInfo:  nodeInfo,
	}
	xdsClient.NodeID = fmt.Sprintf("sidecar~%s~%s~%s", nodeInfo.InstanceIP, nodeInfo.PodName, nodeInfo.Namespace)
	xdsClient.NodeCluster = fmt.Sprintf("%s.%s", nodeInfo.PodName, nodeInfo.Namespace)

	// TODO Handle the TLS certs
	conn, err := grpc.Dial(xdsClient.PilotAddr, grpc.WithInsecure())
	if err != nil {
		return nil, err
	}

	xdsClient.GrpcConn = conn

	xdsClient.ReqCaches = map[XdsType]*XdsReqCache{
		TypeCds: &XdsReqCache{},
		TypeEds: &XdsReqCache{},
		TypeLds: &XdsReqCache{},
		TypeRds: &XdsReqCache{},
	}

	return xdsClient, nil
}

func getAdsResClient(client *XdsClient) (v2.AggregatedDiscoveryService_StreamAggregatedResourcesClient, error) {
	ctx := context.Background()

	adsClient := v2.NewAggregatedDiscoveryServiceClient(client.GrpcConn)
	adsResClient, err := adsClient.StreamAggregatedResources(ctx)
	if err != nil {
		fmt.Println("failed to get stream adsResClient")
		return nil, err
	}

	return adsResClient, nil
}

func (client *XdsClient) getVersionInfo(resType XdsType) string {
	return client.ReqCaches[resType].VersionInfo
}
func (client *XdsClient) getNonce(resType XdsType) string {
	return client.ReqCaches[resType].Nonce
}

func (client *XdsClient) setVersionInfo(resType XdsType, versionInfo string) {
	client.ReqCaches[resType].VersionInfo = versionInfo
}

func (client *XdsClient) setNonce(resType XdsType, nonce string) {
	client.ReqCaches[resType].Nonce = nonce
}

func (client *XdsClient) CDS() ([]apiv2.Cluster, error) {
	adsResClient, err := getAdsResClient(client)
	if err != nil {
		return nil, err
	}

	req := &apiv2.DiscoveryRequest{
		TypeUrl:       "type.googleapis.com/envoy.api.v2.Cluster",
		VersionInfo:   client.getVersionInfo(TypeCds),
		ResponseNonce: client.getNonce(TypeCds),
	}
	req.Node = &apiv2core.Node{
		// Sample taken from istio: router~172.30.77.6~istio-egressgateway-84b4d947cd-rqt45.istio-system~istio-system.svc.cluster.local-2
		// The Node.Id should be in format {nodeType}~{ipAddr}~{serviceId~{domain}, splitted by '~'
		// The format is required by pilot
		Id:      client.NodeID,
		Cluster: client.NodeCluster,
	}
	if err := adsResClient.Send(req); err != nil {
		return nil, err
	}

	resp, err := adsResClient.Recv()
	if err != nil {
		fmt.Println(err)
		return nil, err
	}

	client.setNonce(TypeCds, resp.GetNonce())
	client.setVersionInfo(TypeCds, resp.GetVersionInfo())
	resources := resp.GetResources()

	var cluster apiv2.Cluster
	clusters := []apiv2.Cluster{}
	for _, res := range resources {
		if err := proto.Unmarshal(res.GetValue(), &cluster); err != nil {
			fmt.Println("Failed to unmarshal resource: ", err)
		} else {
			clusters = append(clusters, cluster)
		}
	}
	return clusters, nil
}

func (client *XdsClient) EDS(clusterName string) ([]apiv2.ClusterLoadAssignment, error) {
	adsResClient, err := getAdsResClient(client)
	if err != nil {
		fmt.Println("failed to get stream adsResClient")
		return nil, err
	}

	req := &apiv2.DiscoveryRequest{
		TypeUrl:       "type.googleapis.com/envoy.api.v2.ClusterLoadAssignment",
		VersionInfo:   client.getVersionInfo(TypeEds),
		ResponseNonce: client.getNonce(TypeEds),
	}

	req.Node = &apiv2core.Node{
		Id:      client.NodeID,
		Cluster: client.NodeCluster,
	}
	req.ResourceNames = []string{clusterName}
	if err := adsResClient.Send(req); err != nil {
		return nil, err
	}

	resp, err := adsResClient.Recv()
	if err != nil {
		return nil, err
	}

	resources := resp.GetResources()
	client.setNonce(TypeEds, resp.GetNonce())
	client.setVersionInfo(TypeEds, resp.GetVersionInfo())

	var endpoint apiv2.ClusterLoadAssignment
	endpoints := []apiv2.ClusterLoadAssignment{}

	for _, res := range resources {
		if err := proto.Unmarshal(res.GetValue(), &endpoint); err != nil {
			fmt.Println("Failed to unmarshal resource: ", err)
		} else {
			endpoints = append(endpoints, endpoint)
		}
	}
	return endpoints, nil
}

func (client *XdsClient) RDS(clusterName string) ([]apiv2.RouteConfiguration, error) {
	adsResClient, err := getAdsResClient(client)
	if err != nil {
		fmt.Println("failed to get stream adsResClient")
		return nil, err
	}

	req := &apiv2.DiscoveryRequest{
		TypeUrl:       "type.googleapis.com/envoy.api.v2.RouteConfiguration",
		VersionInfo:   client.getVersionInfo(TypeRds),
		ResponseNonce: client.getNonce(TypeRds),
	}

	req.Node = &apiv2core.Node{
		Id:      client.NodeID,
		Cluster: client.NodeCluster,
	}
	req.ResourceNames = []string{clusterName}
	if err := adsResClient.Send(req); err != nil {
		return nil, err
	}

	resp, err := adsResClient.Recv()
	if err != nil {
		return nil, err
	}

	resources := resp.GetResources()
	client.setNonce(TypeRds, resp.GetNonce())
	client.setVersionInfo(TypeRds, resp.GetVersionInfo())

	var route apiv2.RouteConfiguration
	routes := []apiv2.RouteConfiguration{}

	for _, res := range resources {
		if err := proto.Unmarshal(res.GetValue(), &route); err != nil {
			fmt.Println("Failed to unmarshal resource: ", err)
		} else {
			routes = append(routes, route)
		}
	}
	return routes, nil
}

func (client *XdsClient) LDS() ([]apiv2.Listener, error) {
	adsResClient, err := getAdsResClient(client)
	if err != nil {
		fmt.Println("failed to get stream adsResClient")
		return nil, err
	}

	req := &apiv2.DiscoveryRequest{
		TypeUrl:       "type.googleapis.com/envoy.api.v2.ClusterLoadAssignment",
		VersionInfo:   client.getVersionInfo(TypeLds),
		ResponseNonce: client.getNonce(TypeLds),
	}

	req.Node = &apiv2core.Node{
		Id:      client.NodeID,
		Cluster: client.NodeCluster,
	}
	if err := adsResClient.Send(req); err != nil {
		return nil, err
	}

	resp, err := adsResClient.Recv()
	if err != nil {
		return nil, err
	}

	resources := resp.GetResources()
	client.setNonce(TypeLds, resp.GetNonce())
	client.setVersionInfo(TypeLds, resp.GetVersionInfo())

	var listener apiv2.Listener
	listeners := []apiv2.Listener{}

	for _, res := range resources {
		if err := proto.Unmarshal(res.GetValue(), &listener); err != nil {
			fmt.Println("Failed to unmarshal resource: ", err)
		} else {
			listeners = append(listeners, listener)
		}
	}
	return listeners, nil
}

// TODO Extract the common part of xds calls
func xds(urlType string) error {

	return nil
}
