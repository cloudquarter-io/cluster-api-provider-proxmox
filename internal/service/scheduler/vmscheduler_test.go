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

package scheduler

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/kubernetes/ipam"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox/proxmoxtest"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/scope"
)

type fakeResourceClient map[string]uint64

func (c fakeResourceClient) GetReservableMemoryBytes(_ context.Context, nodeName string, _ int64) (uint64, error) {
	return c[nodeName], nil
}

func miBytes(in int32) uint64 {
	return uint64(in) * 1024 * 1024
}

func TestSelectNode(t *testing.T) {
	allowedNodes := []string{"pve1", "pve2", "pve3"}
	var locations []infrav1.NodeLocation
	var requestMiB = int32(8)
	availableMem := map[string]uint64{
		"pve1": miBytes(20),
		"pve2": miBytes(30),
		"pve3": miBytes(15),
	}

	expectedNodes := []string{
		// initial round-robin: everyone has enough memory
		"pve2", "pve1", "pve3",
		// second round-robin: pve3 out of memory
		"pve2", "pve1", "pve2",
	}

	for i, expectedNode := range expectedNodes {
		t.Run(fmt.Sprintf("round %d", i+1), func(t *testing.T) {
			proxmoxMachine := &infrav1.ProxmoxMachine{
				Spec: infrav1.ProxmoxMachineSpec{
					MemoryMiB: &requestMiB,
				},
			}

			client := fakeResourceClient(availableMem)

			node, err := selectNode(context.Background(), client, proxmoxMachine, locations, allowedNodes, &infrav1.SchedulerHints{}, false)
			require.NoError(t, err)
			require.Equal(t, expectedNode, node)

			require.Greater(t, availableMem[node], miBytes(requestMiB))
			availableMem[node] -= miBytes(requestMiB)

			locations = append(locations, infrav1.NodeLocation{Node: node})
		})
	}

	t.Run("out of memory", func(t *testing.T) {
		proxmoxMachine := &infrav1.ProxmoxMachine{
			Spec: infrav1.ProxmoxMachineSpec{
				MemoryMiB: &requestMiB,
			},
		}

		client := fakeResourceClient(availableMem)

		node, err := selectNode(context.Background(), client, proxmoxMachine, locations, allowedNodes, &infrav1.SchedulerHints{}, false)
		require.ErrorAs(t, err, &InsufficientMemoryError{})
		require.Empty(t, node)

		expectMem := map[string]uint64{
			"pve1": miBytes(4), // 20 - 8 x 2
			"pve2": miBytes(6), // 30 - 8 x 3
			"pve3": miBytes(7), // 15 - 8 x 1
		}
		require.Equal(t, expectMem, availableMem)
	})
}

func TestSelectNodeEvenlySpread(t *testing.T) {
	// Verify that VMs are scheduled evenly across nodes when memory allows
	allowedNodes := []string{"pve1", "pve2", "pve3"}
	var locations []infrav1.NodeLocation
	var requestMiB = int32(8)
	availableMem := map[string]uint64{
		"pve1": miBytes(25), // enough for 3 VMs
		"pve2": miBytes(35), // enough for 4 VMs
		"pve3": miBytes(15), // enough for 1 VM
	}

	expectedNodes := []string{
		// initial round-robin: everyone has enough memory
		"pve2", "pve1", "pve3",
		// second round-robin: pve3 out of memory
		"pve2", "pve1", "pve2",
		// third round-robin: pve1 and pve2 has room for one more VM each
		"pve1", "pve2",
	}

	for i, expectedNode := range expectedNodes {
		t.Run(fmt.Sprintf("round %d", i+1), func(t *testing.T) {
			proxmoxMachine := &infrav1.ProxmoxMachine{
				Spec: infrav1.ProxmoxMachineSpec{
					MemoryMiB: &requestMiB,
				},
			}

			client := fakeResourceClient(availableMem)

			node, err := selectNode(context.Background(), client, proxmoxMachine, locations, allowedNodes, &infrav1.SchedulerHints{}, false)
			require.NoError(t, err)
			require.Equal(t, expectedNode, node)

			require.Greater(t, availableMem[node], miBytes(requestMiB))
			availableMem[node] -= miBytes(requestMiB)

			locations = append(locations, infrav1.NodeLocation{Node: node})
		})
	}

	t.Run("out of memory", func(t *testing.T) {
		proxmoxMachine := &infrav1.ProxmoxMachine{
			Spec: infrav1.ProxmoxMachineSpec{
				MemoryMiB: &requestMiB,
			},
		}

		client := fakeResourceClient(availableMem)

		node, err := selectNode(context.Background(), client, proxmoxMachine, locations, allowedNodes, &infrav1.SchedulerHints{}, false)
		require.ErrorAs(t, err, &InsufficientMemoryError{})
		require.Empty(t, node)

		expectMem := map[string]uint64{
			"pve1": miBytes(1), // 25 - 8 x 3
			"pve2": miBytes(3), // 35 - 8 x 4
			"pve3": miBytes(7), // 15 - 8 x 1
		}
		require.Equal(t, expectMem, availableMem)
	})
}

func TestSelectNodeControlPlaneNeverSharesANode(t *testing.T) {
	allowedNodes := []string{"pve1", "pve2", "pve3"}
	requestMiB := int32(8)
	machine := &infrav1.ProxmoxMachine{Spec: infrav1.ProxmoxMachineSpec{MemoryMiB: &requestMiB}}
	at := func(nodes ...string) []infrav1.NodeLocation {
		locations := make([]infrav1.NodeLocation, 0, len(nodes))
		for _, n := range nodes {
			locations = append(locations, infrav1.NodeLocation{Node: n})
		}
		return locations
	}

	t.Run("the only free node lacks room: wait, never share", func(t *testing.T) {
		client := fakeResourceClient{"pve1": miBytes(20), "pve2": miBytes(4), "pve3": miBytes(20)}
		locations := at("pve1", "pve3")

		node, err := selectNode(context.Background(), client, machine, locations, allowedNodes, &infrav1.SchedulerHints{}, true)
		require.Equal(t, NoFreeNodeError{node: "pve2", available: miBytes(4), requested: miBytes(requestMiB)}, err)
		require.Empty(t, node)

		// A worker with the same inputs still lands where memory allows.
		node, err = selectNode(context.Background(), client, machine, locations, allowedNodes, &infrav1.SchedulerHints{}, false)
		require.NoError(t, err)
		require.Contains(t, []string{"pve1", "pve3"}, node)
	})

	t.Run("several free nodes: the one with the most memory", func(t *testing.T) {
		client := fakeResourceClient{"pve1": miBytes(40), "pve2": miBytes(10), "pve3": miBytes(12)}

		node, err := selectNode(context.Background(), client, machine, at("pve1"), allowedNodes, &infrav1.SchedulerHints{}, true)
		require.NoError(t, err)
		require.Equal(t, "pve3", node)
	})

	t.Run("every node holds one: the fewest with room", func(t *testing.T) {
		client := fakeResourceClient{"pve1": miBytes(20), "pve2": miBytes(30), "pve3": miBytes(25)}

		node, err := selectNode(context.Background(), client, machine, at("pve1", "pve2", "pve2", "pve3", "pve3"), allowedNodes, &infrav1.SchedulerHints{}, true)
		require.NoError(t, err)
		require.Equal(t, "pve1", node)
	})
}

func TestControlPlanesCountOnTheirOwnProxmoxCluster(t *testing.T) {
	ref := func(namespace, name string) *corev1.SecretReference {
		return &corev1.SecretReference{Namespace: namespace, Name: name}
	}
	cluster := &infrav1.ProxmoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "tenant"},
		Spec: infrav1.ProxmoxClusterSpec{
			CredentialsRef: ref("", "own"),
			ZoneConfigs: []infrav1.ZoneConfigSpec{
				{Zone: new("a1"), CredentialsRef: ref("capmox-system", "pve-a")},
				{Zone: new("a2"), CredentialsRef: ref("capmox-system", "pve-a")},
				{Zone: new("b"), CredentialsRef: ref("capmox-system", "pve-b")},
				{Zone: new("plain")},
				{Zone: new("own"), CredentialsRef: ref("tenant", "own")},
			},
		},
	}
	locations := []infrav1.NodeLocation{
		{Machine: corev1.LocalObjectReference{Name: "in-a1"}, Node: "pve1", Zone: new("a1")},
		{Machine: corev1.LocalObjectReference{Name: "in-a2"}, Node: "pve2", Zone: new("a2")},
		{Machine: corev1.LocalObjectReference{Name: "in-b"}, Node: "pve1", Zone: new("b")},
		{Machine: corev1.LocalObjectReference{Name: "in-plain"}, Node: "pve3", Zone: new("plain")},
		{Machine: corev1.LocalObjectReference{Name: "zoneless"}, Node: "pve3"},
		{Machine: corev1.LocalObjectReference{Name: "in-own"}, Node: "pve4", Zone: new("own")},
	}
	names := func(locs []infrav1.NodeLocation) []string {
		out := make([]string, 0, len(locs))
		for _, l := range locs {
			out = append(out, l.Machine.Name)
		}
		return out
	}

	// Zones sharing a Secret share a Proxmox cluster; another Secret is another one.
	require.Equal(t, []string{"in-a1", "in-a2"}, names(onEndpoint(cluster, "a2", locations)))
	require.Equal(t, []string{"in-b"}, names(onEndpoint(cluster, "b", locations)))
	// A zone without a Secret, no zone, and a zone naming the ProxmoxCluster's
	// own Secret all reach the ProxmoxCluster's Proxmox cluster.
	require.Equal(t, []string{"in-plain", "zoneless", "in-own"}, names(onEndpoint(cluster, "", locations)))
	require.Equal(t, []string{"in-plain", "zoneless", "in-own"}, names(onEndpoint(cluster, "own", locations)))
}

func TestScheduleVM(t *testing.T) {
	ctrlClient := setupClient()
	require.NotNil(t, ctrlClient)

	ipamHelper := &ipam.Helper{}

	proxmoxCluster := infrav1.ProxmoxCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bar",
		},
		Spec: infrav1.ProxmoxClusterSpec{
			AllowedNodes: []string{"pve1", "pve2", "pve3"},
		},
		Status: infrav1.ProxmoxClusterStatus{
			NodeLocations: &infrav1.NodeLocations{
				ControlPlane: []infrav1.NodeLocation{},
				Workers: []infrav1.NodeLocation{
					{
						Node: "pve1",
						Machine: corev1.LocalObjectReference{
							Name: "foo-machine",
						},
					},
				},
			},
		},
	}

	err := ctrlClient.Create(context.Background(), &proxmoxCluster)
	require.NoError(t, err)

	proxmoxMachine := &infrav1.ProxmoxMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo-machine",
			Labels: map[string]string{
				"cluster.x-k8s.io/cluster-name": "bar",
			},
		},
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB: new(int32(10)),
		},
	}

	fakeProxmoxClient := proxmoxtest.NewMockClient(t)

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bar",
			Namespace: "default",
		},
	}
	machineScope, err := scope.NewMachineScope(scope.MachineScopeParams{
		Client: ctrlClient,
		Machine: &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "foo-machine",
				Namespace: "default",
			},
		},
		Cluster: cluster,
		InfraCluster: &scope.ClusterScope{
			Cluster:        cluster,
			ProxmoxCluster: &proxmoxCluster,
			ProxmoxClient:  fakeProxmoxClient,
		},
		ProxmoxMachine: proxmoxMachine,
		IPAMHelper:     ipamHelper,
	})
	require.NoError(t, err)

	fakeProxmoxClient.EXPECT().GetReservableMemoryBytes(context.Background(), "pve1", int64(100)).Return(miBytes(60), nil)
	fakeProxmoxClient.EXPECT().GetReservableMemoryBytes(context.Background(), "pve2", int64(100)).Return(miBytes(20), nil)
	fakeProxmoxClient.EXPECT().GetReservableMemoryBytes(context.Background(), "pve3", int64(100)).Return(miBytes(20), nil)

	node, err := ScheduleVM(context.Background(), machineScope)
	require.NoError(t, err)
	require.Equal(t, "pve2", node)
}

func TestScheduleVMWithFailureDomainNodes(t *testing.T) {
	ctrlClient := setupClient()

	proxmoxCluster := infrav1.ProxmoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "bar"},
		Spec: infrav1.ProxmoxClusterSpec{
			AllowedNodes: []string{"pve1", "pve2", "pve3"},
			ZoneConfigs: []infrav1.ZoneConfigSpec{
				{
					Zone:  new("zone-a"),
					Nodes: []string{"pve3"},
				},
			},
		},
		Status: infrav1.ProxmoxClusterStatus{
			NodeLocations: &infrav1.NodeLocations{
				Workers: []infrav1.NodeLocation{},
			},
		},
	}
	require.NoError(t, ctrlClient.Create(context.Background(), &proxmoxCluster))

	proxmoxMachine := &infrav1.ProxmoxMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "foo-machine",
			Labels: map[string]string{"cluster.x-k8s.io/cluster-name": "bar"},
		},
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB: new(int32(10)),
		},
	}

	fakeProxmoxClient := proxmoxtest.NewMockClient(t)
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "bar", Namespace: "default"},
	}
	machineScope, err := scope.NewMachineScope(scope.MachineScopeParams{
		Client: ctrlClient,
		Machine: &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "foo-machine", Namespace: "default"},
			Spec:       clusterv1.MachineSpec{FailureDomain: "zone-a"},
		},
		Cluster: cluster,
		InfraCluster: &scope.ClusterScope{
			Cluster:        cluster,
			ProxmoxCluster: &proxmoxCluster,
			ProxmoxClient:  fakeProxmoxClient,
		},
		ProxmoxMachine: proxmoxMachine,
		IPAMHelper:     &ipam.Helper{},
	})
	require.NoError(t, err)
	require.NoError(t, machineScope.FailureDomainError())

	// The failure domain restricts placement to pve3: only pve3 may be queried.
	fakeProxmoxClient.EXPECT().GetReservableMemoryBytes(context.Background(), "pve3", int64(100)).Return(miBytes(60), nil)

	node, err := ScheduleVM(context.Background(), machineScope)
	require.NoError(t, err)
	require.Equal(t, "pve3", node)
}

func TestScheduleVMControlPlaneIgnoresAnotherProxmoxCluster(t *testing.T) {
	ctrlClient := setupClient()

	// Zone b is reached with the ProxmoxCluster's client; zone a is another
	// Proxmox cluster whose nodes carry the same names.
	proxmoxCluster := infrav1.ProxmoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "bar"},
		Spec: infrav1.ProxmoxClusterSpec{
			ZoneConfigs: []infrav1.ZoneConfigSpec{
				{Zone: new("zone-a"), Nodes: []string{"pve1", "pve2"}, CredentialsRef: &corev1.SecretReference{Namespace: "capmox-system", Name: "pve-a"}},
				{Zone: new("zone-b"), Nodes: []string{"pve1", "pve2"}},
			},
		},
		Status: infrav1.ProxmoxClusterStatus{
			NodeLocations: &infrav1.NodeLocations{
				ControlPlane: []infrav1.NodeLocation{
					{Machine: corev1.LocalObjectReference{Name: "cp-in-a"}, Node: "pve1", Zone: new("zone-a")},
				},
			},
		},
	}
	require.NoError(t, ctrlClient.Create(context.Background(), &proxmoxCluster))

	proxmoxMachine := &infrav1.ProxmoxMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "cp-in-b",
			Labels: map[string]string{"cluster.x-k8s.io/cluster-name": "bar"},
		},
		Spec: infrav1.ProxmoxMachineSpec{MemoryMiB: new(int32(10))},
	}

	fakeProxmoxClient := proxmoxtest.NewMockClient(t)
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "bar", Namespace: "default"}}
	machineScope, err := scope.NewMachineScope(scope.MachineScopeParams{
		Client: ctrlClient,
		Machine: &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cp-in-b",
				Namespace: "default",
				Labels:    map[string]string{clusterv1.MachineControlPlaneLabel: ""},
			},
			Spec: clusterv1.MachineSpec{FailureDomain: "zone-b"},
		},
		Cluster: cluster,
		InfraCluster: &scope.ClusterScope{
			Cluster:        cluster,
			ProxmoxCluster: &proxmoxCluster,
			ProxmoxClient:  fakeProxmoxClient,
		},
		ProxmoxMachine: proxmoxMachine,
		IPAMHelper:     &ipam.Helper{},
	})
	require.NoError(t, err)

	// Only zone b's pve1 has room. zone a's control plane on its own pve1 does
	// not make zone b's pve1 hold one.
	fakeProxmoxClient.EXPECT().GetReservableMemoryBytes(context.Background(), "pve1", int64(100)).Return(miBytes(60), nil)
	fakeProxmoxClient.EXPECT().GetReservableMemoryBytes(context.Background(), "pve2", int64(100)).Return(miBytes(5), nil)

	node, err := ScheduleVM(context.Background(), machineScope)
	require.NoError(t, err)
	require.Equal(t, "pve1", node)
}

func TestInsufficientMemoryError_Error(t *testing.T) {
	err := InsufficientMemoryError{
		node:      "pve1",
		available: 10,
		requested: 20,
	}
	require.Equal(t, "cannot reserve 20B of memory on node pve1: 10B available memory left", err.Error())
}

func setupClient() client.Client {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(infrav1.AddToScheme(scheme))
	utilruntime.Must(infrav1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	return fakeClient
}
