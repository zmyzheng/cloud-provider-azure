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
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-07-01/compute"
	"github.com/Azure/go-autorest/autorest/to"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

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
		klog.V(2).Infof("calling getter function of VmssFlexVMCache for vmss: %s", key)
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
			if vm.Name != nil {
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
			if vm.Name != nil {
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

func (fs *FlexScaleSet) getNodeVmssFlexID(nodeName string) (string, error) {
	klog.V(2).Infof("calling fs.getNodeVmssFlexID(%s)", nodeName)
	fs.lockMap.LockEntry("getNodeVmssFlexID")
	defer fs.lockMap.UnlockEntry("getNodeVmssFlexID")
	vmssFlexID, ok := fs.vmssFlexVMnameToVmssID.Load(nodeName)
	if !ok {
		klog.V(2).Infof("nodeName %s is not saved in vmssFlexVMnameToVmssID map, send a GET request to retrieve its VmssID", nodeName)
		machine, err := fs.getVirtualMachine(types.NodeName(nodeName), azcache.CacheReadTypeUnsafe)
		if err != nil {
			return "", err
		}
		vmssFlexID = to.String(machine.VirtualMachineScaleSet.ID)
		if vmssFlexID == "" {
			return "", ErrorVmssIDIsEmpty
		}
		fs.vmssFlexVMnameToVmssID.Store(nodeName, vmssFlexID)
		_, _ = fs.vmssFlexVMStatusCache.Get(fmt.Sprintf("%v", vmssFlexID), azcache.CacheReadTypeForceRefresh)

	}
	return fmt.Sprintf("%v", vmssFlexID), nil
}

func (fs *FlexScaleSet) getVmssFlexVMWithoutInstanceView(nodeName string, crt azcache.AzureCacheReadType) (vm compute.VirtualMachine, err error) {
	klog.V(2).Infof("calling fs.getVmssFlexVMWithoutInstanceView(%s)", nodeName)
	vmssFlexID, err := fs.getNodeVmssFlexID(nodeName)
	if err != nil {
		klog.Errorf("fs.getNodeVmssFlexID failed: %v", err)
		return vm, err
	}

	cached, err := fs.vmssFlexVMCache.Get(vmssFlexID, crt)
	if err != nil {
		klog.Errorf("fs.vmssFlexVMCache.Get(%s) failed: %v", vmssFlexID, err)
		return vm, err
	}

	vmMap := cached.(*sync.Map)
	fs.lockMap.LockEntry(vmssFlexID)
	defer fs.lockMap.UnlockEntry(vmssFlexID)
	cachvmedVM, ok := vmMap.Load(nodeName)
	if !ok {
		klog.V(2).Infof("did not find node (%s) in the existing cache, force refreshing the cache...", nodeName)
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
	klog.V(2).Infof("calling fs.getVmssFlexVMStatus(%s)", nodeName)
	vmssFlexID, err := fs.getNodeVmssFlexID(nodeName)
	if err != nil {
		klog.Errorf("fs.getNodeVmssFlexID failed: %v", err)
		return vm, err
	}

	cached, err := fs.vmssFlexVMStatusCache.Get(vmssFlexID, crt)
	if err != nil {
		klog.Errorf("fs.vmssFlexVMStatusCache.Get(%s) failed: %v", vmssFlexID, err)
		return vm, err
	}

	vmMap := cached.(*sync.Map)
	fs.lockMap.LockEntry(vmssFlexID)
	defer fs.lockMap.UnlockEntry(vmssFlexID)
	cachvmedVM, ok := vmMap.Load(nodeName)
	if !ok {
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

func (fs *FlexScaleSet) deleteCacheForNode(nodeName string) error {
	klog.V(2).Infof("calling fs.deleteCacheForNode(%s)", nodeName)
	vmssFlexID, err := fs.getNodeVmssFlexID(nodeName)
	if err != nil {
		klog.Errorf("fs.deleteCacheForNode(%s) failed with error: %v", nodeName, err)
		return err
	}
	cached, err := fs.vmssFlexVMCache.Get(vmssFlexID, azcache.CacheReadTypeUnsafe)
	if err != nil {
		klog.Errorf("fs.deleteCacheForNode(%s) failed with error: %v", nodeName, err)
		return err
	}
	vmMap := cached.(*sync.Map)

	vmMap.Delete(nodeName)
	klog.V(2).Infof("fs.deleteCacheForNode(%s): done", nodeName)
	return nil
}

func (fs *FlexScaleSet) getVmssFlexByVmssFlexID(vmssFlexID string, crt azcache.AzureCacheReadType) (*compute.VirtualMachineScaleSet, error) {
	cached, err := fs.vmssFlexCache.Get(consts.VmssFlexKey, crt)
	if err != nil {
		return nil, err
	}

	vmssFlexes := cached.(*sync.Map)
	if vmssFlex, ok := vmssFlexes.Load(vmssFlexID); ok {
		result := vmssFlex.(*compute.VirtualMachineScaleSet)
		return result, nil
	} else {
		klog.V(2).Infof("Couldn't find VMSS Flex with ID %s, refreshing the cache", vmssFlexID)
		cached, err = fs.vmssFlexCache.Get(consts.VmssFlexKey, azcache.CacheReadTypeForceRefresh)
		if err != nil {
			return nil, err
		}

		vmssFlexes = cached.(*sync.Map)
		if vmssFlex, ok = vmssFlexes.Load(vmssFlexID); ok {
			result := vmssFlex.(*compute.VirtualMachineScaleSet)
			return result, nil
		} else {
			return nil, cloudprovider.InstanceNotFound
		}
	}

}

func (fs *FlexScaleSet) getVmssFlexByNodeName(nodeName string, crt azcache.AzureCacheReadType) (*compute.VirtualMachineScaleSet, error) {
	vmssFlexID, err := fs.getNodeVmssFlexID(nodeName)
	if err != nil {
		klog.Errorf("fs.getNodeVmssFlexID(%s) failed with error: %v", nodeName, err)
		return nil, err
	}
	vmssFlex, err := fs.getVmssFlexByVmssFlexID(vmssFlexID, crt)
	if err != nil {
		klog.Errorf("s.getVmssFlexByVmssFlexID failed with error: %v", err)
		return nil, err
	}
	return vmssFlex, nil
}

func (fs *FlexScaleSet) getVmssFlexIDByName(vmssFlexName string) (string, error) {
	cached, err := fs.vmssFlexCache.Get(consts.VmssFlexKey, azcache.CacheReadTypeDefault)
	if err != nil {
		return "", err
	}

	var targetVmssFlexID string
	vmssFlexes := cached.(*sync.Map)
	vmssFlexes.Range(func(key, value interface{}) bool {
		vmssFlexID := key.(string)
		name, err := getLastSegment(vmssFlexID, "/")
		if err != nil {
			return true
		}
		if strings.EqualFold(name, vmssFlexName) {
			targetVmssFlexID = vmssFlexID
			return false
		}
		return true
	})
	if targetVmssFlexID != "" {
		return targetVmssFlexID, nil
	} else {
		return "", errors.New("failed to get vmssFlexID by name")
	}
}

func (fs *FlexScaleSet) getVmssFlexByName(vmssFlexName string) (*compute.VirtualMachineScaleSet, error) {
	cached, err := fs.vmssFlexCache.Get(consts.VmssFlexKey, azcache.CacheReadTypeDefault)
	if err != nil {
		return nil, err
	}

	var targetVmssFlex *compute.VirtualMachineScaleSet
	vmssFlexes := cached.(*sync.Map)
	vmssFlexes.Range(func(key, value interface{}) bool {
		vmssFlexID := key.(string)
		vmssFlex := value.(*compute.VirtualMachineScaleSet)
		name, err := getLastSegment(vmssFlexID, "/")
		if err != nil {
			return true
		}
		if strings.EqualFold(name, vmssFlexName) {
			targetVmssFlex = vmssFlex
			return false
		}
		return true
	})
	if targetVmssFlex != nil {
		return targetVmssFlex, nil
	} else {
		return nil, errors.New("failed to get vmssFlex by name")
	}
}
