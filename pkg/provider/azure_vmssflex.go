/*
Copyright 2020 The Kubernetes Authors.

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
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

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
	// ErrorNotVmssID indicates the id is not a valid vmss id.
	ErrorVmssIDIsEmpty = errors.New("VMSS ID is empty")
)

// FlexScaleSet implements VMSet interface for Azure Flexible VMSS.
type FlexScaleSet struct {
	*Cloud

	vmssFlexCache *azcache.TimedCache

	vmssFlexVMnameToVmssID *sync.Map
	vmssFlexVMCache        *azcache.TimedCache
	vmssFlexVMStatusCache  *azcache.TimedCache

	// lockMap in cache refresh
	lockMap *lockMap
}

func (fs *FlexScaleSet) newVmssFlexCache() (*azcache.TimedCache, error) {

	getter := func(key string) (interface{}, error) {
		localCache := &sync.Map{}

		allResourceGroups, err := fs.GetResourceGroups()
		if err != nil {
			return nil, err
		}

		for _, resourceGroup := range allResourceGroups.List() {
			allScaleSets, rerr := fs.VirtualMachineScaleSetsClient.List(context.Background(), resourceGroup)
			if rerr != nil {
				if rerr.IsNotFound() {
					klog.Warningf("Skip caching vmss for resource group %s due to error: %v", resourceGroup, rerr.Error())
					continue
				}
				klog.Errorf("VirtualMachineScaleSetsClient.List failed: %v", rerr)
				return nil, rerr.Error()
			}

			for i := range allScaleSets {
				scaleSet := allScaleSets[i]
				if scaleSet.ID == nil || *scaleSet.ID == "" {
					klog.Warning("failed to get the ID of VMSS Flex")
					continue
				}

				if scaleSet.OrchestrationMode == compute.OrchestrationModeUniform {
					klog.V(2).Infof("Skip Uniform VMSS: (%s)", *scaleSet.ID)
					continue
				}

				localCache.Store(*scaleSet.ID, &scaleSet)
			}
		}

		return localCache, nil
	}

	if fs.Config.VmssVirtualMachinesCacheTTLInSeconds == 0 {
		fs.Config.VmssVirtualMachinesCacheTTLInSeconds = consts.VmssFlexCacheTTLDefaultInSeconds
	}
	return azcache.NewTimedcache(time.Duration(fs.Config.VmssVirtualMachinesCacheTTLInSeconds)*time.Second, getter)
}

func (fs *FlexScaleSet) newVmssFlexVMCache() (*azcache.TimedCache, error) {
	getter := func(key string) (interface{}, error) {
		localCache := &sync.Map{}

		ctx, cancel := getContextWithCancel()
		defer cancel()

		vms, rerr := fs.VirtualMachinesClient.ListVmssFlexVMsWithoutInstanceView(ctx, key)
		if rerr != nil {
			klog.Errorf("VMSS Flex List failed: %v", rerr)
			return nil, rerr.Error()
		}

		for i := range vms {
			vm := vms[i]
			if vm.ID != nil {
				localCache.Store(*vm.Name, &vm)
				fs.vmssFlexVMnameToVmssID.Store(*vm.Name, key)
			}
		}

		return localCache, nil
	}

	if fs.Config.VmssFlexVMCacheTTLInSeconds == 0 {
		fs.Config.VmssFlexVMCacheTTLInSeconds = consts.VmssFlexVMCacheTTLInSeconds
	}
	return azcache.NewTimedcache(time.Duration(fs.Config.VmssFlexVMCacheTTLInSeconds)*time.Second, getter)
}

func (fs *FlexScaleSet) newVmssFlexVMStatusCache() (*azcache.TimedCache, error) {
	getter := func(key string) (interface{}, error) {
		localCache := &sync.Map{}

		ctx, cancel := getContextWithCancel()
		defer cancel()

		vms, rerr := fs.VirtualMachinesClient.ListVmssFlexVMsWithOnlyInstanceView(ctx, key)
		if rerr != nil {
			klog.Errorf("VMSS Flex List failed: %v", rerr)
			return nil, rerr.Error()
		}

		for i := range vms {
			vm := vms[i]
			if vm.ID != nil {
				localCache.Store(*vm.Name, &vm)
				fs.vmssFlexVMnameToVmssID.Store(*vm.Name, key)
			}
		}

		return localCache, nil
	}

	if fs.Config.VmssFlexVMStatusCacheTTLInSeconds == 0 {
		fs.Config.VmssFlexVMStatusCacheTTLInSeconds = consts.VmssFlexVMStatusCacheTTLInSeconds
	}
	return azcache.NewTimedcache(time.Duration(fs.Config.VmssFlexVMStatusCacheTTLInSeconds)*time.Second, getter)
}

func newFlexScaleSet(az *Cloud) (VMSet, error) {
	fs := &FlexScaleSet{
		Cloud:                  az,
		vmssFlexVMnameToVmssID: &sync.Map{},
		lockMap:                newLockMap(),
	}

	var err error
	fs.vmssFlexCache, err = fs.newVmssFlexCache()
	if err != nil {
		return nil, err
	}
	fs.vmssFlexVMCache, err = fs.newVmssFlexVMCache()
	fs.vmssFlexVMStatusCache, err = fs.newVmssFlexVMStatusCache()

	return fs, nil
}

func (fs *FlexScaleSet) getNodeVmssFlexID(nodeName string) (string, error) {
	vmssFlexID, ok := fs.vmssFlexVMnameToVmssID.Load(nodeName)
	if !ok {
		machine, err := fs.getVirtualMachine(types.NodeName(nodeName), azcache.CacheReadTypeUnsafe)
		if err != nil {
			return "", err
		}
		vmssFlexID = to.String(machine.VirtualMachineScaleSet.ID)
		if vmssFlexID == "" {
			return "", ErrorVmssIDIsEmpty
		}
		fs.vmssFlexVMnameToVmssID.Store(nodeName, vmssFlexID)

	}
	return fmt.Sprintf("%v", vmssFlexID), nil
}

func (fs *FlexScaleSet) getVmssFlexVMWithoutInstanceView(nodeName string, crt azcache.AzureCacheReadType) (vm compute.VirtualMachine, err error) {
	vmssFlexID, err := fs.getNodeVmssFlexID(nodeName)
	if err != nil {
		return vm, err
	}

	cached, err := fs.vmssFlexVMCache.Get(vmssFlexID, crt)
	if err != nil {
		return vm, err
	}

	vmMap := cached.(*sync.Map)
	cachvmedVM, ok := vmMap.Load(nodeName)
	if !ok {
		fs.lockMap.LockEntry(vmssFlexID)
		defer fs.lockMap.UnlockEntry(vmssFlexID)
		cached, err = fs.vmssFlexVMCache.Get(vmssFlexID, azcache.CacheReadTypeForceRefresh)
		vmMap = cached.(*sync.Map)
		cachvmedVM, ok = vmMap.Load(nodeName)
		if !ok {
			fs.vmssFlexVMnameToVmssID.Delete(nodeName) // nodeName was saved to vmssFlexVMnameToVmssID before, but does not exist any more. In this case, delete it from the map.
			return vm, cloudprovider.InstanceNotFound
		}
	}

	return *(cachvmedVM.(*compute.VirtualMachine)), nil
}

func (fs *FlexScaleSet) getVmssFlexVMStatus(nodeName string, crt azcache.AzureCacheReadType) (vm compute.VirtualMachine, err error) {
	vmssFlexID, err := fs.getNodeVmssFlexID(nodeName)
	if err != nil {
		return vm, err
	}

	cached, err := fs.vmssFlexVMStatusCache.Get(vmssFlexID, crt)
	if err != nil {
		return vm, err
	}

	vmMap := cached.(*sync.Map)
	cachvmedVM, ok := vmMap.Load(nodeName)
	if !ok {
		fs.lockMap.LockEntry(vmssFlexID)
		defer fs.lockMap.UnlockEntry(vmssFlexID)
		cached, err = fs.vmssFlexVMStatusCache.Get(vmssFlexID, azcache.CacheReadTypeForceRefresh)
		vmMap = cached.(*sync.Map)
		cachvmedVM, ok = vmMap.Load(nodeName)
		if !ok {
			fs.vmssFlexVMnameToVmssID.Delete(nodeName) // nodeName was saved to vmssFlexVMnameToVmssID before, but does not exist any more. In this case, delete it from the map.
			return vm, cloudprovider.InstanceNotFound
		}
	}

	return *(cachvmedVM.(*compute.VirtualMachine)), nil
}

// ------------------------------------------------------------
// GetNodeNameByProviderID gets the node name by provider ID.
func (fs *FlexScaleSet) GetNodeNameByProviderID(providerID string) (types.NodeName, error) {
	// NodeName is part of providerID for standard instances.
	matches := providerIDRE.FindStringSubmatch(providerID)
	if len(matches) != 2 {
		return "", errors.New("error splitting providerID")
	}

	return types.NodeName(matches[1]), nil
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

// ------------------------------------------------------------

// GetInstanceIDByNodeName gets the cloud provider ID by node name.
// It must return ("", cloudprovider.InstanceNotFound) if the instance does
// not exist or is no longer running.
func (fs *FlexScaleSet) GetInstanceIDByNodeName(name string) (string, error) {
	machine, err := fs.getVmssFlexVMWithoutInstanceView(name, azcache.CacheReadTypeUnsafe)
	if err != nil {
		return "", err
	}
	resourceID := *machine.ID
	convertedResourceID, err := convertResourceGroupNameToLower(resourceID)
	if err != nil {
		klog.Errorf("convertResourceGroupNameToLower failed with error: %v", err)
		return "", err
	}
	return convertedResourceID, nil

}

// GetInstanceTypeByNodeName gets the instance type by node name.
func (fs *FlexScaleSet) GetInstanceTypeByNodeName(name string) (string, error) {
	machine, err := fs.getVmssFlexVMWithoutInstanceView(name, azcache.CacheReadTypeUnsafe)
	if err != nil {
		klog.Errorf("fs.GetInstanceTypeByNodeName(%s) failed: fs.getVmssFlexVMWithoutInstanceView(%s) err=%v", name, name, err)
		return "", err
	}

	if machine.HardwareProfile == nil {
		return "", fmt.Errorf("HardwareProfile of node(%s) is nil", name)
	}
	return string(machine.HardwareProfile.VMSize), nil
}

// GetZoneByNodeName gets availability zone for the specified node. If the node is not running
// with availability zone, then it returns fault domain.
// for details, refer to https://kubernetes-sigs.github.io/cloud-provider-azure/topics/availability-zones/#node-labels
func (fs *FlexScaleSet) GetZoneByNodeName(name string) (cloudprovider.Zone, error) {
	vm, err := fs.getVmssFlexVMWithoutInstanceView(name, azcache.CacheReadTypeUnsafe)
	if err != nil {
		klog.Errorf("fs.GetZoneByNodeName(%s) failed: fs.getVmssFlexVMWithoutInstanceView(%s) err=%v", name, name, err)
		return cloudprovider.Zone{}, err
	}

	var failureDomain string
	if vm.Zones != nil && len(*vm.Zones) > 0 {
		// Get availability zone for the node.
		zones := *vm.Zones
		zoneID, err := strconv.Atoi(zones[0])
		if err != nil {
			return cloudprovider.Zone{}, fmt.Errorf("failed to parse zone %q: %w", zones, err)
		}

		failureDomain = fs.makeZone(to.String(vm.Location), zoneID)
	} else if vm.VirtualMachineProperties.InstanceView != nil && vm.VirtualMachineProperties.InstanceView.PlatformFaultDomain != nil {
		// Availability zone is not used for the node, falling back to fault domain.
		failureDomain = strconv.Itoa(int(to.Int32(vm.VirtualMachineProperties.InstanceView.PlatformFaultDomain)))
	} else {
		err = fmt.Errorf("failed to get zone info")
		klog.Errorf("GetZoneByNodeName: got unexpected error %v", err)
		return cloudprovider.Zone{}, err
	}

	zone := cloudprovider.Zone{
		FailureDomain: strings.ToLower(failureDomain),
		Region:        strings.ToLower(to.String(vm.Location)),
	}
	return zone, nil
}

// GetProvisioningStateByNodeName returns the provisioningState for the specified node.
func (fs *FlexScaleSet) GetProvisioningStateByNodeName(name string) (provisioningState string, err error) {
	vm, err := fs.getVmssFlexVMWithoutInstanceView(name, azcache.CacheReadTypeDefault)
	if err != nil {
		return provisioningState, err
	}

	if vm.VirtualMachineProperties == nil || vm.VirtualMachineProperties.ProvisioningState == nil {
		return provisioningState, nil
	}

	return to.String(vm.VirtualMachineProperties.ProvisioningState), nil
}

// GetPrimaryInterface gets machine primary network interface by node name.
func (fs *FlexScaleSet) GetPrimaryInterface(nodeName string) (network.Interface, error) {
	machine, err := fs.getVmssFlexVMWithoutInstanceView(nodeName, azcache.CacheReadTypeUnsafe)
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

// ------------------------------------------------------------

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
	matches := nicIDRE.FindStringSubmatch(ipConfigurationID)
	if len(matches) != 3 {
		klog.V(4).Infof("Can not extract VM name from ipConfigurationID (%s)", ipConfigurationID)
		return "", "", fmt.Errorf("invalid ip config ID %s", ipConfigurationID)
	}

	nicResourceGroup, nicName := matches[1], matches[2]
	if nicResourceGroup == "" || nicName == "" {
		return "", "", fmt.Errorf("invalid ip config ID %s", ipConfigurationID)
	}
	nic, rerr := fs.InterfacesClient.Get(context.Background(), nicResourceGroup, nicName, "")
	if rerr != nil {
		return "", "", fmt.Errorf("GetNodeNameByIPConfigurationID(%s): failed to get interface of name %s: %w", ipConfigurationID, nicName, rerr.Error())
	}
	vmID := ""
	if nic.InterfacePropertiesFormat != nil && nic.VirtualMachine != nil {
		vmID = to.String(nic.VirtualMachine.ID)
	}
	if vmID == "" {
		klog.V(2).Infof("GetNodeNameByIPConfigurationID(%s): empty vmID", ipConfigurationID)
		return "", "", nil
	}

	matches = vmIDRE.FindStringSubmatch(vmID)
	if len(matches) != 2 {
		return "", "", fmt.Errorf("invalid virtual machine ID %s", vmID)
	}
	vmName := matches[1]

	vmssFlexName, err := fs.getNodeVmssFlexName(vmName)

	if err != nil {
		klog.Errorf("Unable to get the vmss flex name by node name %s: %v", vmName, err)
		return "", "", err
	}

	return vmName, strings.ToLower(vmssFlexName), nil
}

// ------------------------------------------------------------

// EnsureHostsInPool ensures the given Node's primary IP configurations are
// participating in the specified LoadBalancer Backend Pool.
func (fs *FlexScaleSet) EnsureHostsInPool(service *v1.Service, nodes []*v1.Node, backendPoolID string, vmSetName string) error {

}
