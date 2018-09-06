package pilotv2

import (
	"errors"
	"fmt"
	"strings"
	"time"

	apiv2endpoint "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	"github.com/go-chassis/go-chassis/core/archaius"
	"github.com/go-chassis/go-chassis/core/common"
	"github.com/go-chassis/go-chassis/core/config"
	"github.com/go-chassis/go-chassis/core/registry"
)

const (
	DefaultRefreshInterval = time.Second * 30
)

var simpleCache EndpointCache

func init() {
	simpleCache = EndpointCache{
		cache: map[string]EndpointSubset{},
	}
}

type CacheManager struct {
	xdsClient *XdsClient
}

func (cm *CacheManager) AutoSync() {
	cm.refreshCache()

	var ticker *time.Ticker
	refreshInterval := config.GetServiceDiscoveryRefreshInterval()
	if refreshInterval == "" {
		ticker = time.NewTicker(DefaultRefreshInterval)
	} else {
		timeValue, err := time.ParseDuration(refreshInterval)
		if err != nil {
			fmt.Println(err, "refeshInterval is invalid. So use Default value")
			timeValue = DefaultRefreshInterval
		}
		ticker = time.NewTicker(timeValue)
	}
	go func() {
		for range ticker.C {
			cm.refreshCache()
		}
	}()
}

func (cm *CacheManager) refreshCache() {
	// TODO What is the design of autodiscovery
	if archaius.GetBool("cse.service.registry.autodiscovery", false) {
		// TODO CDS
		fmt.Println(errors.New("not supported"), "SyncPilotEndpoints failed.")
	}

	err := cm.pullMicroserviceInstance()
	if err != nil {
		fmt.Println(err, "AutoUpdateMicroserviceInstance failed.")
	}

	if archaius.GetBool("cse.service.registry.autoSchemaIndex", false) {
		fmt.Println(errors.New("Not support operation"), "MakeSchemaIndex failed.")
	}

	if archaius.GetBool("cse.service.registry.autoIPIndex", false) {
		err = cm.MakeIPIndex()
		if err != nil {
			fmt.Println(err, "Auto Update IP index failed.")
		}
	}
}

func (cm *CacheManager) pullMicroserviceInstance() error {
	clusterInfos, err := cm.getClusterInfos()
	if err != nil {
		return err
	}

	for _, clusterInfo := range clusterInfos {
		if clusterInfo.Subset != "" {
			if strings.Index(clusterInfo.ClusterName, "pilotv2server") != -1 {
				fmt.Println("updating pilotv2,", clusterInfo.ClusterName)
				fmt.Println(clusterInfo.Addrs)
			}
			// Update the cache
			instances := []*registry.MicroServiceInstance{}
			for _, addr := range clusterInfo.Addrs {
				msi := &registry.MicroServiceInstance{}
				msi.InstanceID = strings.Replace(addr, ":", "_", 0)
				msi.HostName = clusterInfo.ClusterName
				msi.EndpointsMap = map[string]string{
					common.ProtocolRest: addr,
				}
				msi.DefaultEndpoint = addr
				msi.DefaultProtocol = common.ProtocolRest
				msi.Metadata = clusterInfo.Tags

				instances = append(instances)
			}

			endpointSubset := EndpointSubset{
				tags:      clusterInfo.Tags,
				instances: instances,
			}
			simpleCache.Set(clusterInfo.ClusterName, endpointSubset)
		}
	}

	return nil
}

// TODO Use getClusterInfo to replace the logic
func (cm *CacheManager) MakeIPIndex() error {
	clusterInfos, err := cm.getClusterInfos()
	if err != nil {
		return err
	}

	for _, clusterInfo := range clusterInfos {
		for _, addr := range clusterInfo.Addrs {
			si := &registry.SourceInfo{}
			// TODO Get tags by subset and put them into si.Tags
			si.Name = clusterInfo.ClusterName
			si.Tags = clusterInfo.Tags
			registry.SetIPIndex(addr, si)
			// TODO Why don't we have to index every endpoint?
			// break
		}
	}

	return nil
}

func NewCacheManager(xdsClient *XdsClient, kubeConfig string) (*CacheManager, error) {
	cacheManager := &CacheManager{
		xdsClient: xdsClient,
	}

	return cacheManager, nil
}

type XdsClusterInfo struct {
	ClusterName  string
	Direction    string
	Port         string
	Subset       string
	HostName     string
	ServiceName  string
	Namespace    string
	DomainSuffix string // DomainSuffix might not be used
	Tags         map[string]string
	Addrs        []string // The accessible addresses of the endpoints
}

func ParseClusterName(clusterName string) *XdsClusterInfo {
	// clusterName format: |direction|port|subset|hostName|
	// hostName format: |svc.namespace.svc.cluster.local

	parts := strings.Split(clusterName, "|")
	if len(parts) != 4 {
		return nil
	}

	hostnameParts := strings.Split(parts[3], ".")
	if len(hostnameParts) < 2 {
		return nil
	}

	cluster := &XdsClusterInfo{
		Direction:    parts[0],
		Port:         parts[1],
		Subset:       parts[2],
		HostName:     parts[3],
		ServiceName:  hostnameParts[0],
		Namespace:    hostnameParts[1],
		DomainSuffix: strings.Join(hostnameParts[2:], "."),
		ClusterName:  clusterName,
	}

	return cluster
}

func (cm *CacheManager) getClusterInfos() ([]XdsClusterInfo, error) {
	clusterInfos := []XdsClusterInfo{}

	clusters, err := cm.xdsClient.CDS()
	if err != nil {
		return nil, err
	}

	for _, cluster := range clusters {
		// xDS v2 API: CDS won't obtain the cluster's endpoints, call EDS to get the endpoints

		clusterInfo := ParseClusterName(cluster.Name)
		if clusterInfo == nil {
			continue
		}

		// Get Tags
		if clusterInfo.Subset != "" { // Only clusters with subset contain labels
			if tags, err := cm.xdsClient.GetSubsetTags(clusterInfo.Namespace, clusterInfo.ServiceName, clusterInfo.Subset); err == nil {
				clusterInfo.Tags = tags
			}
		}

		// Get cluster instances' addresses
		loadAssignment, err := cm.xdsClient.EDS(cluster.Name)
		if err != nil {
			return nil, err
		}
		endpoints := loadAssignment.Endpoints
		for _, endpoint := range endpoints {
			for _, lbendpoint := range endpoint.LbEndpoints {
				socketAddress := lbendpoint.Endpoint.Address.GetSocketAddress()
				ipAddr := fmt.Sprintf("%s:%d", socketAddress.GetAddress(), socketAddress.GetPortValue())
				clusterInfo.Addrs = append(clusterInfo.Addrs, ipAddr)
			}
		}
	}
	return clusterInfos, nil
}

// TODO Cache with registry index cache
func updateInstanceIndexCache(lbendpoints []apiv2endpoint.LbEndpoint, clusterName string, tags map[string]string) {
	if len(lbendpoints) == 0 {
		simpleCache.Delete(clusterName)
		return
	}

	instances := make([]*registry.MicroServiceInstance, 0, len(lbendpoints))
	for _, lbendpoint := range lbendpoints {
		msi := toMicroServiceInstance(clusterName, &lbendpoint, tags)
		instances = append(instances, msi)
	}
	subset := EndpointSubset{
		tags:      tags,
		instances: instances,
	}
	simpleCache.Set(clusterName, subset)
}

type EndpointCache struct {
	cache map[string]EndpointSubset
}

type EndpointSubset struct {
	subsetName string
	tags       map[string]string
	instances  []*registry.MicroServiceInstance
}

func (c EndpointCache) Delete(clusterName string) {
	delete(c.cache, clusterName)
}

func (c EndpointCache) Set(clusterName string, subset EndpointSubset) {
	c.cache[clusterName] = subset
}

func (c EndpointCache) GetWithTags(serviceName string, tags map[string]string) []*registry.MicroServiceInstance {
	// Get subsets whose clusterName matches the service name
	matchedSubsets := []EndpointSubset{}
	for clusterName, subset := range c.cache {
		info := ParseClusterName(clusterName)
		if info != nil && info.ServiceName == serviceName {
			matchedSubsets = append(matchedSubsets, subset)
		}
	}

	if len(matchedSubsets) == 0 {
		return nil
	}

	var instances []*registry.MicroServiceInstance

	for _, subset := range matchedSubsets {
		if tagsMatch(subset.tags, tags) {
			instances = subset.instances
			break
		}

	}
	return instances
}

// TODO There might be some utils in go-chassis doing the same thing
func tagsMatch(tags, targetTags map[string]string) bool {
	matched := true
	for k, v := range targetTags {
		if val, exists := tags[k]; !exists || val != v {
			matched = false
			break
		}
	}
	return matched
}
