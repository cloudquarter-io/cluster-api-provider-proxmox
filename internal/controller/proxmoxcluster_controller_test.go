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

package controller

import (
	"context"
	"reflect"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ipamicv1 "sigs.k8s.io/cluster-api-ipam-provider-in-cluster/api/v1alpha2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/kubernetes/ipam"
)

var (
	clusterName   = "test-cluster"
	testFinalizer = "cluster-test.cluster.x-k8s.io"
)

var _ = Describe("Controller Test", func() {
	g := NewWithT(GinkgoT())

	BeforeEach(func() {
		gvk := infrav1.GroupVersion.WithKind(reflect.TypeFor[infrav1.ProxmoxCluster]().Name())

		cl := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: testNS,
				UID:       "1000",
			},
			Spec: clusterv1.ClusterSpec{
				Paused: new(false),
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					Kind:     gvk.Kind,
					Name:     clusterName,
					APIGroup: gvk.Group,
				},
			},
		}

		ctrlutil.AddFinalizer(cl, testFinalizer)
		g.Expect(k8sClient.Create(context.Background(), cl)).To(Succeed())
	})

	AfterEach(func() {
		var cl clusterv1.Cluster
		g.Expect(k8sClient.Get(testEnv.GetContext(), client.ObjectKey{Name: "test", Namespace: testNS}, &cl)).To(Succeed())
		ctrlutil.RemoveFinalizer(&cl, testFinalizer)
		g.Expect(k8sClient.Update(testEnv.GetContext(), &cl)).To(Succeed())

		g.Eventually(func(g Gomega) {
			err := k8sClient.Get(testEnv.GetContext(), client.ObjectKey{Name: "test", Namespace: testNS}, &clusterv1.Cluster{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}).WithTimeout(time.Second * 10).
			WithPolling(time.Second).
			Should(Succeed())
	})

	It("Should initialize Provisioned on creation", func() {
		cl := buildProxmoxCluster(clusterName)
		g.Expect(k8sClient.Create(testEnv.GetContext(), &cl)).NotTo(HaveOccurred())
		defer cleanupResources(testEnv.GetContext(), g, cl)

		// The controller initializes Provisioned during reconcileNormal.
		g.Eventually(func(g Gomega) {
			var res infrav1.ProxmoxCluster
			g.Expect(k8sClient.Get(testEnv.GetContext(), client.ObjectKeyFromObject(&cl), &res)).To(Succeed())
			g.Expect(res.Status.Initialization.Provisioned).NotTo(BeNil())
		}).WithTimeout(time.Second * 10).
			WithPolling(time.Second).
			Should(Succeed())
	})

	Context("IPAM tests", func() {
		It("Should successfully create IPAM related resources", func() {
			cl := buildProxmoxCluster(clusterName)
			g.Expect(k8sClient.Create(testEnv.GetContext(), &cl)).NotTo(HaveOccurred())

			helper := ipam.NewHelper(k8sClient, &cl)

			defer cleanupResources(testEnv.GetContext(), g, cl)

			assertClusterIsReady(testEnv.GetContext(), g, clusterName)

			g.Eventually(func(g Gomega) {
				pool, err := helper.GetDefaultInClusterIPPool(testEnv.GetContext(), infrav1.IPv4Format)
				g.Expect(err).ToNot(HaveOccurred())

				config := cl.Spec.IPv4Config
				g.Expect(pool.Spec.Addresses).To(ConsistOf(config.Addresses))
				g.Expect(config.Gateway).To(BeEquivalentTo(pool.Spec.Gateway))
				g.Expect(pool.Spec.Prefix).To(BeEquivalentTo(24))

				// check if status was updated
				g.Expect(k8sClient.Get(testEnv.GetContext(), client.ObjectKeyFromObject(&cl), &cl)).To(Succeed())
				g.Expect(cl.Status.InClusterIPPoolRef).NotTo(BeNil())
				g.Expect(cl.Status.InClusterIPPoolRef[0].Name).To(BeEquivalentTo(pool.GetName()))
			}).WithTimeout(time.Second * 10).
				WithPolling(time.Second).
				Should(Succeed())
		})
		It("Should successfully create IPAM IPv6 related resources", func() {
			cl := buildProxmoxCluster(clusterName)
			cl.Spec.IPv6Config = &infrav1.IPConfigSpec{
				Addresses: []string{"2001:db8::/64"},
				Prefix:    64,
				Gateway:   "2001:db8::1",
			}
			g.Expect(k8sClient.Create(testEnv.GetContext(), &cl)).NotTo(HaveOccurred())

			helper := ipam.NewHelper(k8sClient, &cl)

			defer cleanupResources(testEnv.GetContext(), g, cl)

			assertClusterIsReady(testEnv.GetContext(), g, clusterName)

			g.Eventually(func(g Gomega) {
				pool, err := helper.GetDefaultInClusterIPPool(testEnv.GetContext(), infrav1.IPv6Format)
				g.Expect(err).ToNot(HaveOccurred())

				config := cl.Spec.IPv6Config
				g.Expect(pool.Spec.Addresses).To(ConsistOf(config.Addresses))
				g.Expect(config.Gateway).To(BeEquivalentTo(pool.Spec.Gateway))
				g.Expect(pool.Spec.Prefix).To(BeEquivalentTo(64))

				// check if status was updated
				g.Expect(k8sClient.Get(testEnv.GetContext(), client.ObjectKeyFromObject(&cl), &cl)).To(Succeed())
				g.Expect(cl.Status.InClusterIPPoolRef).NotTo(BeNil())
				g.Expect(cl.Status.InClusterIPPoolRef[1].Name).To(BeEquivalentTo(pool.GetName()))
			}).WithTimeout(time.Second * 10).
				WithPolling(time.Second).
				Should(Succeed())
		})
		It("Should successfully assign ControlPlaneEndpoint", func() {
			cl := buildProxmoxCluster(clusterName)

			g.Expect(k8sClient.Create(testEnv.GetContext(), &cl)).NotTo(HaveOccurred())
			helper := ipam.NewHelper(k8sClient, &cl)

			defer cleanupResources(testEnv.GetContext(), g, cl)

			assertClusterIsReady(testEnv.GetContext(), g, clusterName)

			g.Eventually(func(g Gomega) {
				pool, err := helper.GetDefaultInClusterIPPool(testEnv.GetContext(), infrav1.IPv4Format)
				g.Expect(err).ToNot(HaveOccurred())

				config := cl.Spec.IPv4Config
				g.Expect(pool.Spec.Addresses).To(ConsistOf(config.Addresses))
				g.Expect(config.Gateway).To(BeEquivalentTo(pool.Spec.Gateway))
				g.Expect(pool.Spec.Prefix).To(BeEquivalentTo(24))

				g.Expect(k8sClient.Get(testEnv.GetContext(), client.ObjectKeyFromObject(&cl), &cl)).To(Succeed())
				g.Expect(cl.Status.InClusterIPPoolRef).NotTo(BeNil())
				g.Expect(cl.Status.InClusterIPPoolRef[0].Name).To(BeEquivalentTo(pool.GetName()))
			}).WithTimeout(time.Second * 10).
				WithPolling(time.Second).
				Should(Succeed())

			pool, err := helper.GetDefaultInClusterIPPool(testEnv.GetContext(), infrav1.IPv4Format)
			g.Expect(err).ToNot(HaveOccurred())
			// create an IPAddress.
			g.Expect(k8sClient.Create(testEnv.GetContext(), dummyIPAddress(k8sClient, &cl, pool.GetName()))).To(Succeed())

			g.Eventually(func(g Gomega) {
				pool, err := helper.GetDefaultInClusterIPPool(testEnv.GetContext(), infrav1.IPv4Format)
				g.Expect(err).ToNot(HaveOccurred())

				ipAddr, err := helper.GetIPAddress(testEnv.GetContext(), client.ObjectKeyFromObject(&cl))
				g.Expect(err).ToNot(HaveOccurred())

				g.Expect(ipAddr).ToNot(BeNil())
				g.Expect(ipAddr.Spec.PoolRef.Name).To(BeEquivalentTo(pool.GetName()))
				g.Expect(ipAddr.Spec.Address).ToNot(BeEmpty())
				g.Expect(ptr.Deref(ipAddr.Spec.Prefix, 0)).To(BeEquivalentTo(pool.Spec.Prefix))
				g.Expect(ipAddr.Spec.Gateway).To(BeEquivalentTo(pool.Spec.Gateway))

				// check controlPlaneEndpoint is updated
				g.Expect(cl.Spec.ControlPlaneEndpoint.IsZero()).NotTo(BeTrue())
				g.Expect(cl.Spec.ControlPlaneEndpoint.Port).To(BeEquivalentTo(ControlPlaneEndpointPort))
				g.Expect(cl.Spec.ControlPlaneEndpoint.Host).To(BeEquivalentTo(ipAddr.Spec.Address))
			}).WithTimeout(time.Second * 10).
				WithPolling(time.Second).
				Should(Succeed())
		})
	})
})

var _ = Describe("External Credentials Tests", func() {
	Context("Reconcile an ProxmoxCluster", func() {
		It("create and destroy a cluster", func() {
			secret := createSecret()
			proxmoxCluster := createProxmoxCluster()
			setCredentialsRefOnProxmoxCluster(proxmoxCluster, secret)
			capiCluster := createOwnerCluster(proxmoxCluster)
			proxmoxCluster = refreshCluster(proxmoxCluster)
			setCapiClusterOwnerRefOnProxmoxCluster(proxmoxCluster, capiCluster)

			assertProxmoxClusterHasFinalizer(proxmoxCluster, infrav1.ClusterFinalizer)
			assertSecretHasNumberOfOwnerRefs(secret, 1)
			assertSecretHasOwnerRef(secret, proxmoxCluster.Name)
			assertSecretHasFinalizer(secret, infrav1.SecretFinalizer)
			assertProxmoxClusterIsReady(proxmoxCluster)

			deleteCapiCluster(capiCluster)
			deleteProxmoxCluster(proxmoxCluster)

			assertSecretHasOwnerRef(secret, proxmoxCluster.Name)
			assertSecretHasFinalizer(secret, infrav1.SecretFinalizer)

			cleanup(proxmoxCluster, capiCluster, secret)
		})

		It("multiple clusters can set ownerRef on secret", func() {
			secret := createSecret()
			setRandomOwnerRefOnSecret(secret, "another-cluster")

			//  First cluster
			proxmoxCluster1 := createProxmoxCluster()
			setCredentialsRefOnProxmoxCluster(proxmoxCluster1, secret)
			capiCluster1 := createOwnerCluster(proxmoxCluster1)
			proxmoxCluster1 = refreshCluster(proxmoxCluster1)
			setCapiClusterOwnerRefOnProxmoxCluster(proxmoxCluster1, capiCluster1)
			assertProxmoxClusterIsReady(proxmoxCluster1)
			assertProxmoxClusterHasFinalizer(proxmoxCluster1, infrav1.ClusterFinalizer)

			//  Second cluster
			proxmoxCluster2 := createProxmoxCluster()
			setCredentialsRefOnProxmoxCluster(proxmoxCluster2, secret)
			capiCluster2 := createOwnerCluster(proxmoxCluster2)
			proxmoxCluster2 = refreshCluster(proxmoxCluster2)
			setCapiClusterOwnerRefOnProxmoxCluster(proxmoxCluster2, capiCluster2)
			assertProxmoxClusterIsReady(proxmoxCluster2)
			assertProxmoxClusterHasFinalizer(proxmoxCluster2, infrav1.ClusterFinalizer)

			// Check owner references
			assertSecretHasNumberOfOwnerRefs(secret, 3)
			assertSecretHasOwnerRef(secret, proxmoxCluster1.Name)
			assertSecretHasOwnerRef(secret, proxmoxCluster2.Name)
			assertSecretHasOwnerRef(secret, "another-cluster")
			assertSecretHasFinalizer(secret, infrav1.SecretFinalizer)

			// Delete second cluster
			deleteCapiCluster(capiCluster2)
			deleteProxmoxCluster(proxmoxCluster2)

			// Check owner references
			assertSecretHasNumberOfOwnerRefs(secret, 2)
			assertSecretHasOwnerRef(secret, proxmoxCluster1.Name)
			assertSecretHasOwnerRef(secret, "another-cluster")
			assertSecretHasFinalizer(secret, infrav1.SecretFinalizer)

			// Delete first cluster
			deleteCapiCluster(capiCluster1)
			deleteProxmoxCluster(proxmoxCluster1)

			// Check owner references
			assertSecretHasNumberOfOwnerRefs(secret, 1)
			assertSecretHasOwnerRef(secret, "another-cluster")
			assertSecretHasFinalizer(secret, infrav1.SecretFinalizer)

			cleanup(proxmoxCluster1, capiCluster1, proxmoxCluster2, capiCluster2, secret)
		})

		It("owns the credentials Secret of every zone like its own", func() {
			secret := createSecret()
			zoneSecret := createSecret()
			proxmoxCluster := createProxmoxCluster()
			setCredentialsRefOnProxmoxCluster(proxmoxCluster, secret)
			setZoneCredentialsRefOnProxmoxCluster(proxmoxCluster, "dc-a", zoneSecret)
			capiCluster := createOwnerCluster(proxmoxCluster)
			proxmoxCluster = refreshCluster(proxmoxCluster)
			setCapiClusterOwnerRefOnProxmoxCluster(proxmoxCluster, capiCluster)

			assertProxmoxClusterHasFinalizer(proxmoxCluster, infrav1.ClusterFinalizer)
			assertProxmoxClusterIsReady(proxmoxCluster)
			for _, s := range []*corev1.Secret{secret, zoneSecret} {
				assertSecretHasNumberOfOwnerRefs(s, 1)
				assertSecretHasOwnerRef(s, proxmoxCluster.Name)
				assertSecretHasFinalizer(s, infrav1.SecretFinalizer)
			}

			By("releasing the zone's Secret once the zone is dropped from the spec")
			clearZoneConfigsOnProxmoxCluster(proxmoxCluster)
			assertSecretHasNoFinalizer(zoneSecret, infrav1.SecretFinalizer)
			assertSecretHasNumberOfOwnerRefs(zoneSecret, 0)
			assertSecretHasFinalizer(secret, infrav1.SecretFinalizer)

			By("releasing the cluster's Secret once the cluster is deleted")
			deleteCapiCluster(capiCluster)
			deleteProxmoxCluster(proxmoxCluster)
			assertProxmoxClusterIsDeleted(proxmoxCluster)
			assertSecretHasNoFinalizer(secret, infrav1.SecretFinalizer)

			cleanup(proxmoxCluster, capiCluster, secret, zoneSecret)
		})

		It("uses a credentials Secret of another namespace without owning it", func() {
			secret := createSecretIn(otherNamespace())
			proxmoxCluster := createProxmoxCluster()
			setCredentialsRefOnProxmoxCluster(proxmoxCluster, secret)
			capiCluster := createOwnerCluster(proxmoxCluster)
			proxmoxCluster = refreshCluster(proxmoxCluster)
			setCapiClusterOwnerRefOnProxmoxCluster(proxmoxCluster, capiCluster)

			assertProxmoxClusterIsReady(proxmoxCluster)
			assertSecretHasUserLabels(secret, proxmoxCluster)
			assertSecretHasFinalizer(secret, infrav1.SecretFinalizer)
			assertSecretHasNumberOfOwnerRefs(secret, 0)

			By("leaving the Secret in place once the cluster is deleted")
			deleteCapiCluster(capiCluster)
			deleteProxmoxCluster(proxmoxCluster)
			assertProxmoxClusterIsDeleted(proxmoxCluster)
			assertSecretHasUserLabels(secret)
			assertSecretHasNoFinalizer(secret, infrav1.SecretFinalizer)
			Expect(secret.GetDeletionTimestamp().IsZero()).To(BeTrue())

			cleanup(proxmoxCluster, capiCluster, secret)
		})

		It("keeps a Secret of another namespace finalized until its last cluster is deleted", func() {
			secret := createSecretIn(otherNamespace())

			proxmoxCluster1 := createProxmoxCluster()
			setCredentialsRefOnProxmoxCluster(proxmoxCluster1, secret)
			capiCluster1 := createOwnerCluster(proxmoxCluster1)
			proxmoxCluster1 = refreshCluster(proxmoxCluster1)
			setCapiClusterOwnerRefOnProxmoxCluster(proxmoxCluster1, capiCluster1)
			assertProxmoxClusterIsReady(proxmoxCluster1)

			proxmoxCluster2 := createProxmoxCluster()
			setCredentialsRefOnProxmoxCluster(proxmoxCluster2, secret)
			capiCluster2 := createOwnerCluster(proxmoxCluster2)
			proxmoxCluster2 = refreshCluster(proxmoxCluster2)
			setCapiClusterOwnerRefOnProxmoxCluster(proxmoxCluster2, capiCluster2)
			assertProxmoxClusterIsReady(proxmoxCluster2)

			assertSecretHasUserLabels(secret, proxmoxCluster1, proxmoxCluster2)
			assertSecretHasFinalizer(secret, infrav1.SecretFinalizer)
			assertSecretHasNumberOfOwnerRefs(secret, 0)

			deleteCapiCluster(capiCluster2)
			deleteProxmoxCluster(proxmoxCluster2)
			assertProxmoxClusterIsDeleted(proxmoxCluster2)
			assertSecretHasUserLabels(secret, proxmoxCluster1)
			assertSecretHasFinalizer(secret, infrav1.SecretFinalizer)

			deleteCapiCluster(capiCluster1)
			deleteProxmoxCluster(proxmoxCluster1)
			assertProxmoxClusterIsDeleted(proxmoxCluster1)
			assertSecretHasUserLabels(secret)
			assertSecretHasNoFinalizer(secret, infrav1.SecretFinalizer)

			cleanup(proxmoxCluster1, capiCluster1, proxmoxCluster2, capiCluster2, secret)
		})

		It("releases a zone's Secret of another namespace once the zone is dropped", func() {
			secret := createSecret()
			zoneSecret := createSecretIn(otherNamespace())
			proxmoxCluster := createProxmoxCluster()
			setCredentialsRefOnProxmoxCluster(proxmoxCluster, secret)
			setZoneCredentialsRefOnProxmoxCluster(proxmoxCluster, "dc-a", zoneSecret)
			capiCluster := createOwnerCluster(proxmoxCluster)
			proxmoxCluster = refreshCluster(proxmoxCluster)
			setCapiClusterOwnerRefOnProxmoxCluster(proxmoxCluster, capiCluster)

			assertProxmoxClusterIsReady(proxmoxCluster)
			assertSecretHasUserLabels(zoneSecret, proxmoxCluster)
			assertSecretHasFinalizer(zoneSecret, infrav1.SecretFinalizer)
			assertSecretHasNumberOfOwnerRefs(zoneSecret, 0)
			assertSecretHasUserLabels(secret)
			assertSecretHasNumberOfOwnerRefs(secret, 1)

			clearZoneConfigsOnProxmoxCluster(proxmoxCluster)
			assertSecretHasUserLabels(zoneSecret)
			assertSecretHasNoFinalizer(zoneSecret, infrav1.SecretFinalizer)
			assertSecretHasFinalizer(secret, infrav1.SecretFinalizer)

			deleteCapiCluster(capiCluster)
			deleteProxmoxCluster(proxmoxCluster)
			assertProxmoxClusterIsDeleted(proxmoxCluster)

			cleanup(proxmoxCluster, capiCluster, secret, zoneSecret)
		})

		It("replaces an owner reference on a Secret of another namespace with the user label", func() {
			secret := createSecretIn(otherNamespace())
			proxmoxCluster := createProxmoxCluster()
			By("an earlier version having made the cluster and another one, which is not reconciled, owners")
			other := createProxmoxCluster()
			setOwnerRefsOnSecret(secret,
				metav1.OwnerReference{APIVersion: infrav1.GroupVersion.String(), Kind: "ProxmoxCluster", Name: proxmoxCluster.Name, UID: proxmoxCluster.UID},
				metav1.OwnerReference{APIVersion: infrav1.GroupVersion.String(), Kind: "ProxmoxCluster", Name: other.Name, UID: other.UID},
			)
			setCredentialsRefOnProxmoxCluster(proxmoxCluster, secret)
			capiCluster := createOwnerCluster(proxmoxCluster)
			proxmoxCluster = refreshCluster(proxmoxCluster)
			setCapiClusterOwnerRefOnProxmoxCluster(proxmoxCluster, capiCluster)

			assertProxmoxClusterIsReady(proxmoxCluster)
			assertSecretHasUserLabels(secret, proxmoxCluster)
			assertSecretHasOwnerUIDs(secret, other)
			assertSecretHasFinalizer(secret, infrav1.SecretFinalizer)

			By("the other cluster's owner reference still counting as a user")
			deleteCapiCluster(capiCluster)
			deleteProxmoxCluster(proxmoxCluster)
			assertProxmoxClusterIsDeleted(proxmoxCluster)
			assertSecretHasUserLabels(secret)
			assertSecretHasOwnerUIDs(secret, other)
			assertSecretHasFinalizer(secret, infrav1.SecretFinalizer)

			forgetSecret(secret)
			cleanup(proxmoxCluster, capiCluster, other, secret)
		})

		It("removes the owner reference of a ProxmoxCluster that is gone from a Secret of another namespace", func() {
			secret := createSecretIn(otherNamespace())
			By("a cluster of the Secret's namespace having been deleted as its last user, which leaves its owner reference")
			setOwnerRefsOnSecret(secret, metav1.OwnerReference{APIVersion: infrav1.GroupVersion.String(), Kind: "ProxmoxCluster", Name: "gone", UID: types.UID(util.RandomString(6))})

			proxmoxCluster := createProxmoxCluster()
			setCredentialsRefOnProxmoxCluster(proxmoxCluster, secret)
			capiCluster := createOwnerCluster(proxmoxCluster)
			proxmoxCluster = refreshCluster(proxmoxCluster)
			setCapiClusterOwnerRefOnProxmoxCluster(proxmoxCluster, capiCluster)

			assertProxmoxClusterIsReady(proxmoxCluster)
			assertSecretHasUserLabels(secret, proxmoxCluster)
			assertSecretHasOwnerUIDs(secret)
			assertSecretHasFinalizer(secret, infrav1.SecretFinalizer)

			deleteCapiCluster(capiCluster)
			deleteProxmoxCluster(proxmoxCluster)
			assertProxmoxClusterIsDeleted(proxmoxCluster)
			assertSecretHasNoFinalizer(secret, infrav1.SecretFinalizer)

			cleanup(proxmoxCluster, capiCluster, secret)
		})

		It("refuses to write a Secret that changed since it was read", func() {
			secret := createSecretIn(otherNamespace())
			proxmoxCluster := createProxmoxCluster()
			reconciler := &ProxmoxClusterReconciler{Client: k8sClient}

			stale := secret.DeepCopy()
			By("another cluster taking the Secret up meanwhile")
			addFinalizerToSecret(secret, infrav1.SecretFinalizer)

			err := reconciler.useCredentialsSecret(testEnv.GetContext(), proxmoxCluster, stale)
			Expect(apierrors.IsConflict(err)).To(BeTrue(), "got %v", err)

			By("the next reconcile reading it again")
			Expect(k8sClient.Get(testEnv.GetContext(), client.ObjectKeyFromObject(secret), secret)).To(Succeed())
			Expect(reconciler.useCredentialsSecret(testEnv.GetContext(), proxmoxCluster, secret)).To(Succeed())
			assertSecretHasUserLabels(secret, proxmoxCluster)

			By("the same for a release")
			stale = secret.DeepCopy()
			addFinalizerToSecret(secret, holdFinalizer)
			err = reconciler.releaseCredentialsSecret(testEnv.GetContext(), proxmoxCluster, stale, false)
			Expect(apierrors.IsConflict(err)).To(BeTrue(), "got %v", err)

			forgetSecret(secret)
			cleanup(proxmoxCluster, secret)
		})

		It("tells its owner reference from that of a cluster of the same name in the Secret's namespace", func() {
			secret := createSecretIn(otherNamespace())

			By("a cluster of the Secret's namespace owning it")
			owner := createProxmoxClusterIn(secret.Namespace, "")
			setCredentialsRefOnProxmoxCluster(owner, secret)
			ownerCapiCluster := createOwnerCluster(owner)
			owner = refreshCluster(owner)
			setCapiClusterOwnerRefOnProxmoxCluster(owner, ownerCapiCluster)
			assertProxmoxClusterIsReady(owner)
			assertSecretHasOwnerUIDs(secret, owner)

			By("a cluster of the same name in another namespace using it")
			user := createProxmoxClusterIn("default", owner.Name)
			setCredentialsRefOnProxmoxCluster(user, secret)
			userCapiCluster := createOwnerCluster(user)
			user = refreshCluster(user)
			setCapiClusterOwnerRefOnProxmoxCluster(user, userCapiCluster)
			assertProxmoxClusterIsReady(user)
			assertSecretHasUserLabels(secret, user)
			assertSecretHasOwnerUIDs(secret, owner)
			assertSecretHasFinalizer(secret, infrav1.SecretFinalizer)

			By("the user going and the owner staying")
			deleteCapiCluster(userCapiCluster)
			deleteProxmoxCluster(user)
			assertProxmoxClusterIsDeleted(user)
			assertSecretHasUserLabels(secret)
			assertSecretHasOwnerUIDs(secret, owner)
			assertSecretHasFinalizer(secret, infrav1.SecretFinalizer)

			deleteCapiCluster(ownerCapiCluster)
			deleteProxmoxCluster(owner)
			assertProxmoxClusterIsDeleted(owner)
			assertSecretHasNoFinalizer(secret, infrav1.SecretFinalizer)

			forgetSecret(secret)
			cleanup(owner, ownerCapiCluster, user, userCapiCluster, secret)
		})

		It("keeps a Secret its owner left to a cluster of another namespace", func() {
			secret := createSecretIn(otherNamespace())

			owner := createProxmoxClusterIn(secret.Namespace, "")
			setCredentialsRefOnProxmoxCluster(owner, secret)
			ownerCapiCluster := createOwnerCluster(owner)
			owner = refreshCluster(owner)
			setCapiClusterOwnerRefOnProxmoxCluster(owner, ownerCapiCluster)
			assertProxmoxClusterIsReady(owner)

			user := createProxmoxCluster()
			setCredentialsRefOnProxmoxCluster(user, secret)
			userCapiCluster := createOwnerCluster(user)
			user = refreshCluster(user)
			setCapiClusterOwnerRefOnProxmoxCluster(user, userCapiCluster)
			assertProxmoxClusterIsReady(user)
			assertSecretHasUserLabels(secret, user)
			assertSecretHasOwnerUIDs(secret, owner)

			By("the owner going first: its owner reference goes, and the Secret stays")
			deleteCapiCluster(ownerCapiCluster)
			deleteProxmoxCluster(owner)
			assertProxmoxClusterIsDeleted(owner)
			assertSecretHasOwnerUIDs(secret)
			assertSecretHasUserLabels(secret, user)
			assertSecretHasFinalizer(secret, infrav1.SecretFinalizer)

			deleteCapiCluster(userCapiCluster)
			deleteProxmoxCluster(user)
			assertProxmoxClusterIsDeleted(user)
			assertSecretHasUserLabels(secret)
			assertSecretHasNoFinalizer(secret, infrav1.SecretFinalizer)
			Expect(secret.GetDeletionTimestamp().IsZero()).To(BeTrue())

			cleanup(owner, ownerCapiCluster, user, userCapiCluster, secret)
		})

		It("uses a Secret of another namespace that is being deleted without finalizing it", func() {
			secret := createSecretIn(otherNamespace())
			addFinalizerToSecret(secret, holdFinalizer)
			Expect(k8sClient.Delete(testEnv.GetContext(), secret)).To(Succeed())

			proxmoxCluster := createProxmoxCluster()
			setCredentialsRefOnProxmoxCluster(proxmoxCluster, secret)
			capiCluster := createOwnerCluster(proxmoxCluster)
			proxmoxCluster = refreshCluster(proxmoxCluster)
			setCapiClusterOwnerRefOnProxmoxCluster(proxmoxCluster, capiCluster)

			assertProxmoxClusterIsReady(proxmoxCluster)
			assertSecretHasUserLabels(secret, proxmoxCluster)
			Expect(secret.GetDeletionTimestamp().IsZero()).To(BeFalse())
			Expect(secret.GetFinalizers()).To(ConsistOf(holdFinalizer))

			deleteCapiCluster(capiCluster)
			deleteProxmoxCluster(proxmoxCluster)
			assertProxmoxClusterIsDeleted(proxmoxCluster)
			assertSecretHasUserLabels(secret)

			forgetSecret(secret)
			cleanup(proxmoxCluster, capiCluster)
		})

		It("does not count an owner reference of another kind as a user", func() {
			secret := createSecretIn(otherNamespace())
			setOwnerRefsOnSecret(secret, metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "someone-else", UID: types.UID(util.RandomString(6))})
			proxmoxCluster := createProxmoxCluster()
			setCredentialsRefOnProxmoxCluster(proxmoxCluster, secret)
			capiCluster := createOwnerCluster(proxmoxCluster)
			proxmoxCluster = refreshCluster(proxmoxCluster)
			setCapiClusterOwnerRefOnProxmoxCluster(proxmoxCluster, capiCluster)
			assertProxmoxClusterIsReady(proxmoxCluster)
			assertSecretHasUserLabels(secret, proxmoxCluster)
			assertSecretHasFinalizer(secret, infrav1.SecretFinalizer)

			deleteCapiCluster(capiCluster)
			deleteProxmoxCluster(proxmoxCluster)
			assertProxmoxClusterIsDeleted(proxmoxCluster)
			assertSecretHasUserLabels(secret)
			assertSecretHasNoFinalizer(secret, infrav1.SecretFinalizer)
			assertSecretHasOwnerRef(secret, "someone-else")

			forgetSecret(secret)
			cleanup(proxmoxCluster, capiCluster, secret)
		})

		It("owns a Secret of its namespace that is being deleted without finalizing it", func() {
			secret := createSecret()
			addFinalizerToSecret(secret, holdFinalizer)
			Expect(k8sClient.Delete(testEnv.GetContext(), secret)).To(Succeed())

			proxmoxCluster := createProxmoxCluster()
			setCredentialsRefOnProxmoxCluster(proxmoxCluster, secret)
			capiCluster := createOwnerCluster(proxmoxCluster)
			proxmoxCluster = refreshCluster(proxmoxCluster)
			setCapiClusterOwnerRefOnProxmoxCluster(proxmoxCluster, capiCluster)

			assertProxmoxClusterIsReady(proxmoxCluster)
			assertSecretHasOwnerUIDs(secret, proxmoxCluster)
			Expect(secret.GetDeletionTimestamp().IsZero()).To(BeFalse())
			Expect(secret.GetFinalizers()).To(ConsistOf(holdFinalizer))

			deleteCapiCluster(capiCluster)
			deleteProxmoxCluster(proxmoxCluster)
			assertProxmoxClusterIsDeleted(proxmoxCluster)

			forgetSecret(secret)
			cleanup(proxmoxCluster, capiCluster)
		})

		It("releases a Secret of its namespace it no longer reads when it is deleted", func() {
			secret := createSecret()
			proxmoxCluster := createProxmoxCluster()
			setCredentialsRefOnProxmoxCluster(proxmoxCluster, secret)
			capiCluster := createOwnerCluster(proxmoxCluster)
			proxmoxCluster = refreshCluster(proxmoxCluster)
			setCapiClusterOwnerRefOnProxmoxCluster(proxmoxCluster, capiCluster)
			assertProxmoxClusterIsReady(proxmoxCluster)
			assertSecretHasOwnerUIDs(secret, proxmoxCluster)
			assertSecretHasFinalizer(secret, infrav1.SecretFinalizer)

			By("the reference moving to a Secret that does not exist, which stops the reconcile before any release")
			proxmoxCluster = refreshCluster(proxmoxCluster)
			setRandomCredentialsRefOnProxmoxCluster(proxmoxCluster)
			assertProxmoxClusterIsNotReady(proxmoxCluster)
			assertSecretHasFinalizer(secret, infrav1.SecretFinalizer)

			deleteCapiCluster(capiCluster)
			deleteProxmoxCluster(proxmoxCluster)
			assertProxmoxClusterIsDeleted(proxmoxCluster)
			assertSecretHasNoFinalizer(secret, infrav1.SecretFinalizer)

			forgetSecret(secret)
			cleanup(proxmoxCluster, capiCluster, secret)
		})

		It("releases a Secret of another namespace it no longer reads when it is deleted", func() {
			secret := createSecretIn(otherNamespace())
			proxmoxCluster := createProxmoxCluster()
			setCredentialsRefOnProxmoxCluster(proxmoxCluster, secret)
			capiCluster := createOwnerCluster(proxmoxCluster)
			proxmoxCluster = refreshCluster(proxmoxCluster)
			setCapiClusterOwnerRefOnProxmoxCluster(proxmoxCluster, capiCluster)
			assertProxmoxClusterIsReady(proxmoxCluster)
			assertSecretHasUserLabels(secret, proxmoxCluster)

			By("the reference moving to a Secret that does not exist, which stops the reconcile before any release")
			proxmoxCluster = refreshCluster(proxmoxCluster)
			setRandomCredentialsRefOnProxmoxCluster(proxmoxCluster)
			assertProxmoxClusterIsNotReady(proxmoxCluster)
			assertSecretHasUserLabels(secret, proxmoxCluster)

			deleteCapiCluster(capiCluster)
			deleteProxmoxCluster(proxmoxCluster)
			assertProxmoxClusterIsDeleted(proxmoxCluster)
			assertSecretHasUserLabels(secret)
			assertSecretHasNoFinalizer(secret, infrav1.SecretFinalizer)

			cleanup(proxmoxCluster, capiCluster, secret)
		})
	})

	It("should remove ProxmoxCluster finalizer if the secret does not exist", func() {
		proxmoxCluster := createProxmoxCluster()
		setRandomCredentialsRefOnProxmoxCluster(proxmoxCluster)

		capiCluster := createOwnerCluster(proxmoxCluster)
		proxmoxCluster = refreshCluster(proxmoxCluster)
		setCapiClusterOwnerRefOnProxmoxCluster(proxmoxCluster, capiCluster)

		assertProxmoxClusterIsNotReady(proxmoxCluster)
		assertProxmoxClusterHasFinalizer(proxmoxCluster, infrav1.ClusterFinalizer)

		By("deleting the proxmoxcluster while the secret is gone")
		deleteCapiCluster(capiCluster)
		deleteProxmoxCluster(proxmoxCluster)
		assertProxmoxClusterIsDeleted(proxmoxCluster)
	})
})

func cleanupResources(ctx context.Context, g Gomega, cl infrav1.ProxmoxCluster) {
	g.Expect(k8sClient.Delete(context.Background(), &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: testNS}})).To(Succeed())
	g.Expect(k8sClient.Delete(ctx, &cl)).To(Succeed())
	g.Expect(k8sClient.DeleteAllOf(ctx, &ipamicv1.InClusterIPPool{}, client.InNamespace(testNS))).To(Succeed())
	g.Eventually(func(g Gomega) {
		err := k8sClient.Get(ctx, client.ObjectKey{Name: cl.GetName(), Namespace: cl.GetNamespace()}, &infrav1.ProxmoxCluster{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(Succeed())
}

func buildProxmoxCluster(name string) infrav1.ProxmoxCluster {
	cl := infrav1.ProxmoxCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNS,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: clusterv1.GroupVersion.String(),
					Kind:       "Cluster",
					Name:       "test",
					UID:        "1000",
				},
			},
		},
		Spec: infrav1.ProxmoxClusterSpec{
			ControlPlaneEndpoint: infrav1.APIEndpoint{
				Host: "10.10.10.11",
				Port: 6443,
			},
			IPv4Config: &infrav1.IPConfigSpec{
				Addresses: []string{
					"10.10.10.2-10.10.10.10",
					"10.10.10.100-10.10.10.125",
					"10.10.10.192/64",
				},
				Gateway: "10.10.10.1",
				Prefix:  24,
			},
			DNSServers: []string{"8.8.8.8", "8.8.4.4"},
		},
	}

	return cl
}

func assertClusterIsReady(ctx context.Context, g Gomega, clusterName string) {
	g.Eventually(func(g Gomega) {
		var res infrav1.ProxmoxCluster
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{
			Namespace: testNS,
			Name:      clusterName,
		}, &res)).To(Succeed())

		g.Expect(ptr.Deref(res.Status.Initialization.Provisioned, false)).To(BeTrue())
	}).WithTimeout(time.Second * 20).
		WithPolling(time.Second).
		Should(Succeed())
}

func dummyIPAddress(client client.Client, owner client.Object, poolName string) *ipamv1.IPAddress {
	gvk, err := apiutil.GVKForObject(new(ipamicv1.InClusterIPPool), client.Scheme())
	if err != nil {
		panic(err)
	}
	return &ipamv1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      owner.GetName(),
			Namespace: owner.GetNamespace(),
		},
		Spec: ipamv1.IPAddressSpec{
			ClaimRef: ipamv1.IPAddressClaimReference{
				Name: owner.GetName(),
			},
			PoolRef: ipamv1.IPPoolReference{
				APIGroup: gvk.Group,
				Kind:     gvk.Kind,
				Name:     poolName,
			},
			Address: "10.10.10.11",
			Prefix:  new(int32(24)),
			Gateway: "10.10.10.1",
		},
	}
}

func createSecret() *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "secret-",
			Namespace:    "default",
		},
		Data: map[string][]byte{
			"url":    []byte("url"),
			"token":  []byte("token"),
			"secret": []byte("secret"),
		},
	}
	Expect(k8sClient.Create(testEnv.GetContext(), secret)).To(Succeed())
	return secret
}

// otherNamespace is a namespace other than default, where most
// ProxmoxClusters of these tests live, created on first use.
func otherNamespace() string {
	const name = "credentials"
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := k8sClient.Create(testEnv.GetContext(), ns); err != nil {
		Expect(apierrors.IsAlreadyExists(err)).To(BeTrue())
	}
	return name
}

// createSecretIn creates a credentials Secret in the given namespace.
func createSecretIn(namespace string) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "secret-",
			Namespace:    namespace,
		},
		Data: map[string][]byte{
			"url":    []byte("url"),
			"token":  []byte("token"),
			"secret": []byte("secret"),
		},
	}
	Expect(k8sClient.Create(testEnv.GetContext(), secret)).To(Succeed())
	return secret
}

// holdFinalizer keeps a Secret from being removed while a test looks at it
// being deleted.
const holdFinalizer = "test.cluster.x-k8s.io/hold"

// addFinalizerToSecret puts the finalizer on the Secret.
func addFinalizerToSecret(secret *corev1.Secret, finalizer string) {
	key := client.ObjectKeyFromObject(secret)
	Eventually(func() error {
		if err := k8sClient.Get(testEnv.GetContext(), key, secret); err != nil {
			return err
		}
		ctrlutil.AddFinalizer(secret, finalizer)
		return k8sClient.Update(testEnv.GetContext(), secret)
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(Succeed())
}

// forgetSecret takes every owner reference and finalizer off a Secret, so
// that cleanup removes it.
func forgetSecret(secret *corev1.Secret) {
	key := client.ObjectKeyFromObject(secret)
	Eventually(func() error {
		if err := k8sClient.Get(testEnv.GetContext(), key, secret); err != nil {
			return client.IgnoreNotFound(err)
		}
		secret.OwnerReferences = nil
		secret.Finalizers = nil
		return k8sClient.Update(testEnv.GetContext(), secret)
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(Succeed())
}

// assertSecretHasOwnerUIDs waits until the Secret's owner references are
// exactly those of the given ProxmoxClusters.
func assertSecretHasOwnerUIDs(secret *corev1.Secret, owners ...*infrav1.ProxmoxCluster) {
	want := make([]types.UID, 0, len(owners))
	for _, owner := range owners {
		want = append(want, owner.UID)
	}
	key := client.ObjectKey{Namespace: secret.Namespace, Name: secret.Name}
	Eventually(func(g Gomega) {
		g.Expect(testEnv.Get(testEnv.GetContext(), key, secret)).To(Succeed())
		got := make([]types.UID, 0, len(secret.OwnerReferences))
		for _, ref := range secret.OwnerReferences {
			got = append(got, ref.UID)
		}
		g.Expect(got).To(ConsistOf(want))
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(Succeed())
}

// setOwnerRefsOnSecret replaces the Secret's owner references.
func setOwnerRefsOnSecret(secret *corev1.Secret, ownerRefs ...metav1.OwnerReference) {
	Eventually(func() error {
		ph, err := patch.NewHelper(secret, testEnv)
		Expect(err).ShouldNot(HaveOccurred())
		secret.OwnerReferences = ownerRefs
		return ph.Patch(testEnv.GetContext(), secret)
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(Succeed())
}

// assertSecretHasUserLabels waits until the Secret's user labels are exactly
// those of the given ProxmoxClusters.
func assertSecretHasUserLabels(secret *corev1.Secret, users ...*infrav1.ProxmoxCluster) {
	want := make([]string, 0, len(users))
	for _, user := range users {
		want = append(want, infrav1.SecretUserLabelPrefix+string(user.UID))
	}
	key := client.ObjectKey{Namespace: secret.Namespace, Name: secret.Name}
	Eventually(func(g Gomega) {
		g.Expect(testEnv.Get(testEnv.GetContext(), key, secret)).To(Succeed())
		got := []string{}
		for label := range secret.Labels {
			if strings.HasPrefix(label, infrav1.SecretUserLabelPrefix) {
				got = append(got, label)
			}
		}
		g.Expect(got).To(ConsistOf(want))
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(Succeed())
}

func createProxmoxCluster() *infrav1.ProxmoxCluster {
	return createProxmoxClusterIn("default", "")
}

// createProxmoxClusterIn creates a ProxmoxCluster in the given namespace,
// under the given name or, without one, a generated name.
func createProxmoxClusterIn(namespace, name string) *infrav1.ProxmoxCluster {
	meta := metav1.ObjectMeta{Namespace: namespace, Name: name}
	if name == "" {
		meta.GenerateName = "proxmox-test-"
	}
	proxmoxCluster := &infrav1.ProxmoxCluster{
		ObjectMeta: meta,
		Spec: infrav1.ProxmoxClusterSpec{
			IPv4Config: &infrav1.IPConfigSpec{
				Addresses: []string{
					"10.10.10.2-10.10.10.10",
					"10.10.10.100-10.10.10.125",
					"10.10.10.192/64",
				},
				Gateway: "10.10.10.1",
				Prefix:  24,
			},
			DNSServers: []string{"8.8.8.8", "8.8.4.4"},
		},
	}
	Expect(testEnv.Create(testEnv.GetContext(), proxmoxCluster)).To(Succeed())
	return proxmoxCluster
}

func setCredentialsRefOnProxmoxCluster(proxmoxCluster *infrav1.ProxmoxCluster, secret *corev1.Secret) {
	Eventually(func() error {
		ph, err := patch.NewHelper(proxmoxCluster, testEnv)
		Expect(err).ShouldNot(HaveOccurred())
		proxmoxCluster.Spec.CredentialsRef = &corev1.SecretReference{
			Name:      secret.Name,
			Namespace: secret.Namespace,
		}
		return ph.Patch(testEnv.GetContext(), proxmoxCluster, patch.WithStatusObservedGeneration{})
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(Succeed())
}

// setZoneCredentialsRefOnProxmoxCluster declares a zone backed by the given
// credentials Secret, with the pool and resolver the schema requires.
func setZoneCredentialsRefOnProxmoxCluster(proxmoxCluster *infrav1.ProxmoxCluster, zone string, secret *corev1.Secret) {
	Eventually(func() error {
		ph, err := patch.NewHelper(proxmoxCluster, testEnv)
		Expect(err).ShouldNot(HaveOccurred())
		proxmoxCluster.Spec.ZoneConfigs = append(proxmoxCluster.Spec.ZoneConfigs, infrav1.ZoneConfigSpec{
			Zone: new(zone),
			IPv4Config: &infrav1.IPConfigSpec{
				Addresses: []string{"10.10.20.2-10.10.20.10"},
				Gateway:   "10.10.20.1",
				Prefix:    24,
			},
			DNSServers: []string{"8.8.8.8"},
			CredentialsRef: &corev1.SecretReference{
				Name:      secret.Name,
				Namespace: secret.Namespace,
			},
		})
		return ph.Patch(testEnv.GetContext(), proxmoxCluster, patch.WithStatusObservedGeneration{})
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(Succeed())
}

// clearZoneConfigsOnProxmoxCluster drops every zone from the spec.
func clearZoneConfigsOnProxmoxCluster(proxmoxCluster *infrav1.ProxmoxCluster) {
	Eventually(func() error {
		refreshCluster(proxmoxCluster)
		ph, err := patch.NewHelper(proxmoxCluster, testEnv)
		Expect(err).ShouldNot(HaveOccurred())
		proxmoxCluster.Spec.ZoneConfigs = nil
		return ph.Patch(testEnv.GetContext(), proxmoxCluster, patch.WithStatusObservedGeneration{})
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(Succeed())
}

// assertSecretHasNoFinalizer waits until the Secret no longer carries the
// finalizer, which is how the controller releases a Secret it owned alone.
func assertSecretHasNoFinalizer(secret *corev1.Secret, finalizer string) {
	key := client.ObjectKey{Namespace: secret.Namespace, Name: secret.Name}
	Eventually(func() bool {
		if err := testEnv.Get(testEnv.GetContext(), key, secret); err != nil {
			return false
		}
		return !ctrlutil.ContainsFinalizer(secret, finalizer)
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(BeTrue())
}

func setRandomCredentialsRefOnProxmoxCluster(proxmoxCluster *infrav1.ProxmoxCluster) {
	Eventually(func() error {
		ph, err := patch.NewHelper(proxmoxCluster, testEnv)
		Expect(err).ShouldNot(HaveOccurred())
		proxmoxCluster.Spec.CredentialsRef = &corev1.SecretReference{
			Name:      util.RandomString(6),
			Namespace: util.RandomString(6),
		}
		return ph.Patch(testEnv.GetContext(), proxmoxCluster, patch.WithStatusObservedGeneration{})
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(Succeed())
}

func createOwnerCluster(proxmoxCluster *infrav1.ProxmoxCluster) *clusterv1.Cluster {
	capiCluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-",
			Namespace:    proxmoxCluster.Namespace,
		},
		Spec: clusterv1.ClusterSpec{
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: infrav1.GroupVersion.Group,
				Kind:     "ProxmoxCluster",
				Name:     proxmoxCluster.Name,
			},
		},
	}
	ctrlutil.AddFinalizer(capiCluster, "cluster.cluster.x-k8s.io")
	Expect(testEnv.Create(testEnv.GetContext(), capiCluster)).To(Succeed())
	return capiCluster
}

func setCapiClusterOwnerRefOnProxmoxCluster(proxmoxCluster *infrav1.ProxmoxCluster, capiCluster *clusterv1.Cluster) {
	Eventually(func() error {
		ph, err := patch.NewHelper(proxmoxCluster, testEnv)
		Expect(err).ShouldNot(HaveOccurred())
		proxmoxCluster.OwnerReferences = append(proxmoxCluster.OwnerReferences, metav1.OwnerReference{
			Kind:       "Cluster",
			APIVersion: clusterv1.GroupVersion.String(),
			Name:       capiCluster.Name,
			UID:        (types.UID)(util.RandomString(6)),
		})
		return ph.Patch(testEnv.GetContext(), proxmoxCluster, patch.WithStatusObservedGeneration{})
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(Succeed())
}

func setRandomOwnerRefOnSecret(secret *corev1.Secret, ownerRef string) {
	Eventually(func() error {
		ph, err := patch.NewHelper(secret, testEnv)
		Expect(err).ShouldNot(HaveOccurred())
		secret.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion: infrav1.GroupVersion.String(),
				Kind:       "ProxmoxCluster",
				Name:       ownerRef,
				UID:        (types.UID)(util.RandomString(6)),
			},
		}
		return ph.Patch(testEnv.GetContext(), secret, patch.WithStatusObservedGeneration{})
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(Succeed())
}

func refreshCluster(proxmoxCluster *infrav1.ProxmoxCluster) *infrav1.ProxmoxCluster {
	key := client.ObjectKey{Namespace: proxmoxCluster.Namespace, Name: proxmoxCluster.Name}
	Expect(k8sClient.Get(testEnv.GetContext(), key, proxmoxCluster)).To(Succeed())
	return proxmoxCluster
}

func deleteProxmoxCluster(proxmoxCluster *infrav1.ProxmoxCluster) {
	Eventually(func() bool {
		err := testEnv.Delete(testEnv.GetContext(), proxmoxCluster)
		return err == nil
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(BeTrue())
}

func deleteCapiCluster(cluster *clusterv1.Cluster) {
	Eventually(func() bool {
		err := testEnv.Delete(testEnv.GetContext(), cluster)
		return err == nil
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(BeTrue())
}

func assertProxmoxClusterHasFinalizer(proxmoxCluster *infrav1.ProxmoxCluster, finalizer string) {
	key := client.ObjectKey{Namespace: proxmoxCluster.Namespace, Name: proxmoxCluster.Name}
	Eventually(func() bool {
		if err := testEnv.Get(testEnv.GetContext(), key, proxmoxCluster); err != nil {
			return false
		}
		return ctrlutil.ContainsFinalizer(proxmoxCluster, finalizer)
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(BeTrue())
}

func assertSecretHasFinalizer(secret *corev1.Secret, finalizer string) {
	key := client.ObjectKey{Namespace: secret.Namespace, Name: secret.Name}
	Eventually(func() bool {
		if err := testEnv.Get(testEnv.GetContext(), key, secret); err != nil {
			return false
		}
		return ctrlutil.ContainsFinalizer(secret, finalizer)
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(BeTrue())
}

func assertSecretHasOwnerRef(secret *corev1.Secret, ownerRef string) {
	key := client.ObjectKey{Namespace: secret.Namespace, Name: secret.Name}
	Eventually(func() bool {
		if err := testEnv.Get(testEnv.GetContext(), key, secret); err != nil {
			return false
		}
		for _, ref := range secret.OwnerReferences {
			if ref.Name == ownerRef {
				return true
			}
		}
		return false
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(BeTrue())
}

func assertSecretHasNumberOfOwnerRefs(secret *corev1.Secret, nrOfOwnerRefs int) {
	key := client.ObjectKey{Namespace: secret.Namespace, Name: secret.Name}
	Eventually(func() bool {
		if err := testEnv.Get(testEnv.GetContext(), key, secret); err != nil {
			return false
		}
		return len(secret.OwnerReferences) == nrOfOwnerRefs
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(BeTrue())
}

func assertProxmoxClusterIsReady(proxmoxCluster *infrav1.ProxmoxCluster) {
	key := client.ObjectKey{Namespace: proxmoxCluster.Namespace, Name: proxmoxCluster.Name}
	Eventually(func() bool {
		if err := testEnv.Get(testEnv.GetContext(), key, proxmoxCluster); err != nil {
			return false
		}
		return conditions.IsTrue(proxmoxCluster, infrav1.ProxmoxClusterProxmoxAvailableCondition)
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(BeTrue())
}

func assertProxmoxClusterIsNotReady(proxmoxCluster *infrav1.ProxmoxCluster) {
	key := client.ObjectKey{Namespace: proxmoxCluster.Namespace, Name: proxmoxCluster.Name}
	Eventually(func() bool {
		if err := testEnv.Get(testEnv.GetContext(), key, proxmoxCluster); err != nil {
			return false
		}
		return conditions.IsFalse(proxmoxCluster, infrav1.ProxmoxClusterProxmoxAvailableCondition)
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(BeTrue())
}

func assertProxmoxClusterIsDeleted(proxmoxCluster *infrav1.ProxmoxCluster) {
	key := client.ObjectKey{Namespace: proxmoxCluster.Namespace, Name: proxmoxCluster.Name}
	Eventually(func() bool {
		err := testEnv.Get(testEnv.GetContext(), key, proxmoxCluster)
		return apierrors.IsNotFound(err)
	}).WithTimeout(time.Second * 10).
		WithPolling(time.Second).
		Should(BeTrue())
}

func cleanup(objs ...client.Object) {
	Expect(testEnv.Cleanup(testEnv.GetContext(), objs...)).To(Succeed())
}
