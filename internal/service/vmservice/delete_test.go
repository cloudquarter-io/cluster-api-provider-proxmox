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
	"fmt"
	"testing"

	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox/goproxmox"
)

func TestDeleteVM_SuccessNotFound(t *testing.T) {
	machineScope, proxmoxClient, _ := setupReconcilerTest(t)
	vm := newRunningVM()
	machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(vm.VMID))
	machineScope.InfraCluster.ProxmoxCluster.AddNodeLocation(infrav1.NodeLocation{
		Machine: corev1.LocalObjectReference{Name: machineScope.Name()},
		Node:    "node1",
	}, false)

	proxmoxClient.EXPECT().FindVMResource(context.TODO(), uint64(123)).Return(nil, fmt.Errorf("unable to find VM: %w", goproxmox.ErrVMNotListed)).Once()
	proxmoxClient.EXPECT().DeleteVM(context.TODO(), "node1", int64(123)).Return(nil, errors.New("vm does not exist: some reason")).Once()

	require.NoError(t, DeleteVM(context.TODO(), machineScope))
	require.Empty(t, machineScope.ProxmoxMachine.Finalizers)
	require.Empty(t, machineScope.InfraCluster.ProxmoxCluster.GetNode(machineScope.Name(), false))
}

// movedNode is where Proxmox HA moved the VM after CAPMOX recorded node1.
const movedNode = "node2"

func TestDeleteVM_DeletesOnTheNodeTheVMMovedTo(t *testing.T) {
	machineScope, proxmoxClient, _ := setupReconcilerTest(t)
	machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(123))
	machineScope.ProxmoxMachine.Status.ProxmoxNode = new("node1")
	machineScope.ProxmoxMachine.Finalizers = []string{infrav1.MachineFinalizer}
	moved := newVMResource()
	moved.Node = movedNode

	proxmoxClient.EXPECT().FindVMResource(context.TODO(), uint64(123)).Return(moved, nil).Once()
	proxmoxClient.EXPECT().DeleteVM(context.TODO(), movedNode, int64(123)).Return(&proxmox.Task{}, nil).Once()

	require.NoError(t, DeleteVM(context.TODO(), machineScope))
	require.Equal(t, movedNode, *machineScope.ProxmoxMachine.Status.ProxmoxNode)
	require.Equal(t, []string{infrav1.MachineFinalizer}, machineScope.ProxmoxMachine.Finalizers)
}

func TestDeleteVM_KeepsTheRecordedNodeWhenTheVMIDNamesAnotherVM(t *testing.T) {
	machineScope, proxmoxClient, _ := setupReconcilerTest(t)
	machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(123))
	machineScope.ProxmoxMachine.Status.ProxmoxNode = new("node1")
	other := newVMResource()
	other.Name = "other"
	other.Node = movedNode

	proxmoxClient.EXPECT().FindVMResource(context.TODO(), uint64(123)).Return(other, nil).Once()
	proxmoxClient.EXPECT().DeleteVM(context.TODO(), "node1", int64(123)).Return(&proxmox.Task{}, nil).Once()

	require.NoError(t, DeleteVM(context.TODO(), machineScope))
	require.Equal(t, "node1", *machineScope.ProxmoxMachine.Status.ProxmoxNode)
}

func TestDeleteVM_RetriesWhenTheVMCannotBeLocated(t *testing.T) {
	machineScope, proxmoxClient, _ := setupReconcilerTest(t)
	machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(123))
	machineScope.ProxmoxMachine.Finalizers = []string{infrav1.MachineFinalizer}

	proxmoxClient.EXPECT().FindVMResource(context.TODO(), uint64(123)).Return(nil, errors.New("cannot get cluster status")).Once()

	require.Error(t, DeleteVM(context.TODO(), machineScope))
	require.Equal(t, []string{infrav1.MachineFinalizer}, machineScope.ProxmoxMachine.Finalizers)
}
