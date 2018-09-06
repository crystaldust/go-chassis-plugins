package pilotv2

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"strings"

	apiv2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	apiv2core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	apiv2endpoint "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	apiv2route "github.com/envoyproxy/go-control-plane/envoy/api/v2/route"
	"github.com/envoyproxy/go-control-plane/envoy/service/discovery/v2"
	"github.com/gogo/protobuf/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"k8s.io/client-go/rest"
)

type XdsClient struct {
	PilotAddr   string
	GrpcConn    *grpc.ClientConn
	ReqCaches   map[XdsType]*XdsReqCache
	nodeInfo    *NodeInfo
	NodeID      string
	NodeCluster string
	k8sClient   *rest.RESTClient
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

func NewXdsClient(pilotAddr string, tlsConfig *tls.Config, nodeInfo *NodeInfo, kubeconfigPath string) (*XdsClient, error) {
	// TODO Handle the array
	xdsClient := &XdsClient{
		PilotAddr: pilotAddr,
		nodeInfo:  nodeInfo,
	}
	xdsClient.NodeID = fmt.Sprintf("sidecar~%s~%s~%s", nodeInfo.InstanceIP, nodeInfo.PodName, nodeInfo.Namespace)
	xdsClient.NodeCluster = nodeInfo.PodName

	// TODO Handle the TLS certs
	var conn *grpc.ClientConn
	var err error

	if tlsConfig != nil {
		creds := credentials.NewTLS(tlsConfig)
		conn, err = grpc.Dial(xdsClient.PilotAddr, grpc.WithTransportCredentials(creds))
	} else {
		conn, err = grpc.Dial(xdsClient.PilotAddr, grpc.WithInsecure())
	}
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

	if k8sClient, err := CreateK8SRestClient(kubeconfigPath, "apis", "networking.istio.io", "v1alpha3"); err != nil {
		return nil, err
	} else {
		xdsClient.k8sClient = k8sClient
	}

	return xdsClient, nil
}

func (client *XdsClient) GetSubsetTags(namespace, hostName, subsetName string) (map[string]string, error) {
	req := client.k8sClient.Get()
	req.Resource("destinationrules")
	req.Namespace(namespace)

	result := req.Do()
	rawBody, err := result.Raw()
	if err != nil {
		fmt.Println("Failed to get rawBody: ", err)
		return nil, err
	}

	var drResult DestinationRuleResult
	json.Unmarshal(rawBody, &drResult)

	// Find the subset
	tags := map[string]string{}
	for _, dr := range drResult.Items {
		if dr.Spec.Host == hostName {
			for _, subset := range dr.Spec.Subsets {
				if subset.Name == subsetName {
					for k, v := range subset.Labels {
						tags[k] = v
					}
					break
				}
			}
			break
		}
	}

	return tags, nil
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

func (client *XdsClient) getRouterClusters(clusterName string) ([]string, error) {
	virtualHosts, err := client.RDS(clusterName)
	if err != nil {
		return nil, err
	}

	routerClusters := []string{}
	for _, h := range virtualHosts {
		for _, r := range h.Routes {
			routerClusters = append(routerClusters, r.GetRoute().GetCluster())
		}
	}

	return routerClusters, nil
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

func (client *XdsClient) EDS(clusterName string) (*apiv2.ClusterLoadAssignment, error) {
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

	var loadAssignment apiv2.ClusterLoadAssignment
	var e error
	// endpoints := []apiv2.ClusterLoadAssignment{}

	for _, res := range resources {
		if err := proto.Unmarshal(res.GetValue(), &loadAssignment); err != nil {
			e = err
		} else {
			// The cluster's LoadAssignment will always be ONE, with Endpoints as its field
			break
		}
	}
	return &loadAssignment, e
}

func (client *XdsClient) GetEndpointsByTags(serviceName string, tags map[string]string) ([]apiv2endpoint.LbEndpoint, string, error) {
	clusters, err := client.CDS()
	if err != nil {
		return nil, "", err
	}

	lbendpoints := []apiv2endpoint.LbEndpoint{}
	clusterName := ""
	for _, cluster := range clusters {
		clusterInfo := ParseClusterName(cluster.Name)
		if clusterInfo == nil || clusterInfo.Subset == "" || clusterInfo.ServiceName != serviceName {
			continue
		}
		fmt.Println("try ", cluster.Name)
		// So clusterInfo is not nil and subset is not empty
		if subsetTags, err := client.GetSubsetTags(clusterInfo.Namespace, clusterInfo.ServiceName, clusterInfo.Subset); err == nil {
			fmt.Println("subsetTags: ", cluster.Name, subsetTags)
			// filter with tags
			matched := true
			for k, v := range tags {
				if subsetTagValue, exists := subsetTags[k]; exists == false || subsetTagValue != v {
					fmt.Println(k, "not matching")
					matched = false
					break
				}
			}

			if matched { // We got the cluster!
				fmt.Println("matched!", cluster.Name)
				clusterName = cluster.Name
				loadAssignment, err := client.EDS(cluster.Name)
				if err != nil {
					fmt.Println("failed to get load assignment")
					return nil, clusterName, err
				}

				for _, item := range loadAssignment.Endpoints {
					lbendpoints = append(lbendpoints, item.LbEndpoints...)
				}
				fmt.Println("got ", len(lbendpoints), "lbendpoints")

				return lbendpoints, clusterName, nil
			}
		}
	}

	return lbendpoints, clusterName, nil
}

func (client *XdsClient) RDS(clusterName string) ([]apiv2route.VirtualHost, error) {
	parts := strings.Split(clusterName, "|")
	port := parts[1]
	serviceName := parts[3]

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
	virtualHosts := []apiv2route.VirtualHost{}

	for _, res := range resources {
		if err := proto.Unmarshal(res.GetValue(), &route); err != nil {
			fmt.Println("Failed to unmarshal resource: ", err)
		} else {
			vhosts := route.GetVirtualHosts()
			for _, vhost := range vhosts {
				if vhost.Name == fmt.Sprintf("%s:%s", serviceName, port) {
					virtualHosts = append(virtualHosts, vhost)
					for _, r := range vhost.Routes {
						routerClusterName := r.GetRoute().GetCluster()
						fmt.Println("[JUZHEN DEBUG]: ", routerClusterName)
					}
				}
			}
		}
	}
	return virtualHosts, nil
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
