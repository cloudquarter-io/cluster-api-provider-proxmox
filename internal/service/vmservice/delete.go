/*
Copyright 2023-2026 IONOS Cloud.

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

package vmservice

import (
	"context"
	"strings"

	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox/goproxmox"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/scope"
)

// DeleteVM implements the logic of destroying a VM.
func DeleteVM(ctx context.Context, machineScope *scope.MachineScope) error {
	vmID := machineScope.ProxmoxMachine.GetVirtualMachineID()
	if err := locateVMForDelete(ctx, machineScope); err != nil {
		return err
	}
	node := machineScope.LocateProxmoxNode()

	if _, err := machineScope.ProxmoxClient().DeleteVM(ctx, node, vmID); err != nil {
		if VMNotFound(err) || errors.Is(err, goproxmox.ErrVMIDFree) {
			// remove machine from cluster status
			machineScope.InfraCluster.ProxmoxCluster.RemoveNodeLocation(machineScope.Name(), util.IsControlPlaneMachine(machineScope.Machine))
			// The VM is deleted so remove the finalizer.
			ctrlutil.RemoveFinalizer(machineScope.ProxmoxMachine, infrav1.MachineFinalizer)
			return machineScope.InfraCluster.PatchObject()
		}
		conditions.Set(machineScope.ProxmoxMachine, metav1.Condition{
			Type:   infrav1.ProxmoxMachineVirtualMachineProvisionedCondition,
			Status: metav1.ConditionFalse,
			Reason: infrav1.ProxmoxMachineVirtualMachineProvisionedDeletionFailedReason,
		})
		return err
	}

	return nil
}

// locateVMForDelete records the node the VM runs on now. Proxmox HA moves a
// VM without CAPMOX, and on the node it left the VM "does not exist", which
// DeleteVM reads as already deleted: it would drop the finalizer and leave
// the VM running. A VMID no node lists, or one that names another VM, keeps
// the recorded node, so the existing checks decide.
func locateVMForDelete(ctx context.Context, machineScope *scope.MachineScope) error {
	vmID := machineScope.ProxmoxMachine.GetVirtualMachineID()
	if vmID < 100 {
		return nil
	}
	rsc, err := machineScope.ProxmoxClient().FindVMResource(ctx, uint64(vmID))
	if errors.Is(err, goproxmox.ErrVMNotListed) {
		return nil
	}
	if err != nil {
		return errors.Wrapf(err, "unable to locate vm %d before deleting it", vmID)
	}
	if rsc.Name == machineScope.ProxmoxMachine.GetName() && rsc.Node != "" {
		machineScope.ProxmoxMachine.Status.ProxmoxNode = new(rsc.Node)
	}
	return nil
}

// VMNotFound checks if the given err is related to that the VM is not found in Proxmox.
func VMNotFound(err error) bool {
	return strings.Contains(err.Error(), "does not exist")
}
