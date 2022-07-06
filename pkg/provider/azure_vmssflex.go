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
	"regexp"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-07-01/compute"
	"k8s.io/klog/v2"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

var (
	vmssResourceGroupRE = regexp.MustCompile(`.*/subscriptions/(?:.*)/resourceGroups/(.+)/providers/Microsoft.Compute/virtualMachineScaleSets/(?:.*)`)
)

// FlexScaleSet implements VMSet interface for Azure Flexible VMSS.
type FlexScaleSet struct {
	*Cloud

	vmssFlexCache *azcache.TimedCache

	vmssFlexVMIDToVmssID *sync.Map
	vmssFlexVMCache      *azcache.TimedCache

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

		resourceGroup, err := fs.extractResourceGroupByVmssID(key)
		if err != nil {
			return nil, err
		}

		ctx, cancel := getContextWithCancel()
		defer cancel()

		vms, rerr := fs.VirtualMachinesClient.ListVmssFlexVMs(ctx, resourceGroup, key)
		if rerr != nil {
			klog.Errorf("VMSS Flex List failed: %v", rerr)
			return nil, rerr.Error()
		}

		for i := range vms {
			vm := vms[i]
			if vm.ID != nil {
				localCache.Store(*vm.ID, &vm)
				fs.vmssFlexVMIDToVmssID.Store(*vm.ID, key)
			}
		}

		return localCache, nil
	}

	if fs.Config.VmssFlexVMCacheTTLInSeconds == 0 {
		fs.Config.VmssFlexVMCacheTTLInSeconds = consts.VmssFlexVMCacheTTLInSeconds
	}
	return azcache.NewTimedcache(time.Duration(fs.Config.VmssFlexVMCacheTTLInSeconds)*time.Second, getter)
}

func newFlexScaleSet(az *Cloud) (VMSet, error) {
	fs := &FlexScaleSet{
		Cloud:                az,
		vmssFlexVMIDToVmssID: &sync.Map{},
		lockMap:              newLockMap(),
	}

	var err error
	fs.vmssFlexCache, err = fs.newVmssFlexCache()
	if err != nil {
		return nil, err
	}
	fs.vmssFlexVMCache, err = fs.newVmssFlexVMCache()

	return fs, nil
}

func (fs *FlexScaleSet) extractResourceGroupByVmssID(vmssID string) (string, error) {
	matches := vmssResourceGroupRE.FindStringSubmatch(vmssID)
	if len(matches) != 2 {
		return "", ErrorNotVmssInstance
	}

	return matches[1], nil
}
