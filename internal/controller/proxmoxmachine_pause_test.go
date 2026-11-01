/*
Copyright 2026 IONOS Cloud.

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

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/kubernetes/ipam"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox/proxmoxtest"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/scope"
)

// readyName names the Cluster, Machine, ProxmoxCluster, ProxmoxMachine and
// virtual machine of readyMachine.
const readyName = "test"

// readyMachine builds the scopes of a provisioned ProxmoxMachine whose virtual
// machine is vmid 123 on node1.
func readyMachine(t *testing.T) (*scope.MachineScope, *scope.ClusterScope, *proxmoxtest.MockClient) {
	t.Helper()
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: readyName, Namespace: metav1.NamespaceDefault},
		Status: clusterv1.ClusterStatus{
			Initialization: clusterv1.ClusterInitializationStatus{InfrastructureProvisioned: new(true)},
		},
	}
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: readyName, Namespace: metav1.NamespaceDefault,
			Labels: map[string]string{clusterv1.ClusterNameLabel: readyName},
		},
		Spec: clusterv1.MachineSpec{Bootstrap: clusterv1.Bootstrap{DataSecretName: new("bootstrap")}},
	}
	infraCluster := &infrav1.ProxmoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: readyName, Namespace: metav1.NamespaceDefault},
		Status:     infrav1.ProxmoxClusterStatus{NodeLocations: &infrav1.NodeLocations{}},
	}
	infraMachine := &infrav1.ProxmoxMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name: readyName, Namespace: metav1.NamespaceDefault,
			Finalizers: []string{infrav1.MachineFinalizer},
			Labels:     map[string]string{clusterv1.ClusterNameLabel: readyName},
		},
		Spec: infrav1.ProxmoxMachineSpec{
			VirtualMachineID: new(int64(123)),
			VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
				TemplateSource: infrav1.TemplateSource{SourceNode: new("node1"), TemplateID: new(int32(100))},
			},
		},
	}
	conditions.Set(infraMachine, metav1.Condition{
		Type:   infrav1.ProxmoxMachineVirtualMachineProvisionedCondition,
		Status: metav1.ConditionTrue,
		Reason: clusterv1.ProvisionedReason,
	})

	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, clusterv1.AddToScheme(s))
	require.NoError(t, ipamv1.AddToScheme(s))
	require.NoError(t, infrav1.AddToScheme(s))
	kubeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cluster, machine, infraCluster, infraMachine).
		WithStatusSubresource(&infrav1.ProxmoxCluster{}, &infrav1.ProxmoxMachine{}).
		Build()

	logger := logr.Discard()
	mockClient := proxmoxtest.NewMockClient(t)
	ipamHelper := ipam.NewHelper(kubeClient, infraCluster)
	clusterScope, err := scope.NewClusterScope(scope.ClusterScopeParams{
		Client: kubeClient, Logger: &logger, Cluster: cluster, ProxmoxCluster: infraCluster,
		ProxmoxClient: mockClient, IPAMHelper: ipamHelper,
	})
	require.NoError(t, err)
	machineScope, err := scope.NewMachineScope(scope.MachineScopeParams{
		Client: kubeClient, Logger: &logger, Cluster: cluster, Machine: machine,
		InfraCluster: clusterScope, ProxmoxMachine: infraMachine, IPAMHelper: ipamHelper,
	})
	require.NoError(t, err)
	return machineScope, clusterScope, mockClient
}

func readyVM(qmpStatus string) *proxmox.VirtualMachine {
	return &proxmox.VirtualMachine{
		VirtualMachineConfig: &proxmox.VirtualMachineConfig{Name: readyName},
		Name:                 readyName,
		Node:                 "node1",
		VMID:                 123,
		Status:               proxmox.StatusVirtualMachineRunning,
		QMPStatus:            qmpStatus,
	}
}

func TestReconcileNormal_ReadyMachineIsCheckedAgain(t *testing.T) {
	machineScope, clusterScope, mockClient := readyMachine(t)
	mockClient.EXPECT().GetVM(context.Background(), "node1", int64(123)).Return(readyVM(proxmox.StatusVirtualMachineRunning), nil).Once()

	r := &ProxmoxMachineReconciler{VMPauseCheckInterval: 42 * time.Second}
	result, err := r.reconcileNormal(context.Background(), machineScope, clusterScope)
	require.NoError(t, err)
	require.Equal(t, 42*time.Second, result.RequeueAfter)
	require.False(t, conditions.Has(machineScope.ProxmoxMachine, infrav1.ProxmoxMachineVirtualMachinePausedCondition))
}

func TestReconcileNormal_ResumesAReadyMachinePausedOnIOError(t *testing.T) {
	machineScope, clusterScope, mockClient := readyMachine(t)
	vm := readyVM("io-error")
	mockClient.EXPECT().GetVM(context.Background(), "node1", int64(123)).Return(vm, nil).Once()
	mockClient.EXPECT().ResumeVM(context.Background(), vm).Return(&proxmox.Task{UPID: "resume"}, nil).Once()

	r := &ProxmoxMachineReconciler{VMPauseCheckInterval: 42 * time.Second}
	result, err := r.reconcileNormal(context.Background(), machineScope, clusterScope)
	require.NoError(t, err)
	require.Equal(t, 42*time.Second, result.RequeueAfter)
	require.True(t, conditions.IsTrue(machineScope.ProxmoxMachine, infrav1.ProxmoxMachineVirtualMachinePausedCondition))
	require.True(t, conditions.IsTrue(machineScope.ProxmoxMachine, infrav1.ProxmoxMachineVirtualMachineProvisionedCondition))
}
