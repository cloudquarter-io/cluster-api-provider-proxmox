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
	"fmt"

	"github.com/luthermonson/go-proxmox"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	capmox "github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/scope"
)

func reconcilePowerState(ctx context.Context, machineScope *scope.MachineScope) (requeue bool, err error) {
	if conditions.GetReason(machineScope.ProxmoxMachine, infrav1.ProxmoxMachineVirtualMachineProvisionedCondition) != infrav1.ProxmoxMachineVirtualMachineProvisionedWaitingForVMPowerUpReason {
		// Machine is in the wrong state to reconcile, we only reconcile machines waiting to power on
		return false, nil
	}

	machineScope.V(4).Info("ensuring machine is started")

	t, err := startVirtualMachine(ctx, machineScope.ProxmoxClient(), machineScope.VirtualMachine)
	if err != nil {
		conditions.Set(machineScope.ProxmoxMachine, metav1.Condition{
			Type:    infrav1.ProxmoxMachineVirtualMachineProvisionedCondition,
			Status:  metav1.ConditionFalse,
			Reason:  infrav1.ProxmoxMachineVirtualMachineProvisionedPoweringOnFailedReason,
			Message: err.Error(),
		})
		return false, err
	}

	if t != nil {
		machineScope.ProxmoxMachine.Status.TaskRef = new(string(t.UPID))
		return true, nil
	}

	conditions.Set(machineScope.ProxmoxMachine, metav1.Condition{
		Type:   infrav1.ProxmoxMachineVirtualMachineProvisionedCondition,
		Status: metav1.ConditionFalse,
		Reason: infrav1.ProxmoxMachineVirtualMachineProvisionedWaitingForClusterAPIMachineAddressesReason,
	})
	return false, nil
}

// qmpStatusIOError is the QEMU run state of a virtual machine paused on a disk
// write error. Proxmox reports its status as running.
const qmpStatusIOError = "io-error"

// reconcileIOErrorPause resumes a virtual machine QEMU paused on a disk write
// error, at any stage of the machine's life, and holds the VirtualMachinePaused
// condition while the pause lasts. Nothing else resumes it: Proxmox, and its HA
// manager, read the virtual machine as running. The resume is a request: the
// next reconcile reads the virtual machine again and requests it again while
// the pause lasts. A lock other than backup, which Proxmox's resume accepts,
// holds the request back until it is released.
func reconcileIOErrorPause(ctx context.Context, machineScope *scope.MachineScope) {
	vm := machineScope.VirtualMachine
	if vm == nil || vm.Status != proxmox.StatusVirtualMachineRunning || vm.QMPStatus != qmpStatusIOError {
		conditions.Delete(machineScope.ProxmoxMachine, infrav1.ProxmoxMachineVirtualMachinePausedCondition)
		return
	}

	message := "QEMU paused the virtual machine on a disk write error; its resume is requested"
	switch {
	case vm.Lock != "" && vm.Lock != "backup":
		message = fmt.Sprintf("QEMU paused the virtual machine on a disk write error; its resume waits for the %s lock", vm.Lock)
	default:
		machineScope.Info("resuming the virtual machine QEMU paused on a disk write error", "vmid", vm.VMID)
		if _, err := machineScope.ProxmoxClient().ResumeVM(ctx, vm); err != nil {
			machineScope.Error(err, "unable to resume the virtual machine", "vmid", vm.VMID)
			message = fmt.Sprintf("QEMU paused the virtual machine on a disk write error; resuming it failed: %v", err)
		}
	}
	conditions.Set(machineScope.ProxmoxMachine, metav1.Condition{
		Type:    infrav1.ProxmoxMachineVirtualMachinePausedCondition,
		Status:  metav1.ConditionTrue,
		Reason:  infrav1.ProxmoxMachineVirtualMachinePausedIOErrorReason,
		Message: message,
	})
}

func startVirtualMachine(ctx context.Context, client capmox.Client, vm *proxmox.VirtualMachine) (*proxmox.Task, error) {
	if vm.IsPaused() {
		t, err := client.ResumeVM(ctx, vm)
		if err != nil {
			return nil, fmt.Errorf("unable to resume the virtual machine %d: %w", vm.VMID, err)
		}

		return t, nil
	}

	if vm.IsStopped() || vm.IsHibernated() {
		t, err := client.StartVM(ctx, vm)
		if err != nil {
			return nil, fmt.Errorf("unable to start the virtual machine %d: %w", vm.VMID, err)
		}

		return t, nil
	}

	// nothing to do.
	return nil, nil
}
