package pilotv2

import "github.com/go-chassis/go-chassis/core/registry"
import "github.com/go-chassis/go-chassis/pkg/util/tags"

type ServiceDiscovery struct {
	Name   string
	client *XdsClient
}

func (discovery *ServiceDiscovery) GetMicroServiceID(appID, microServiceName, version, env string) (string, error) {

	return "", nil
}

func (discovery *ServiceDiscovery) GetAllMicroServices() ([]*registry.MicroService, error) {
	return nil, nil
}

func (discovery *ServiceDiscovery) GetMicroService(microServiceID string) (*registry.MicroService, error) {
	return nil, nil
}

func (discovery *ServiceDiscovery) GetMicroServiceInstances(consumerID, providerID string) ([]*registry.MicroServiceInstance, error) {
	return nil, nil
}

func (discovery *ServiceDiscovery) FindMicroServiceInstances(consumerID, microServiceName string, tags utiltags.Tags) ([]*registry.MicroServiceInstance, error) {
	return nil, nil
}

func (discovery *ServiceDiscovery) AutoSync() {

}

func (discovery *ServiceDiscovery) Close() error {
	return nil
}

func NewDiscoveryService(options registry.Options) registry.ServiceDiscovery {
	pilotAddr := options.Addrs[0]
	xdsClient, err := NewXdsClient(pilotAddr, options.TLSConfig)
	if err != nil {
		panic("Failed to create XDS client: " + err.Error())
	}

	discovery := &ServiceDiscovery{
		client: xdsClient,
		Name:   "pilotv2",
	}

	return discovery
}

func init() {
	registry.InstallServiceDiscovery("pilotv2", NewDiscoveryService)
}
