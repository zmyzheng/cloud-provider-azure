/*
Copyright 2022 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package provider

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-07-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-08-01/network"
	"github.com/Azure/go-autorest/autorest/to"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

var (
	// ErrorVmssIDIsEmpty indicates the vmss id is empty.
	ErrorVmssIDIsEmpty = errors.New("VMSS ID is empty")
)

// FlexScaleSet implements VMSet interface for Azure Flexible VMSS.
type FlexScaleSet struct {
	*Cloud

	vmssFlexCache *azcache.TimedCache

	vmssFlexVMNameToVmssID   *sync.Map
	vmssFlexVMNameToNodeName *sync.Map
	vmssFlexVMCache          *azcache.TimedCache

	// lockMap in cache refresh
	lockMap *lockMap
}

func newFlexScaleSet(az *Cloud) (VMSet, error) {
	fs := &FlexScaleSet{
		Cloud:                    az,
		vmssFlexVMNameToVmssID:   &sync.Map{},
		vmssFlexVMNameToNodeName: &sync.Map{},
		lockMap:                  newLockMap(),
	}

	var err error
	fs.vmssFlexCache, err = fs.newVmssFlexCache()
	if err != nil {
		return nil, err
	}
	fs.vmssFlexVMCache, err = fs.newVmssFlexVMCache()
	if err != nil {
		return nil, err
	}

	return fs, nil
}

// GetNodeNameByProviderID gets the node name by provider ID.
func (fs *FlexScaleSet) GetNodeNameByProviderID(providerID string) (types.NodeName, error) {

	return types.NodeName(""), nil
}

// GetPrimaryVMSetName returns the VM set name depending on the configured vmType.
// It returns config.PrimaryScaleSetName for vmss and config.PrimaryAvailabilitySetName for standard vmType.
func (fs *FlexScaleSet) GetPrimaryVMSetName() string {
	return fs.Config.PrimaryScaleSetName
}

// getNodeVMSetName returns the vmss flex name by the node name.
func (fs *FlexScaleSet) getNodeVmssFlexName(nodeName string) (string, error) {
	vmssFlexID, err := fs.getNodeVmssFlexID(nodeName)
	if err != nil {
		return "", err
	}
	vmssFlexName, err := getLastSegment(vmssFlexID, "/")
	if err != nil {
		return "", err
	}
	return vmssFlexName, nil

}

// GetNodeVMSetName returns the availability set or vmss name by the node name.
// It will return empty string when using standalone vms.
func (fs *FlexScaleSet) GetNodeVMSetName(node *v1.Node) (string, error) {
	return fs.getNodeVmssFlexName(node.Name)
}

// GetAgentPoolVMSetNames returns all vmSet names according to the nodes
func (fs *FlexScaleSet) GetAgentPoolVMSetNames(nodes []*v1.Node) (*[]string, error) {
	vmSetNames := make([]string, 0)
	for _, node := range nodes {
		vmSetName, err := fs.GetNodeVMSetName(node)
		if err != nil {
			klog.Errorf("Unable to get the vmss flex name by node name %s: %v", node.Name, err)
			continue
		}
		vmSetNames = append(vmSetNames, vmSetName)
	}
	return &vmSetNames, nil
}

// GetVMSetNames selects all possible availability sets or scale sets
// (depending vmType configured) for service load balancer, if the service has
// no loadbalancer mode annotation returns the primary VMSet. If service annotation
// for loadbalancer exists then returns the eligible VMSet. The mode selection
// annotation would be ignored when using one SLB per cluster.
func (fs *FlexScaleSet) GetVMSetNames(service *v1.Service, nodes []*v1.Node) (*[]string, error) {
	hasMode, isAuto, serviceVMSetName := fs.getServiceLoadBalancerMode(service)
	useSingleSLB := fs.useStandardLoadBalancer() && !fs.EnableMultipleStandardLoadBalancers
	if !hasMode || useSingleSLB {
		// no mode specified in service annotation or use single SLB mode
		// default to PrimaryScaleSetName
		vmssFlexNames := &[]string{fs.Config.PrimaryScaleSetName}
		return vmssFlexNames, nil
	}

	vmssFlexNames, err := fs.GetAgentPoolVMSetNames(nodes)
	if err != nil {
		klog.Errorf("fs.GetVMSetNames - GetAgentPoolVMSetNames failed err=(%v)", err)
		return nil, err
	}

	if !isAuto {
		found := false
		for asx := range *vmssFlexNames {
			if strings.EqualFold((*vmssFlexNames)[asx], serviceVMSetName) {
				found = true
				serviceVMSetName = (*vmssFlexNames)[asx]
				break
			}
		}
		if !found {
			klog.Errorf("fs.GetVMSetNames - scale set (%s) in service annotation not found", serviceVMSetName)
			return nil, fmt.Errorf("scale set (%s) - not found", serviceVMSetName)
		}
		return &[]string{serviceVMSetName}, nil
	}
	return vmssFlexNames, nil
}

// GetInstanceIDByNodeName gets the cloud provider ID by node name.
// It must return ("", cloudprovider.InstanceNotFound) if the instance does
// not exist or is no longer running.
func (fs *FlexScaleSet) GetInstanceIDByNodeName(name string) (string, error) {
	return "", nil

}

// GetInstanceTypeByNodeName gets the instance type by node name.
func (fs *FlexScaleSet) GetInstanceTypeByNodeName(name string) (string, error) {
	return "", nil
}

// GetZoneByNodeName gets availability zone for the specified node. If the node is not running
// with availability zone, then it returns fault domain.
// for details, refer to https://kubernetes-sigs.github.io/cloud-provider-azure/topics/availability-zones/#node-labels
func (fs *FlexScaleSet) GetZoneByNodeName(name string) (cloudprovider.Zone, error) {
	return cloudprovider.Zone{}, nil
}

// GetProvisioningStateByNodeName returns the provisioningState for the specified node.
func (fs *FlexScaleSet) GetProvisioningStateByNodeName(name string) (provisioningState string, err error) {
	return "", nil
}

// GetPowerStatusByNodeName returns the powerState for the specified node.
func (fs *FlexScaleSet) GetPowerStatusByNodeName(name string) (powerState string, err error) {
	return "", nil
}

// GetPrimaryInterface gets machine primary network interface by node name.
func (fs *FlexScaleSet) GetPrimaryInterface(nodeName string) (network.Interface, error) {
	machine, err := fs.getVmssFlexVM(nodeName, azcache.CacheReadTypeDefault)
	if err != nil {
		klog.Errorf("fs.GetInstanceTypeByNodeName(%s) failed: fs.getVmssFlexVMWithoutInstanceView(%s) err=%v", nodeName, nodeName, err)
		return network.Interface{}, err
	}

	primaryNicID, err := getPrimaryInterfaceID(machine)
	if err != nil {
		return network.Interface{}, err
	}
	nicName, err := getLastSegment(primaryNicID, "/")
	if err != nil {
		return network.Interface{}, err
	}

	nicResourceGroup, err := extractResourceGroupByNicID(primaryNicID)
	if err != nil {
		return network.Interface{}, err
	}

	ctx, cancel := getContextWithCancel()
	defer cancel()
	nic, rerr := fs.InterfacesClient.Get(ctx, nicResourceGroup, nicName, "")
	if rerr != nil {
		return network.Interface{}, rerr.Error()
	}

	return nic, nil
}

// GetIPByNodeName gets machine private IP and public IP by node name.
func (fs *FlexScaleSet) GetIPByNodeName(name string) (string, string, error) {
	nic, err := fs.GetPrimaryInterface(name)
	if err != nil {
		return "", "", err
	}

	ipConfig, err := getPrimaryIPConfig(nic)
	if err != nil {
		klog.Errorf("fs.GetIPByNodeName(%s) failed: getPrimaryIPConfig(%v), err=%v", name, nic, err)
		return "", "", err
	}

	privateIP := *ipConfig.PrivateIPAddress
	publicIP := ""
	if ipConfig.PublicIPAddress != nil && ipConfig.PublicIPAddress.ID != nil {
		pipID := *ipConfig.PublicIPAddress.ID
		pipName, err := getLastSegment(pipID, "/")
		if err != nil {
			return "", "", fmt.Errorf("failed to publicIP name for node %q with pipID %q", name, pipID)
		}
		pip, existsPip, err := fs.getPublicIPAddress(fs.ResourceGroup, pipName, azcache.CacheReadTypeDefault)
		if err != nil {
			return "", "", err
		}
		if existsPip {
			publicIP = *pip.IPAddress
		}
	}

	return privateIP, publicIP, nil

}

// GetPrivateIPsByNodeName returns a slice of all private ips assigned to node (ipv6 and ipv4)
// TODO (khenidak): This should read all nics, not just the primary
// allowing users to split ipv4/v6 on multiple nics
func (fs *FlexScaleSet) GetPrivateIPsByNodeName(name string) ([]string, error) {
	ips := make([]string, 0)
	nic, err := fs.GetPrimaryInterface(name)
	if err != nil {
		return ips, err
	}

	if nic.IPConfigurations == nil {
		return ips, fmt.Errorf("nic.IPConfigurations for nic (nicname=%q) is nil", *nic.Name)
	}

	for _, ipConfig := range *(nic.IPConfigurations) {
		if ipConfig.PrivateIPAddress != nil {
			ips = append(ips, *(ipConfig.PrivateIPAddress))
		}
	}

	return ips, nil
}

// GetNodeNameByIPConfigurationID gets the nodeName and vmSetName by IP configuration ID.
func (fs *FlexScaleSet) GetNodeNameByIPConfigurationID(ipConfigurationID string) (string, string, error) {
	nodeName, vmssFlexName, _, err := fs.getNodeInformationByIPConfigurationID(ipConfigurationID)
	if err != nil {
		klog.Errorf("fs.GetNodeNameByIPConfigurationID(%s) failed. Error: %v", ipConfigurationID, err)
		return "", "", err
	}

	return nodeName, strings.ToLower(vmssFlexName), nil
}

func (fs *FlexScaleSet) getNodeInformationByIPConfigurationID(ipConfigurationID string) (string, string, string, error) {
	matches := nicIDRE.FindStringSubmatch(ipConfigurationID)
	if len(matches) != 3 {
		klog.V(4).Infof("Can not extract nic name from ipConfigurationID (%s)", ipConfigurationID)
		return "", "", "", fmt.Errorf("invalid ip config ID %s", ipConfigurationID)
	}

	nicResourceGroup, nicName := matches[1], matches[2]
	if nicResourceGroup == "" || nicName == "" {
		return "", "", "", fmt.Errorf("invalid ip config ID %s", ipConfigurationID)
	}

	vmName := strings.Replace(nicName, "-nic", "", 1)
	nodeName, err := fs.getNodeNameByVMName(vmName)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to map VM Name to NodeName: VM Name %s", vmName)
	}

	vmssFlexName, err := fs.getNodeVmssFlexName(nodeName)

	if err != nil {
		klog.Errorf("Unable to get the vmss flex name by node name %s: %v", vmName, err)
		return "", "", "", err
	}

	return nodeName, strings.ToLower(vmssFlexName), nicName, nil
}

// GetNodeCIDRMaskByProviderID returns the node CIDR subnet mask by provider ID.
func (fs *FlexScaleSet) GetNodeCIDRMasksByProviderID(providerID string) (int, int, error) {
	nodeNameWrapper, err := fs.GetNodeNameByProviderID(providerID)
	if err != nil {
		klog.Errorf("Unable to get the vmss flex vm node name by providerID %s: %v", providerID, err)
		return 0, 0, err
	}
	nodeName := mapNodeNameToVMName(nodeNameWrapper)

	vmssFlex, err := fs.getVmssFlexByNodeName(nodeName, azcache.CacheReadTypeDefault)
	if err != nil {
		if errors.Is(err, cloudprovider.InstanceNotFound) {
			return consts.DefaultNodeMaskCIDRIPv4, consts.DefaultNodeMaskCIDRIPv6, nil
		}
		return 0, 0, err
	}

	var ipv4Mask, ipv6Mask int
	if v4, ok := vmssFlex.Tags[consts.VMSetCIDRIPV4TagKey]; ok && v4 != nil {
		ipv4Mask, err = strconv.Atoi(to.String(v4))
		if err != nil {
			klog.Errorf("GetNodeCIDRMasksByProviderID: error when paring the value of the ipv4 mask size %s: %v", to.String(v4), err)
		}
	}
	if v6, ok := vmssFlex.Tags[consts.VMSetCIDRIPV6TagKey]; ok && v6 != nil {
		ipv6Mask, err = strconv.Atoi(to.String(v6))
		if err != nil {
			klog.Errorf("GetNodeCIDRMasksByProviderID: error when paring the value of the ipv6 mask size%s: %v", to.String(v6), err)
		}
	}

	return ipv4Mask, ipv6Mask, nil
}

// EnsureHostInPool ensures the given VM's Primary NIC's Primary IP Configuration is
// participating in the specified LoadBalancer Backend Pool, which returns (resourceGroup, vmasName, instanceID, vmssVM, error).
func (fs *FlexScaleSet) EnsureHostInPool(service *v1.Service, nodeName types.NodeName, backendPoolID string, vmSetNameOfLB string) (string, string, string, *compute.VirtualMachineScaleSetVM, error) {
	return "", "", "", nil, nil

}

// EnsureHostsInPool ensures the given Node's primary IP configurations are
// participating in the specified LoadBalancer Backend Pool.
func (fs *FlexScaleSet) EnsureHostsInPool(service *v1.Service, nodes []*v1.Node, backendPoolID string, vmSetNameOfLB string) error {
	return nil
}

// EnsureBackendPoolDeletedFromVMSets ensures the loadBalancer backendAddressPools deleted from the specified VMSS Flex
func (fs *FlexScaleSet) EnsureBackendPoolDeletedFromVMSets(vmssNamesMap map[string]bool, backendPoolID string) error {
	return nil
}

// EnsureBackendPoolDeleted ensures the loadBalancer backendAddressPools deleted from the specified nodes.
func (fs *FlexScaleSet) EnsureBackendPoolDeleted(service *v1.Service, backendPoolID, vmSetName string, backendAddressPools *[]network.BackendAddressPool, deleteFromVMSet bool) error {
	return nil

}
