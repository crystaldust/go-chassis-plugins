package pilotv2

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-chassis/go-chassis/core/archaius"
	"github.com/go-chassis/go-chassis/core/config"
	"github.com/go-chassis/go-chassis/core/registry"
	"k8s.io/client-go/rest"
)

const (
	DefaultRefreshInterval = time.Second * 10
)

type CacheManager struct {
	xdsClient *XdsClient
	k8sClient *rest.RESTClient
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
	old := registry.MicroserviceInstanceIndex.Items()
	labels := registry.MicroserviceInstanceIndex.GetIndexTags()
	fmt.Println("pullMicroserviceInstance")

	fmt.Println(len(old))
	// jsonPrint(old)
	jsonPrint(labels)

	// for serviceKey, store := range old {
	//     for key := range store.Items() {
	//         tags := pilotTags(labels, key)
	//         hs, err := cm.client.GetHostsByKey(serviceKey, tags)
	//         if err != nil {
	//             continue
	//         }
	//         filterRestore(hs.Hosts, serviceKey, tags)
	//     }
	// }
	return nil
}

func (cm *CacheManager) MakeIPIndex() error {
	fmt.Println("Make IP index")
	clusters, err := cm.xdsClient.CDS()
	if err != nil {
		fmt.Println(err, "Failed to get clusters")
		return err
	}
	for _, cluster := range clusters {
		// xDS v2 API: CDS won't obtain the cluster's endpoints, call EDS to get the endpoints
		loadAssignment, err := cm.xdsClient.EDS(cluster.Name)
		if err != nil {
			fmt.Println(err, "Failed to get endpoints of cluster %s", cluster.Name)
			return err
		}

		endpoints := loadAssignment.Endpoints
		for _, endpoint := range endpoints {
			for _, lbendpoint := range endpoint.LbEndpoints {
				socketAddress := lbendpoint.Endpoint.Address.GetSocketAddress()
				si := &registry.SourceInfo{}
				// TODO Get tags by subset and put them into si.Tags
				si.Name = loadAssignment.ClusterName

				clusterInfo := ParseClusterName(loadAssignment.ClusterName)
				if clusterInfo != nil && clusterInfo.Subset != "" { // Only clusters with subset contain labels
					if tags, err := cm.GetSubsetTags(clusterInfo.Namespace, clusterInfo.ServiceName, clusterInfo.Subset); err == nil {
						si.Tags = tags
						fmt.Printf("%s:%d\n", socketAddress.GetAddress(), socketAddress.GetPortValue())
						jsonPrint(si)
					}
				}

				ipAddr := fmt.Sprintf("%s:%d", socketAddress.GetAddress(), socketAddress.GetPortValue())
				registry.SetIPIndex(ipAddr, si)
				// TODO Why don't we have to index every endpoint?
				// break
			}
		}
	}
	return nil
}

func (cm *CacheManager) GetSubsetTags(namespace, hostName, subsetName string) (map[string]string, error) {
	req := cm.k8sClient.Get()
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

func NewCacheManager(xdsClient *XdsClient, kubeConfig string) (*CacheManager, error) {
	cacheManager := &CacheManager{
		xdsClient: xdsClient,
	}

	k8sClient, err := CreateK8SRestClient(kubeConfig, "apis", "networking.istio.io", "v1alpha3")
	if err != nil {
		return nil, err
	}
	cacheManager.k8sClient = k8sClient

	return cacheManager, nil
}

type XdsCluster struct {
	Direction    string
	Port         string
	Subset       string
	HostName     string
	ServiceName  string
	Namespace    string
	DomainSuffix string // DomainSuffix might not be used
}

func ParseClusterName(clusterName string) *XdsCluster {
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

	cluster := &XdsCluster{
		Direction:    parts[0],
		Port:         parts[1],
		Subset:       parts[2],
		HostName:     parts[3],
		ServiceName:  hostnameParts[0],
		Namespace:    hostnameParts[1],
		DomainSuffix: strings.Join(hostnameParts[2:], "."),
	}

	return cluster
}
