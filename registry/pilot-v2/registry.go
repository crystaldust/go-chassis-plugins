package pilotv2

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/go-chassis/go-chassis/core/common"
	"github.com/go-chassis/go-chassis/core/metadata"
	"github.com/go-chassis/go-chassis/core/registry"
	"github.com/go-chassis/go-chassis/pkg/util/iputil"
	"github.com/go-chassis/go-chassis/pkg/util/tags"

	apiv2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	apiv2endpoint "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
)

var (
	POD_NAME      string
	POD_NAMESPACE string
	INSTANCE_IP   string
)

type ServiceDiscovery struct {
	Name    string
	client  *XdsClient
	options registry.Options
}

func (discovery *ServiceDiscovery) GetMicroServiceID(appID, microServiceName, version, env string) (string, error) {
	return "", nil
}

func (discovery *ServiceDiscovery) GetAllMicroServices() ([]*registry.MicroService, error) {
	clusters, err := discovery.client.CDS()
	if err != nil {
		return nil, err
	}
	microServices := []*registry.MicroService{}
	for _, cluster := range clusters {
		microServices = append(microServices, toMicroService(&cluster))
	}
	return microServices, nil
}

func toMicroService(cluster *apiv2.Cluster) *registry.MicroService {
	svc := &registry.MicroService{}
	svc.ServiceID = cluster.Name
	svc.ServiceName = cluster.Name
	svc.Version = common.DefaultVersion
	svc.AppID = common.DefaultApp
	svc.Level = "BACK"
	svc.Status = "UP"
	svc.Framework = &registry.Framework{
		Name:    "Istio",
		Version: common.LatestVersion,
	}
	svc.RegisterBy = metadata.PlatformRegistrationComponent

	return svc
}

func toMicroServiceInstance(clusterName string, lbendpoint *apiv2endpoint.LbEndpoint) *registry.MicroServiceInstance {
	socketAddress := lbendpoint.Endpoint.Address.GetSocketAddress()
	addr := socketAddress.Address
	port := socketAddress.GetPortValue()
	msi := &registry.MicroServiceInstance{}
	msi.InstanceID = fmt.Sprintf("%s_%d", addr, port)
	msi.HostName = clusterName
	msi.EndpointsMap = map[string]string{
		common.ProtocolRest: fmt.Sprintf("%s:%d", addr, port),
	}
	msi.DefaultEndpoint = fmt.Sprintf("%s:%d", addr, port)
	msi.DefaultProtocol = common.ProtocolRest

	return msi
}

func (discovery *ServiceDiscovery) GetMicroService(microServiceID string) (*registry.MicroService, error) {
	// If the service is in the clusters, return it, or nil

	clusters, err := discovery.client.CDS()
	if err != nil {
		return nil, err
	}

	var targetCluster apiv2.Cluster
	for _, cluster := range clusters {
		parts := strings.Split(cluster.Name, "|")
		if len(parts) < 4 {
			fmt.Println("[WARN] invalid cluster name: ", cluster.Name)
			continue
		}

		svcName := parts[3]
		if strings.Index(svcName, microServiceID+".") == 0 {
			targetCluster = cluster
			break
		}
	}

	if &targetCluster == nil {
		return nil, nil
	}

	return toMicroService(&targetCluster), nil
}

func (discovery *ServiceDiscovery) GetMicroServiceInstances(consumerID, providerID string) ([]*registry.MicroServiceInstance, error) {
	// TODO Handle the registry.MicroserviceIndex cache
	// TODO Handle the microServiceName
	service, err := discovery.GetMicroService(providerID)
	if err != nil {
		return nil, err
	}

	loadAssignment, err := discovery.client.EDS(service.ServiceName)
	if err != nil {
		return nil, err
	}

	instances := []*registry.MicroServiceInstance{}
	endpionts := loadAssignment.Endpoints
	for _, item := range endpionts {
		for _, lbendpoint := range item.LbEndpoints {
			msi := toMicroServiceInstance(loadAssignment.ClusterName, &lbendpoint)
			instances = append(instances, msi)
		}
	}

	return instances, nil
}

func (discovery *ServiceDiscovery) FindMicroServiceInstances(consumerID, microServiceName string, tags utiltags.Tags) ([]*registry.MicroServiceInstance, error) {
	fmt.Println("FindMicroServiceInstances with params: ", consumerID, microServiceName, tags)
	jsonPrint(tags)

	clusters, err := discovery.client.CDS()
	if err != nil {
		return nil, err
	}

	for _, cluster := range clusters {
		clusterInfo := ParseClusterName(cluster.Name)
		if clusterInfo == nil || clusterInfo.Subset == "" || clusterInfo.ServiceName != microServiceName {
			continue
		}
		// So clusterInfo is not nil and subset is not empty
		// TODO The code is ugly orgnized, optimize it later
		if subsetTags, err := cacheManager.GetSubsetTags(clusterInfo.Namespace, clusterInfo.ServiceName, clusterInfo.Subset); err != nil {
			//
			continue
		} else {
			// filter with tags
			matched := true
			for k, v := range tags.KV {
				if subsetTagValue, exists := subsetTags[k]; exists == false || subsetTagValue != v {
					matched = false
					continue
				}
			}

			if matched { // We got the cluster!
				fmt.Println("[JUZHEN DEBUG]: ", "We got the cluster ", cluster.Name)
				jsonPrint(subsetTags)

				loadAssignment, err := discovery.client.EDS(cluster.Name)
				if err != nil {
					return nil, err
				}

				instances := []*registry.MicroServiceInstance{}
				endpionts := loadAssignment.Endpoints
				for _, item := range endpionts {
					for _, lbendpoint := range item.LbEndpoints {
						msi := toMicroServiceInstance(loadAssignment.ClusterName, &lbendpoint)
						instances = append(instances, msi)
					}
				}

				return instances, nil
			}
		}
	}

	// Get the cluster filtered by tags

	// TODO Find micro service instances filtered by tags
	// The tags are generated by the router rules

	return nil, nil
}

var cacheManager *CacheManager

func (discovery *ServiceDiscovery) AutoSync() {
	fmt.Println("Pilot V2 Discovery AutoSync is in dirty debugging!")

	var err error
	cacheManager, err = NewCacheManager(discovery.client, discovery.options.ConfigPath)
	if err != nil {
		fmt.Println("Failed to create cache manager, auto ip indexing will not work:", err)
	} else {
		cacheManager.AutoSync()
	}
}

func (discovery *ServiceDiscovery) Close() error {
	// TODO Should we explicitly recycle discovery's other resources?
	// discovery.client.ReqCaches = nil
	return discovery.client.GrpcConn.Close()
}

func NewDiscoveryService(options registry.Options) registry.ServiceDiscovery {
	if len(options.Addrs) == 0 {
		panic("Failed to create discovery service: Address not specified")
	}
	pilotAddr := options.Addrs[0]
	nodeInfo := &NodeInfo{
		PodName:    POD_NAME,
		Namespace:  POD_NAMESPACE,
		InstanceIP: INSTANCE_IP,
	}
	fmt.Println("init xds client with node Info:")
	bs, _ := json.MarshalIndent(nodeInfo, "", " ")
	fmt.Println(string(bs))
	xdsClient, err := NewXdsClient(pilotAddr, options.TLSConfig, nodeInfo)
	if err != nil {
		panic("Failed to create XDS client: " + err.Error())
	}

	discovery := &ServiceDiscovery{
		client:  xdsClient,
		Name:    "pilotv2",
		options: options,
	}

	return discovery
}

func init() {
	// Init the node info
	POD_NAME = os.Getenv("POD_NAME")
	POD_NAMESPACE = os.Getenv("POD_NAMESPACE")
	INSTANCE_IP = os.Getenv("INSTANCE_IP")

	// TODO Handle the default value
	if POD_NAME == "" {
		POD_NAME = "pod_name_default"
	}
	if POD_NAMESPACE == "" {
		POD_NAMESPACE = "default"
	}
	if INSTANCE_IP == "" {
		// TODO Read ip from network adaptor
		//
		fmt.Println("[WARN] Env var INSTANCE_IP not found, the service might not work properly.")
		INSTANCE_IP = iputil.GetLocalIP()
		fmt.Println("localip: ", INSTANCE_IP)
		if INSTANCE_IP == "" {
			// Won't work without instance ip
			panic("Failed to get instance ip")
		}
	}

	registry.InstallServiceDiscovery("pilotv2", NewDiscoveryService)
}
