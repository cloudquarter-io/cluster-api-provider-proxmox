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

	"github.com/luthermonson/go-proxmox"
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
	released, err := destroyVM(ctx, machineScope, true)
	if err != nil {
		conditions.Set(machineScope.ProxmoxMachine, metav1.Condition{
			Type:   infrav1.ProxmoxMachineVirtualMachineProvisionedCondition,
			Status: metav1.ConditionFalse,
			Reason: infrav1.ProxmoxMachineVirtualMachineProvisionedDeletionFailedReason,
		})
		return err
	}
	if released {
		// remove machine from cluster status
		machineScope.InfraCluster.ProxmoxCluster.RemoveNodeLocation(machineScope.Name(), util.IsControlPlaneMachine(machineScope.Machine))
		// The VM is deleted so remove the finalizer.
		ctrlutil.RemoveFinalizer(machineScope.ProxmoxMachine, infrav1.MachineFinalizer)
		return machineScope.InfraCluster.PatchObject()
	}

	return nil
}

// destroyVM deletes the machine's VM. It reports released once no VM of this
// machine holds its VMID; until then a later reconcile calls it again.
// cloneUnnamed deletes a VM with an empty or placeholder name as this
// machine's, as a clone still in progress is; without it such a VM is waited
// on, neither deleted nor released.
func destroyVM(ctx context.Context, machineScope *scope.MachineScope, cloneUnnamed bool) (released bool, err error) {
	vmID := machineScope.ProxmoxMachine.GetVirtualMachineID()
	where, err := locateVMForDelete(ctx, machineScope, cloneUnnamed)
	if err == nil && where == vmUnnamed {
		return false, nil
	}
	if err == nil {
		if where == vmGone {
			err = goproxmox.ErrVMIDFree
		} else {
			_, err = machineScope.ProxmoxClient().DeleteVM(ctx, machineScope.LocateProxmoxNode(), vmID)
			// Nothing this machine can see holds an unlisted VMID: it is not our VM.
			if where == vmUnlisted && errors.Is(err, goproxmox.ErrVMNotOnNode) {
				err = goproxmox.ErrVMIDFree
			}
			// The next reconcile deletes the VM once its stop has finished.
			if errors.Is(err, goproxmox.ErrVMStopping) {
				machineScope.Info("stopping vm before deleting it", "vmID", vmID)
				return false, nil
			}
		}
	}
	if err != nil {
		if errors.Is(err, goproxmox.ErrVMIDFree) || (VMNotFound(err) && !errors.Is(err, goproxmox.ErrVMNotOnNode)) {
			return true, nil
		}
		return false, err
	}

	return false, nil
}

// vmLocation is what the cluster says about a machine's VMID before it is deleted.
type vmLocation int

const (
	// vmUnlisted: no node lists the VMID; the recorded node decides.
	vmUnlisted vmLocation = iota
	// vmOurs: the VMID is this machine's VM, on the node now recorded.
	vmOurs
	// vmGone: another guest holds the VMID; this machine's VM is gone.
	vmGone
	// vmUnnamed: a VM with no name yet holds the VMID; it may be this machine's.
	vmUnnamed
)

// locateVMForDelete finds where the machine's VMID is before deleting it.
// Proxmox HA moves a VM without CAPMOX, and on the node it left the VM "does
// not exist", which would release the machine and leave the VM running. The
// cluster listing only says where the VMID is; ownership is read from the VM
// there (vmOwnership), never from the listing's name, which can be stale.
func locateVMForDelete(ctx context.Context, machineScope *scope.MachineScope, cloneUnnamed bool) (vmLocation, error) {
	vmID := machineScope.ProxmoxMachine.GetVirtualMachineID()
	if vmID < 100 {
		return vmUnlisted, nil
	}
	rsc, err := machineScope.ProxmoxClient().FindVMResource(ctx, uint64(vmID))
	if errors.Is(err, goproxmox.ErrVMNotListed) {
		return vmUnlisted, nil
	}
	if err != nil {
		return vmUnlisted, errors.Wrapf(err, "unable to locate vm %d before deleting it", vmID)
	}
	if rsc.Node == "" {
		return vmUnlisted, nil
	}
	if rsc.Type == "lxc" {
		machineScope.Info("vmid now names a container; leaving it", "vmID", vmID, "node", rsc.Node)
		return vmGone, nil
	}
	vm, err := machineScope.ProxmoxClient().GetVM(ctx, rsc.Node, vmID)
	if err != nil {
		return vmUnlisted, errors.Wrapf(err, "unable to read vm %d on node %s before deleting it", vmID, rsc.Node)
	}
	if vm.VirtualMachineConfig == nil {
		return vmUnlisted, errors.Errorf("vm %d on node %s has no config yet", vmID, rsc.Node)
	}
	switch vmOwnership(machineScope, vm.VirtualMachineConfig, vmID) {
	case vmNotOurs:
		machineScope.Info("vmid now names another vm; leaving it", "vmID", vmID, "vmName", vm.VirtualMachineConfig.Name, "node", rsc.Node)
		return vmGone, nil
	case vmMaybeOurs:
		if !cloneUnnamed {
			machineScope.Info("vmid holds an unnamed vm; waiting for its name", "vmID", vmID, "node", rsc.Node)
			return vmUnnamed, nil
		}
	}
	machineScope.ProxmoxMachine.Status.ProxmoxNode = new(rsc.Node)
	return vmOurs, nil
}

// ownership is whether a VM is this machine's.
type ownership int

const (
	vmNotOurs ownership = iota
	vmIsOurs
	// vmMaybeOurs: a clone's empty or placeholder name, before the machine
	// recorded the VM's providerID.
	vmMaybeOurs
)

// vmOwnership reads a VM's ownership: by its SMBIOS UUID once the machine
// recorded it as its providerID, else by name.
func vmOwnership(machineScope *scope.MachineScope, config *proxmox.VirtualMachineConfig, vmID int64) ownership {
	if providerID := machineScope.GetProviderID(); providerID != "" {
		if providerID == "proxmox://"+extractUUID(config.SMBios1) {
			return vmIsOurs
		}
		return vmNotOurs
	}
	switch config.Name {
	case machineScope.ProxmoxMachine.GetName():
		return vmIsOurs
	case "", placeholderVMName(vmID):
		return vmMaybeOurs
	}
	return vmNotOurs
}

// VMNotFound checks if the given err is related to that the VM is not found in Proxmox.
func VMNotFound(err error) bool {
	return strings.Contains(err.Error(), "does not exist")
}
