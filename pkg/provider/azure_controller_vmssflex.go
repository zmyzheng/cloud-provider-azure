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

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-07-01/compute"
	"github.com/Azure/go-autorest/autorest/azure"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
)

// WaitForUpdateResult waits for the response of the update request
func (fs *FlexScaleSet) WaitForUpdateResult(ctx context.Context, future *azure.Future, resourceGroupName, source string) error {
	if rerr := fs.VirtualMachinesClient.WaitForUpdateResult(ctx, future, resourceGroupName, source); rerr != nil {
		return rerr.Error()
	}
	return nil
}

// UpdateVM updates a vm
func (fs *FlexScaleSet) UpdateVM(ctx context.Context, nodeName types.NodeName) error {
	vmName := mapNodeNameToVMName(nodeName)
	nodeResourceGroup, err := fs.GetNodeResourceGroup(vmName)
	if err != nil {
		return err
	}
	klog.V(2).Infof("azureDisk - update(%s): vm(%s)", nodeResourceGroup, vmName)
	// Invalidate the cache right after updating
	defer func() {

		_ = fs.deleteCacheForNode(vmName)

	}()

	rerr := fs.VirtualMachinesClient.Update(ctx, nodeResourceGroup, vmName, compute.VirtualMachineUpdate{}, "update_vm")
	klog.V(2).Infof("azureDisk - update(%s): vm(%s) - returned with %v", nodeResourceGroup, vmName, rerr)
	if rerr != nil {
		return rerr.Error()
	}
	return nil
}

// GetDataDisks gets a list of data disks attached to the node.
func (fs *FlexScaleSet) GetDataDisks(nodeName types.NodeName, crt azcache.AzureCacheReadType) ([]compute.DataDisk, *string, error) {
	vm, err := fs.getVmssFlexVMWithoutInstanceView(string(nodeName), crt)
	if err != nil {
		return nil, nil, err
	}

	if vm.StorageProfile.DataDisks == nil {
		return nil, nil, nil
	}

	return *vm.StorageProfile.DataDisks, vm.ProvisioningState, nil
}
