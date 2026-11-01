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
	"errors"
	"testing"

	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
)

func TestReconcilePowerState_SetTaskRef(t *testing.T) {
	ctx := context.TODO()
	machineScope, proxmoxClient, _ := setupReconcilerTestWithCondition(t, infrav1.ProxmoxMachineVirtualMachineProvisionedWaitingForVMPowerUpReason)
	machineScope.ProxmoxMachine.Status.IPAddresses = []infrav1.IPAddressesSpec{{
		NetName: string(infrav1.DefaultNetworkDevice),
		IPv4:    []string{"10.10.10.10"},
	}, {
		NetName: "default",
		IPv4:    []string{"10.10.10.10"},
	}}

	vm := newStoppedVM()
	task := newTask()
	machineScope.SetVirtualMachine(vm)
	proxmoxClient.EXPECT().StartVM(ctx, vm).Return(task, nil).Once()

	requeue, err := reconcilePowerState(ctx, machineScope)
	require.True(t, requeue)
	require.NoError(t, err)
	require.NotEmpty(t, *machineScope.ProxmoxMachine.Status.TaskRef)
}

func TestStartVirtualMachine_Paused(t *testing.T) {
	ctx := context.TODO()
	_, proxmoxClient, _ := setupReconcilerTestWithCondition(t, infrav1.ProxmoxMachineVirtualMachineProvisionedWaitingForVMPowerUpReason)
	vm := newPausedVM()
	proxmoxClient.EXPECT().ResumeVM(ctx, vm).Return(newTask(), nil).Once()

	task, err := startVirtualMachine(ctx, proxmoxClient, vm)
	require.NoError(t, err)
	require.NotNil(t, task)
}

func TestStartVirtualMachine_Stopped(t *testing.T) {
	ctx := context.TODO()
	_, proxmoxClient, _ := setupReconcilerTestWithCondition(t, infrav1.ProxmoxMachineVirtualMachineProvisionedWaitingForVMPowerUpReason)
	vm := newStoppedVM()
	proxmoxClient.EXPECT().StartVM(ctx, vm).Return(newTask(), nil).Once()

	task, err := startVirtualMachine(ctx, proxmoxClient, vm)
	require.NoError(t, err)
	require.NotNil(t, task)
}

func TestStartVirtualMachine_Hibernated(t *testing.T) {
	ctx := context.TODO()
	_, proxmoxClient, _ := setupReconcilerTestWithCondition(t, infrav1.ProxmoxMachineVirtualMachineProvisionedWaitingForVMPowerUpReason)
	vm := newHibernatedVM()
	proxmoxClient.EXPECT().StartVM(ctx, vm).Return(newTask(), nil).Once()

	task, err := startVirtualMachine(ctx, proxmoxClient, vm)
	require.NoError(t, err)
	require.NotNil(t, task)
}

func TestStartVirtualMachine_Started(t *testing.T) {
	_, proxmoxClient, _ := setupReconcilerTestWithCondition(t, infrav1.ProxmoxMachineVirtualMachineProvisionedWaitingForVMPowerUpReason)
	vm := newRunningVM()

	task, err := startVirtualMachine(context.TODO(), proxmoxClient, vm)
	require.NoError(t, err)
	require.Nil(t, task)
}

func newIOErrorVM(lock string) *proxmox.VirtualMachine {
	vm := newRunningVM()
	vm.QMPStatus = qmpStatusIOError
	vm.Lock = lock
	return vm
}

func TestReconcileIOErrorPause_Resumes(t *testing.T) {
	for _, lock := range []string{"", "backup"} {
		t.Run("lock="+lock, func(t *testing.T) {
			ctx := context.TODO()
			machineScope, proxmoxClient, _ := setupReconcilerTestWithCondition(t, infrav1.ProxmoxMachineVirtualMachineProvisionedWaitingForCloudInitReason)
			vm := newIOErrorVM(lock)
			machineScope.SetVirtualMachine(vm)
			proxmoxClient.EXPECT().ResumeVM(ctx, vm).Return(newTask(), nil).Once()

			reconcileIOErrorPause(ctx, machineScope)

			c := conditions.Get(machineScope.ProxmoxMachine, infrav1.ProxmoxMachineVirtualMachinePausedCondition)
			require.NotNil(t, c)
			require.Equal(t, metav1.ConditionTrue, c.Status)
			require.Equal(t, infrav1.ProxmoxMachineVirtualMachinePausedIOErrorReason, c.Reason)
			require.Nil(t, machineScope.ProxmoxMachine.Status.TaskRef)
			require.Equal(t, infrav1.ProxmoxMachineVirtualMachineProvisionedWaitingForCloudInitReason,
				conditions.GetReason(machineScope.ProxmoxMachine, infrav1.ProxmoxMachineVirtualMachineProvisionedCondition))
		})
	}
}

func TestReconcileIOErrorPause_LockedWaits(t *testing.T) {
	machineScope, _, _ := setupReconcilerTestWithCondition(t, infrav1.ProxmoxMachineVirtualMachineProvisionedWaitingForCloudInitReason)
	machineScope.SetVirtualMachine(newIOErrorVM("migrate"))

	reconcileIOErrorPause(context.TODO(), machineScope)

	c := conditions.Get(machineScope.ProxmoxMachine, infrav1.ProxmoxMachineVirtualMachinePausedCondition)
	require.NotNil(t, c)
	require.Equal(t, metav1.ConditionTrue, c.Status)
	require.Contains(t, c.Message, "migrate lock")
}

func TestReconcileIOErrorPause_FailedResumeIsReported(t *testing.T) {
	ctx := context.TODO()
	machineScope, proxmoxClient, _ := setupReconcilerTestWithCondition(t, infrav1.ProxmoxMachineVirtualMachineProvisionedWaitingForCloudInitReason)
	vm := newIOErrorVM("")
	machineScope.SetVirtualMachine(vm)
	proxmoxClient.EXPECT().ResumeVM(ctx, vm).Return(nil, errors.New("storage full")).Once()

	reconcileIOErrorPause(ctx, machineScope)

	c := conditions.Get(machineScope.ProxmoxMachine, infrav1.ProxmoxMachineVirtualMachinePausedCondition)
	require.NotNil(t, c)
	require.Equal(t, metav1.ConditionTrue, c.Status)
	require.Contains(t, c.Message, "storage full")
}

func TestReconcileIOErrorPause_LeavesOtherStatesAlone(t *testing.T) {
	for _, qmp := range []string{proxmox.StatusVirtualMachineRunning, proxmox.StatusVirtualMachinePaused, "prelaunch", "postmigrate", "suspended"} {
		t.Run(qmp, func(t *testing.T) {
			machineScope, _, _ := setupReconcilerTestWithCondition(t, infrav1.ProxmoxMachineVirtualMachineProvisionedWaitingForCloudInitReason)
			conditions.Set(machineScope.ProxmoxMachine, metav1.Condition{
				Type:   infrav1.ProxmoxMachineVirtualMachinePausedCondition,
				Status: metav1.ConditionTrue,
				Reason: infrav1.ProxmoxMachineVirtualMachinePausedIOErrorReason,
			})
			vm := newRunningVM()
			vm.QMPStatus = qmp
			machineScope.SetVirtualMachine(vm)

			reconcileIOErrorPause(context.TODO(), machineScope)

			require.False(t, conditions.Has(machineScope.ProxmoxMachine, infrav1.ProxmoxMachineVirtualMachinePausedCondition))
		})
	}
	t.Run("stopped", func(t *testing.T) {
		machineScope, _, _ := setupReconcilerTestWithCondition(t, infrav1.ProxmoxMachineVirtualMachineProvisionedWaitingForCloudInitReason)
		vm := newStoppedVM()
		vm.QMPStatus = qmpStatusIOError
		machineScope.SetVirtualMachine(vm)

		reconcileIOErrorPause(context.TODO(), machineScope)

		require.False(t, conditions.Has(machineScope.ProxmoxMachine, infrav1.ProxmoxMachineVirtualMachinePausedCondition))
	})
}

func TestReconcileVM_ResumesAVMPausedOnIOError(t *testing.T) {
	machineScope, proxmoxClient, _ := setupReconcilerTestWithCondition(t, infrav1.ProxmoxMachineVirtualMachineProvisionedWaitingForCloudInitReason)
	vm := newIOErrorVM("")
	machineScope.SetVirtualMachineID(int64(vm.VMID))
	machineScope.ProxmoxMachine.Status.BootstrapDataProvided = new(true)
	machineScope.ProxmoxMachine.Status.Initialization.Provisioned = new(true)

	proxmoxClient.EXPECT().GetVM(context.Background(), "node1", int64(123)).Return(vm, nil).Once()
	proxmoxClient.EXPECT().ResumeVM(context.Background(), vm).Return(newTask(), nil).Once()
	proxmoxClient.EXPECT().QemuAgentStatus(context.Background(), vm).Return(errors.New("guest agent not running")).Once()

	_, _ = ReconcileVM(context.Background(), machineScope)

	require.True(t, conditions.IsTrue(machineScope.ProxmoxMachine, infrav1.ProxmoxMachineVirtualMachinePausedCondition))
}
