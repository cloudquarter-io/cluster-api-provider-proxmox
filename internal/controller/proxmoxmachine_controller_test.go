/*
Copyright 2024-2026 IONOS Cloud.

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
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/internal/service/vmservice"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/scope"
)

var _ = Describe("ProxmoxMachineReconciler", func() {
	BeforeEach(func() {})
	AfterEach(func() {})

	Context("Reconcile an ProxmoxMachine", func() {
		It("should not error with minimal set up", func() {
			reconciler := &ProxmoxMachineReconciler{
				Client:        k8sClient,
				Scheme:        runtime.NewScheme(),
				ProxmoxClient: proxmoxClient,
			}
			By("Calling reconcile")
			ctx := context.Background()
			instance := &infrav1.ProxmoxMachine{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "default"}}
			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: client.ObjectKey{
					Namespace: instance.Namespace,
					Name:      instance.Name,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
		})
	})

	Context("Requeue a ready ProxmoxMachine", func() {
		It("should requeue after the jittered location resync, and never when it is zero", func() {
			Expect((&ProxmoxMachineReconciler{}).readyResult().RequeueAfter).To(BeZero())
			resync := time.Minute
			for range 20 {
				after := (&ProxmoxMachineReconciler{LocationResync: resync}).readyResult().RequeueAfter
				Expect(after).To(BeNumerically(">=", resync))
				Expect(after).To(BeNumerically("<=", resync+resync/5))
			}
		})
	})

	Context("Reconcile a ProxmoxMachine whose VM failed in a way a new VM can fix", func() {
		retrying := func() (*scope.MachineScope, *scope.ClusterScope) {
			logger := logr.Discard()
			pm := &infrav1.ProxmoxMachine{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "default"}}
			conditions.Set(pm, metav1.Condition{
				Type:   infrav1.ProxmoxMachineVirtualMachineProvisionedCondition,
				Status: metav1.ConditionFalse,
				Reason: infrav1.ProxmoxMachineVirtualMachineProvisionedVMProvisionRetryingReason,
			})
			return &scope.MachineScope{Logger: &logger, Machine: &clusterv1.Machine{}, ProxmoxMachine: pm}, &scope.ClusterScope{Logger: &logger}
		}

		It("should wait the backoff before replacing the VM", func() {
			machineScope, clusterScope := retrying()
			reconciler := &ProxmoxMachineReconciler{Reprovision: vmservice.ReprovisionPolicy{Limit: 3, Backoff: 30 * time.Second}}
			result, err := reconciler.reconcileNormal(context.Background(), machineScope, clusterScope)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically("~", 30*time.Second, time.Second))
			Expect(machineScope.ProxmoxMachine.Status.RetryAfter).NotTo(BeNil())
		})

		It("should make the failure final when retries are disabled", func() {
			machineScope, clusterScope := retrying()
			result, err := (&ProxmoxMachineReconciler{}).reconcileNormal(context.Background(), machineScope, clusterScope)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			Expect(machineScope.HasFailed()).To(BeTrue())
		})
	})
})
