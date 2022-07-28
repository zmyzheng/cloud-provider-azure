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
	"sync"

	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
)

var (
	// ErrorVmssIDIsEmpty indicates the vmss id is empty.
	ErrorVmssIDIsEmpty = errors.New("VMSS ID is empty")
)

// FlexScaleSet implements VMSet interface for Azure Flexible VMSS.
type FlexScaleSet struct {
	*Cloud

	vmssFlexCache *azcache.TimedCache

	vmssFlexVMNameToVmssID *sync.Map
	vmssFlexVMCache        *azcache.TimedCache
	vmssFlexVMStatusCache  *azcache.TimedCache

	// lockMap in cache refresh
	lockMap *lockMap
}

func newFlexScaleSet(az *Cloud) (*FlexScaleSet, error) { //TODO: change back to VMSet after all the functions of VMSet interface is implemented
	fs := &FlexScaleSet{
		Cloud:                  az,
		vmssFlexVMNameToVmssID: &sync.Map{},
		lockMap:                newLockMap(),
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
	fs.vmssFlexVMStatusCache, err = fs.newVmssFlexVMStatusCache()
	if err != nil {
		return nil, err
	}

	return fs, nil
}
