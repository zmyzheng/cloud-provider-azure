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
	"fmt"
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

func (fs *FlexScaleSet) deleteCacheForNode(nodeName string) error {
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

	cached, err = fs.vmssFlexVMStatusCache.Get(vmssFlexID, azcache.CacheReadTypeUnsafe)
	if err != nil {
		klog.Errorf("fs.deleteCacheForNode(%s) failed with error: %v", nodeName, err)
		return err
	}
	vmMap = cached.(*sync.Map)
	vmMap.Delete(nodeName)

	return nil
}
