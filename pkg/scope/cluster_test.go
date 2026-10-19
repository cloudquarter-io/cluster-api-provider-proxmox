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

package scope

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/kubernetes/ipam"
	capmoxClient "github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox/goproxmox"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox/proxmoxtest"
)

func TestNewClusterScope_MissingParams(t *testing.T) {
	k8sClient := fake.NewClientBuilder().Build()

	tests := []struct {
		name   string
		params ClusterScopeParams
	}{
		{"missing client", ClusterScopeParams{Cluster: &clusterv1.Cluster{}, ProxmoxCluster: &infrav1.ProxmoxCluster{}, ProxmoxClient: &goproxmox.APIClient{}, IPAMHelper: &ipam.Helper{}}},
		{"missing cluster", ClusterScopeParams{Client: k8sClient, ProxmoxCluster: &infrav1.ProxmoxCluster{}, ProxmoxClient: &goproxmox.APIClient{}, IPAMHelper: &ipam.Helper{}}},
		{"missing proxmox cluster", ClusterScopeParams{Client: k8sClient, Cluster: &clusterv1.Cluster{}, ProxmoxClient: &goproxmox.APIClient{}, IPAMHelper: &ipam.Helper{}}},
		{"missing ipam helper", ClusterScopeParams{Client: k8sClient, Cluster: &clusterv1.Cluster{}, ProxmoxCluster: &infrav1.ProxmoxCluster{}, ProxmoxClient: &goproxmox.APIClient{}}},
		{"missing proxmox client", ClusterScopeParams{Client: k8sClient, Cluster: &clusterv1.Cluster{}, ProxmoxCluster: &infrav1.ProxmoxCluster{}, IPAMHelper: &ipam.Helper{}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewClusterScope(test.params)
			require.Error(t, err)
		})
	}
}

func TestNewClusterScope_MissingProxmoxClient(t *testing.T) {
	k8sClient := getFakeClient(t)

	proxmoxCluster := &infrav1.ProxmoxCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: infrav1.GroupVersion.String(),
			Kind:       "ProxmoxCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "proxmoxcluster",
			Namespace: "default",
		},
		Spec: infrav1.ProxmoxClusterSpec{
			AllowedNodes: []string{"pve", "pve-2"},
		},
	}

	tests := []struct {
		name   string
		params ClusterScopeParams
	}{
		{"missing proxmox client in ref", ClusterScopeParams{Client: k8sClient, Cluster: &clusterv1.Cluster{}, ProxmoxCluster: proxmoxCluster, IPAMHelper: &ipam.Helper{}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewClusterScope(test.params)
			require.Error(t, err)
			cond := conditions.Get(proxmoxCluster, infrav1.ProxmoxClusterProxmoxAvailableCondition)
			require.NotNil(t, cond)
			require.Equal(t, metav1.ConditionFalse, cond.Status)
		})
	}
}

func TestNewClusterScope_SetupProxmoxClient(t *testing.T) {
	k8sClient := getFakeClient(t)

	proxmoxCluster := &infrav1.ProxmoxCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: infrav1.GroupVersion.String(),
			Kind:       "ProxmoxCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "proxmoxcluster",
			Namespace: "default",
		},
		Spec: infrav1.ProxmoxClusterSpec{
			AllowedNodes: []string{"pve", "pve-2"},
			CredentialsRef: &corev1.SecretReference{
				Name:      "test-secret",
				Namespace: "default",
			},
		},
	}

	params := ClusterScopeParams{Client: k8sClient, Cluster: &clusterv1.Cluster{}, ProxmoxCluster: proxmoxCluster, IPAMHelper: &ipam.Helper{}}
	_, err := NewClusterScope(params)
	require.Error(t, err)

	creds := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "default",
		},
		StringData: map[string]string{
			"url":    "https://localhost:8006",
			"token":  "test-token",
			"secret": "test-secret",
		},
	}

	err = k8sClient.Create(context.Background(), &creds)
	require.NoError(t, err)

	_, err = NewClusterScope(params)
	require.Error(t, err)
}

func TestNewClusterScope_NoDefaultClientWhenEveryZoneHasCredentials(t *testing.T) {
	k8sClient := getFakeClient(t)

	proxmoxCluster := &infrav1.ProxmoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "proxmoxcluster", Namespace: "default"},
		Spec: infrav1.ProxmoxClusterSpec{
			ZoneConfigs: []infrav1.ZoneConfigSpec{
				{Zone: new("dc-a"), CredentialsRef: &corev1.SecretReference{Name: "pve-a"}},
				{Zone: new("dc-b"), CredentialsRef: &corev1.SecretReference{Name: "pve-b"}},
			},
		},
	}

	// No controller credentials, no spec.credentialsRef: the cluster needs no
	// default client because every zone reaches its own endpoint.
	params := ClusterScopeParams{Client: k8sClient, Cluster: &clusterv1.Cluster{}, ProxmoxCluster: proxmoxCluster, IPAMHelper: &ipam.Helper{}}
	clusterScope, err := NewClusterScope(params)
	require.NoError(t, err)
	require.Nil(t, clusterScope.ProxmoxClient)
	require.Nil(t, conditions.Get(proxmoxCluster, infrav1.ProxmoxClusterProxmoxAvailableCondition))

	// One zone without its own credentials brings the default client back,
	// and its absence fails the cluster as before.
	proxmoxCluster.Spec.ZoneConfigs[1].CredentialsRef = nil
	_, err = NewClusterScope(params)
	require.Error(t, err)
	cond := conditions.Get(proxmoxCluster, infrav1.ProxmoxClusterProxmoxAvailableCondition)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionFalse, cond.Status)
}

func TestClusterScope_ProxmoxClientForZone(t *testing.T) {
	k8sClient := getFakeClient(t)
	defaultClient := proxmoxtest.NewMockClient(t)
	zoneClient := proxmoxtest.NewMockClient(t)

	proxmoxCluster := &infrav1.ProxmoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "proxmoxcluster", Namespace: "default"},
		Spec: infrav1.ProxmoxClusterSpec{
			ZoneConfigs: []infrav1.ZoneConfigSpec{
				{Zone: new("shared")},
				{Zone: new("dc-a"), CredentialsRef: &corev1.SecretReference{Name: "pve-a"}},
			},
		},
	}
	clusterScope := &ClusterScope{client: k8sClient, ProxmoxCluster: proxmoxCluster, ProxmoxClient: defaultClient}

	t.Run("no zone and a zone without credentials share the default client", func(t *testing.T) {
		for _, zone := range []string{"", "shared"} {
			got, err := clusterScope.ProxmoxClientForZone(context.Background(), zone)
			require.NoError(t, err, zone)
			require.Same(t, defaultClient, got, zone)
		}
	})

	t.Run("a zone whose credentials Secret is missing fails and reports it", func(t *testing.T) {
		_, err := clusterScope.ProxmoxClientForZone(context.Background(), "dc-a")
		require.Error(t, err)
		require.Contains(t, err.Error(), `zone "dc-a"`)
		cond := conditions.Get(proxmoxCluster, infrav1.ProxmoxClusterProxmoxAvailableCondition)
		require.NotNil(t, cond)
		require.Contains(t, cond.Message, "default/pve-a")
	})

	t.Run("a zone with credentials gets a client built from them", func(t *testing.T) {
		var built *corev1.SecretReference
		clusterScope.newClient = func(_ context.Context, ref *corev1.SecretReference) (capmoxClient.Client, error) {
			built = ref
			return zoneClient, nil
		}
		defer func() { clusterScope.newClient = nil }()
		got, err := clusterScope.ProxmoxClientForZone(context.Background(), "dc-a")
		require.NoError(t, err)
		require.Same(t, zoneClient, got)
		require.Equal(t, "pve-a", built.Name)
	})

	t.Run("an undeclared zone is refused once any zone has its own credentials", func(t *testing.T) {
		_, err := clusterScope.ProxmoxClientForZone(context.Background(), "gone")
		require.Error(t, err)
		require.Contains(t, err.Error(), `zone "gone" is not declared`)
	})

	t.Run("an undeclared zone shares the default client when no zone has credentials", func(t *testing.T) {
		sharedOnly := &ClusterScope{ProxmoxClient: defaultClient, ProxmoxCluster: &infrav1.ProxmoxCluster{
			Spec: infrav1.ProxmoxClusterSpec{ZoneConfigs: []infrav1.ZoneConfigSpec{{Zone: new("shared")}}},
		}}
		got, err := sharedOnly.ProxmoxClientForZone(context.Background(), "gone")
		require.NoError(t, err)
		require.Same(t, defaultClient, got)
	})

	t.Run("without a default client a machine outside every credentialed zone is refused", func(t *testing.T) {
		allOwn := &ClusterScope{client: k8sClient, ProxmoxCluster: &infrav1.ProxmoxCluster{
			Spec: infrav1.ProxmoxClusterSpec{
				ZoneConfigs: []infrav1.ZoneConfigSpec{
					{Zone: new("dc-a"), CredentialsRef: &corev1.SecretReference{Name: "pve-a"}},
				},
			},
		}}
		_, err := allOwn.ProxmoxClientForZone(context.Background(), "")
		require.Error(t, err)
		require.Contains(t, err.Error(), "placed in none of them")
	})
}

func TestListProxmoxMachinesForCluster(t *testing.T) {
	k8sClient := getFakeClient(t)
	proxmoxClient := proxmoxtest.NewMockClient(t)

	cluster := &clusterv1.Cluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Cluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "proxmoxcluster",
			Namespace: "default",
		},
	}
	err := k8sClient.Create(context.Background(), cluster)
	require.NoError(t, err)

	proxmoxCluster := &infrav1.ProxmoxCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: infrav1.GroupVersion.String(),
			Kind:       "ProxmoxCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "proxmoxcluster",
			Namespace: "default",
		},
		Spec: infrav1.ProxmoxClusterSpec{
			AllowedNodes: []string{"pve", "pve-2"},
			CredentialsRef: &corev1.SecretReference{
				Name:      "test-secret",
				Namespace: "default",
			},
		},
	}
	err = k8sClient.Create(context.Background(), proxmoxCluster)
	require.NoError(t, err)

	creds := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "default",
		},
		StringData: map[string]string{
			"url":    "https://localhost:8006",
			"token":  "test-token",
			"secret": "test-secret",
		},
	}

	err = k8sClient.Create(context.Background(), &creds)
	require.NoError(t, err)

	params := ClusterScopeParams{Client: k8sClient, Cluster: cluster, ProxmoxCluster: proxmoxCluster, ProxmoxClient: proxmoxClient, IPAMHelper: &ipam.Helper{}}
	clusterScope, err := NewClusterScope(params)
	require.NoError(t, err)

	expectedMachineList := &infrav1.ProxmoxMachineList{
		Items: []infrav1.ProxmoxMachine{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "machine01",
					Namespace: "default",
					Labels: map[string]string{
						clusterv1.ClusterNameLabel: clusterScope.Name(),
					},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "machine02",
					Namespace: "default",
					Labels: map[string]string{
						clusterv1.ClusterNameLabel: clusterScope.Name(),
					},
				},
			},
		},
	}

	for machineIdx := range expectedMachineList.Items {
		err = k8sClient.Create(context.Background(), &expectedMachineList.Items[machineIdx])
		require.NoError(t, err)
		// As the k8sClient sets ResourceVersion to 1, we also set it in the expectedMachineList.
		expectedMachineList.Items[machineIdx].ResourceVersion = "1"
	}

	unexpectedMachine := &infrav1.ProxmoxMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-cluster-machine01",
			Namespace: "default",
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: "other-cluster",
			},
		},
	}
	err = k8sClient.Create(context.Background(), unexpectedMachine)
	require.NoError(t, err)

	machines, err := clusterScope.ListProxmoxMachinesForCluster(context.Background())

	require.NoError(t, err)
	require.Equal(t, expectedMachineList.Items, machines)
}

func getFakeClient(t *testing.T) ctrlclient.Client {
	scheme := runtime.NewScheme()

	// Register client-go scheme with the scheme
	err := clientgoscheme.AddToScheme(scheme)
	require.NoError(t, err)
	err = clusterv1.AddToScheme(scheme)
	require.NoError(t, err)
	err = infrav1.AddToScheme(scheme)
	require.NoError(t, err)

	return fake.NewClientBuilder().WithScheme(scheme).Build()
}
