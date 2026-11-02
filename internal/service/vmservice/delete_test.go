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
	"sigs.k8s.io/cluster-api/util/conditions"

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
	proxmoxClient.EXPECT().GetVM(context.TODO(), movedNode, int64(123)).Return(namedVM("test"), nil).Once()
	proxmoxClient.EXPECT().DeleteVM(context.TODO(), movedNode, int64(123)).Return(&proxmox.Task{}, nil).Once()

	require.NoError(t, DeleteVM(context.TODO(), machineScope))
	require.Equal(t, movedNode, *machineScope.ProxmoxMachine.Status.ProxmoxNode)
	require.Equal(t, []string{infrav1.MachineFinalizer}, machineScope.ProxmoxMachine.Finalizers)
}

func TestDeleteVM_LeavesAVMIDThatNamesAnotherVM(t *testing.T) {
	machineScope, proxmoxClient, _ := setupReconcilerTest(t)
	machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(123))
	machineScope.ProxmoxMachine.Status.ProxmoxNode = new("node1")
	machineScope.ProxmoxMachine.Finalizers = []string{infrav1.MachineFinalizer}
	other := newVMResource()
	other.Name = "other"
	other.Node = movedNode

	proxmoxClient.EXPECT().FindVMResource(context.TODO(), uint64(123)).Return(other, nil).Once()
	proxmoxClient.EXPECT().GetVM(context.TODO(), movedNode, int64(123)).Return(namedVM("other"), nil).Once()

	require.NoError(t, DeleteVM(context.TODO(), machineScope))
	require.Empty(t, machineScope.ProxmoxMachine.Finalizers)
}

func TestDeleteVM_LeavesAVMIDThatNamesAContainer(t *testing.T) {
	machineScope, proxmoxClient, _ := setupReconcilerTest(t)
	machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(123))
	machineScope.ProxmoxMachine.Finalizers = []string{infrav1.MachineFinalizer}
	container := newVMResource()
	container.Type = "lxc"
	container.Node = movedNode

	proxmoxClient.EXPECT().FindVMResource(context.TODO(), uint64(123)).Return(container, nil).Once()

	require.NoError(t, DeleteVM(context.TODO(), machineScope))
	require.Empty(t, machineScope.ProxmoxMachine.Finalizers)
}

func TestDeleteVM_ReleasesAnUnlistedVMIDInUseElsewhere(t *testing.T) {
	machineScope, proxmoxClient, _ := setupReconcilerTest(t)
	machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(123))
	machineScope.ProxmoxMachine.Finalizers = []string{infrav1.MachineFinalizer}

	proxmoxClient.EXPECT().FindVMResource(context.TODO(), uint64(123)).Return(nil, fmt.Errorf("unable to find VM: %w", goproxmox.ErrVMNotListed)).Once()
	proxmoxClient.EXPECT().DeleteVM(context.TODO(), "node1", int64(123)).Return(nil, fmt.Errorf("vm with id 123: %w node1: %w", goproxmox.ErrVMNotOnNode, errors.New("does not exist"))).Once()

	require.NoError(t, DeleteVM(context.TODO(), machineScope))
	require.Empty(t, machineScope.ProxmoxMachine.Finalizers)
}

func TestDeleteVM_RetriesWhenOurVMMovesDuringTheDelete(t *testing.T) {
	machineScope, proxmoxClient, _ := setupReconcilerTest(t)
	machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(123))
	machineScope.ProxmoxMachine.Finalizers = []string{infrav1.MachineFinalizer}
	listed := newVMResource()
	listed.Node = movedNode

	proxmoxClient.EXPECT().FindVMResource(context.TODO(), uint64(123)).Return(listed, nil).Once()
	proxmoxClient.EXPECT().GetVM(context.TODO(), movedNode, int64(123)).Return(namedVM("test"), nil).Once()
	proxmoxClient.EXPECT().DeleteVM(context.TODO(), movedNode, int64(123)).Return(nil, fmt.Errorf("vm with id 123: %w %s: %w", goproxmox.ErrVMNotOnNode, movedNode, errors.New("does not exist"))).Once()

	require.Error(t, DeleteVM(context.TODO(), machineScope))
	require.Equal(t, []string{infrav1.MachineFinalizer}, machineScope.ProxmoxMachine.Finalizers)
}

func TestDeleteVM_DecidesOwnershipByTheRecordedProviderID(t *testing.T) {
	const ours, theirs = "uuid=56603c36-46b9-4608-90ae-c731c15eae64", "uuid=0f8fad5b-d9cb-469f-a165-70867728950e"
	for _, tc := range []struct {
		smbios, name string
		deleted      bool
	}{
		{smbios: ours, name: "renamed-by-hand", deleted: true},
		{smbios: theirs, name: "", deleted: false},
		{smbios: theirs, name: "test", deleted: false},
	} {
		machineScope, proxmoxClient, _ := setupReconcilerTest(t)
		machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(123))
		machineScope.SetProviderID("56603c36-46b9-4608-90ae-c731c15eae64")
		machineScope.ProxmoxMachine.Finalizers = []string{infrav1.MachineFinalizer}
		listed := newVMResource()
		listed.Node = movedNode
		vm := namedVM(tc.name)
		vm.VirtualMachineConfig.SMBios1 = tc.smbios

		proxmoxClient.EXPECT().FindVMResource(context.TODO(), uint64(123)).Return(listed, nil).Once()
		proxmoxClient.EXPECT().GetVM(context.TODO(), movedNode, int64(123)).Return(vm, nil).Once()
		if tc.deleted {
			proxmoxClient.EXPECT().DeleteVM(context.TODO(), movedNode, int64(123)).Return(&proxmox.Task{}, nil).Once()
		}

		require.NoError(t, DeleteVM(context.TODO(), machineScope), tc.smbios)
		require.Equal(t, tc.deleted, len(machineScope.ProxmoxMachine.Finalizers) == 1, tc.smbios)
	}
}

func TestDeleteVM_TrustsTheVMConfigOverAStaleListingName(t *testing.T) {
	machineScope, proxmoxClient, _ := setupReconcilerTest(t)
	machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(123))
	machineScope.ProxmoxMachine.Status.ProxmoxNode = new("node1")
	machineScope.ProxmoxMachine.Finalizers = []string{infrav1.MachineFinalizer}
	stale := newVMResource()
	stale.Name = "deleted-earlier"
	stale.Node = movedNode

	proxmoxClient.EXPECT().FindVMResource(context.TODO(), uint64(123)).Return(stale, nil).Once()
	proxmoxClient.EXPECT().GetVM(context.TODO(), movedNode, int64(123)).Return(namedVM("test"), nil).Once()
	proxmoxClient.EXPECT().DeleteVM(context.TODO(), movedNode, int64(123)).Return(&proxmox.Task{}, nil).Once()

	require.NoError(t, DeleteVM(context.TODO(), machineScope))
	require.Equal(t, []string{infrav1.MachineFinalizer}, machineScope.ProxmoxMachine.Finalizers)
}

func TestDeleteVM_RetriesWhenTheListedVMCannotBeRead(t *testing.T) {
	machineScope, proxmoxClient, _ := setupReconcilerTest(t)
	machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(123))
	machineScope.ProxmoxMachine.Finalizers = []string{infrav1.MachineFinalizer}
	listed := newVMResource()
	listed.Node = movedNode

	proxmoxClient.EXPECT().FindVMResource(context.TODO(), uint64(123)).Return(listed, nil).Once()
	proxmoxClient.EXPECT().GetVM(context.TODO(), movedNode, int64(123)).Return(nil, errors.New("node unreachable")).Once()

	require.Error(t, DeleteVM(context.TODO(), machineScope))
	require.Equal(t, []string{infrav1.MachineFinalizer}, machineScope.ProxmoxMachine.Finalizers)
}

// namedVM is a VM whose config carries name.
func namedVM(name string) *proxmox.VirtualMachine {
	return &proxmox.VirtualMachine{VirtualMachineConfig: &proxmox.VirtualMachineConfig{Name: name}, VMID: 123}
}

func TestDeleteVM_DeletesAPlaceholderOrUnnamedVMWhereItIsListed(t *testing.T) {
	for _, name := range []string{"VM 123", ""} {
		machineScope, proxmoxClient, _ := setupReconcilerTest(t)
		machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(123))
		machineScope.ProxmoxMachine.Status.ProxmoxNode = new("node1")
		cloning := newVMResource()
		cloning.Name = name
		cloning.Node = movedNode

		proxmoxClient.EXPECT().FindVMResource(context.TODO(), uint64(123)).Return(cloning, nil).Once()
		proxmoxClient.EXPECT().GetVM(context.TODO(), movedNode, int64(123)).Return(namedVM(name), nil).Once()
		proxmoxClient.EXPECT().DeleteVM(context.TODO(), movedNode, int64(123)).Return(&proxmox.Task{}, nil).Once()

		require.NoError(t, DeleteVM(context.TODO(), machineScope), name)
		require.Equal(t, movedNode, *machineScope.ProxmoxMachine.Status.ProxmoxNode, name)
	}
}

func TestDeleteVM_WaitsForARunningVMToStop(t *testing.T) {
	machineScope, proxmoxClient, _ := setupReconcilerTest(t)
	machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(123))
	machineScope.ProxmoxMachine.Status.ProxmoxNode = new("node1")
	machineScope.ProxmoxMachine.Finalizers = []string{infrav1.MachineFinalizer}

	proxmoxClient.EXPECT().FindVMResource(context.TODO(), uint64(123)).Return(newVMResource(), nil).Once()
	proxmoxClient.EXPECT().GetVM(context.TODO(), "node1", int64(123)).Return(namedVM("test"), nil).Once()
	proxmoxClient.EXPECT().DeleteVM(context.TODO(), "node1", int64(123)).Return(&proxmox.Task{}, goproxmox.ErrVMStopping).Once()

	require.NoError(t, DeleteVM(context.TODO(), machineScope))
	require.Equal(t, []string{infrav1.MachineFinalizer}, machineScope.ProxmoxMachine.Finalizers)
	require.Nil(t, conditions.Get(machineScope.ProxmoxMachine, infrav1.ProxmoxMachineVirtualMachineProvisionedCondition))
}

func TestDeleteVM_KeepsTheRecordedNodeWhenTheListingNamesNone(t *testing.T) {
	machineScope, proxmoxClient, _ := setupReconcilerTest(t)
	machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(123))
	machineScope.ProxmoxMachine.Status.ProxmoxNode = new("node1")
	listed := newVMResource()
	listed.Node = ""

	proxmoxClient.EXPECT().FindVMResource(context.TODO(), uint64(123)).Return(listed, nil).Once()
	proxmoxClient.EXPECT().DeleteVM(context.TODO(), "node1", int64(123)).Return(&proxmox.Task{}, nil).Once()

	require.NoError(t, DeleteVM(context.TODO(), machineScope))
	require.Equal(t, "node1", *machineScope.ProxmoxMachine.Status.ProxmoxNode)
}

func TestDeleteVM_WithoutAVMIDLocatesNothing(t *testing.T) {
	machineScope, proxmoxClient, _ := setupReconcilerTest(t)

	proxmoxClient.EXPECT().DeleteVM(context.TODO(), "node1", int64(-1)).Return(nil, errors.New("vm with id -1 does not exist")).Once()

	require.NoError(t, DeleteVM(context.TODO(), machineScope))
	proxmoxClient.AssertNotCalled(t, "FindVMResource", context.TODO(), uint64(18446744073709551615))
}

func TestDeleteVM_RetriesWhenTheVMCannotBeLocated(t *testing.T) {
	machineScope, proxmoxClient, _ := setupReconcilerTest(t)
	machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(123))
	machineScope.ProxmoxMachine.Finalizers = []string{infrav1.MachineFinalizer}

	proxmoxClient.EXPECT().FindVMResource(context.TODO(), uint64(123)).Return(nil, errors.New("cannot get cluster status")).Once()

	require.Error(t, DeleteVM(context.TODO(), machineScope))
	require.Equal(t, []string{infrav1.MachineFinalizer}, machineScope.ProxmoxMachine.Finalizers)
	requireConditionIsFalse(t, machineScope.ProxmoxMachine, infrav1.ProxmoxMachineVirtualMachineProvisionedCondition)
}
