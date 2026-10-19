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

// Package controller implements controller types.
package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	clusterutil "sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/consts"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/kubernetes/ipam"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/scope"
)

const (
	// ControlPlaneEndpointPort default API server port.
	ControlPlaneEndpointPort = 6443
)

// ProxmoxClusterReconciler reconciles a ProxmoxCluster object.
type ProxmoxClusterReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Recorder      record.EventRecorder
	ProxmoxClient proxmox.Client
}

// SetupWithManager sets up the controller with the Manager.
func (r *ProxmoxClusterReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.ProxmoxCluster{}).
		WithEventFilter(predicates.ResourceNotPaused(r.Scheme, ctrl.LoggerFrom(ctx))).
		Watches(&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(clusterutil.ClusterToInfrastructureMapFunc(ctx, infrav1.GroupVersion.WithKind(infrav1.ProxmoxClusterKind), mgr.GetClient(), &infrav1.ProxmoxCluster{})),
			builder.WithPredicates(predicates.ClusterUnpaused(r.Scheme, ctrl.LoggerFrom(ctx)))).
		WithEventFilter(predicates.ResourceIsNotExternallyManaged(r.Scheme, ctrl.LoggerFrom(ctx))).
		Complete(r)
}

// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=proxmoxclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=proxmoxclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=proxmoxclusters/finalizers,verbs=update

// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch;patch

// +kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=inclusterippools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=globalinclusterippools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=ipaddresses,verbs=get;list;watch
// +kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=ipaddressclaims,verbs=get;list;watch;create;update;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.4/pkg/reconcile
func (r *ProxmoxClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	logger := log.FromContext(ctx)

	proxmoxCluster := &infrav1.ProxmoxCluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, proxmoxCluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Get owner cluster
	cluster, err := clusterutil.GetOwnerCluster(ctx, r.Client, proxmoxCluster.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cluster == nil {
		logger.Info("Waiting for Cluster Controller to set OwnerRef on ProxmoxCluster")
		return ctrl.Result{}, nil
	}

	logger = logger.WithValues("cluster", klog.KObj(cluster))
	ctx = ctrl.LoggerInto(ctx, logger)

	if annotations.IsPaused(cluster, proxmoxCluster) {
		logger.Info("ProxmoxCluster or owning Cluster is marked as paused, not reconciling")

		return ctrl.Result{}, nil
	}

	// Create the scope.
	clusterScope, err := scope.NewClusterScope(scope.ClusterScopeParams{
		Client:         r.Client,
		Logger:         &logger,
		Cluster:        cluster,
		ProxmoxCluster: proxmoxCluster,
		ControllerName: "proxmoxcluster",
		ProxmoxClient:  r.ProxmoxClient,
		IPAMHelper:     ipam.NewHelper(r.Client, proxmoxCluster.DeepCopy()),
	})
	if err != nil {
		return reconcile.Result{}, errors.Errorf("failed to create scope: %+v", err)
	}

	// Always close the scope when exiting this function so we can persist any ProxmoxCluster changes.
	defer func() {
		if err := clusterScope.Close(); err != nil && reterr == nil {
			reterr = err
		}
	}()

	// Handle deleted clusters
	if !proxmoxCluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, clusterScope)
	}

	// Handle non-deleted clusters
	return r.reconcileNormal(ctx, clusterScope)
}

func (r *ProxmoxClusterReconciler) reconcileDelete(ctx context.Context, clusterScope *scope.ClusterScope) (reconcile.Result, error) {
	// We want to prevent deletion unless the owning cluster was flagged for deletion.
	if clusterScope.Cluster.DeletionTimestamp.IsZero() {
		clusterScope.Error(errors.New("deletion was requested but owning cluster wasn't deleted"), "Unable to delete ProxmoxCluster")
		// We stop reconciling here. It will be triggered again once the owning cluster was deleted.
		return reconcile.Result{}, nil
	}

	clusterScope.Logger.V(4).Info("Reconciling ProxmoxCluster delete")
	// Deletion usually should be triggered through the deletion of the owning cluster.
	// If the ProxmoxCluster was also flagged for deletion (e.g. deletion using the manifest file)
	// we should only allow to remove the finalizer when there are no ProxmoxMachines left.
	machines, err := clusterScope.ListProxmoxMachinesForCluster(ctx)
	if err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "could not retrieve proxmox machines for cluster %q", clusterScope.InfraClusterName())
	}

	// Requeue if there are one or more machines left.
	if len(machines) > 0 {
		clusterScope.Info("waiting for machines to be deleted", "remaining", len(machines))
		return ctrl.Result{RequeueAfter: infrav1.DefaultReconcilerRequeue}, nil
	}

	if err := r.reconcileDeleteCredentialsSecret(ctx, clusterScope); err != nil {
		return reconcile.Result{}, err
	}

	clusterScope.Info("cluster deleted successfully")
	ctrlutil.RemoveFinalizer(clusterScope.ProxmoxCluster, infrav1.ClusterFinalizer)
	return ctrl.Result{}, nil
}

func (r *ProxmoxClusterReconciler) reconcileNormal(ctx context.Context, clusterScope *scope.ClusterScope) (reconcile.Result, error) {
	clusterScope.Logger.Info("Reconciling ProxmoxCluster")

	// If the ProxmoxCluster doesn't have our finalizer, add it.
	ctrlutil.AddFinalizer(clusterScope.ProxmoxCluster, infrav1.ClusterFinalizer)

	if ptr.Deref(clusterScope.ProxmoxCluster.Spec.ExternalManagedControlPlane, false) {
		if clusterScope.ProxmoxCluster.Spec.ControlPlaneEndpoint.IsZero() {
			clusterScope.Logger.Info("ProxmoxCluster is not ready, missing or waiting for a ControlPlaneEndpoint")

			conditions.Set(clusterScope.ProxmoxCluster, metav1.Condition{
				Type:    infrav1.ProxmoxClusterProxmoxAvailableCondition,
				Status:  metav1.ConditionFalse,
				Reason:  infrav1.ProxmoxClusterProxmoxAvailableMissingControlPlaneEndpointReason,
				Message: "The ProxmoxCluster is missing or waiting for a ControlPlaneEndpoint",
			})

			return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
		}
	}

	res, err := r.reconcileIPAM(ctx, clusterScope)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !res.IsZero() {
		return res, nil
	}

	r.reconcileFailureDomains(clusterScope)

	if err := r.reconcileNormalCredentialsSecret(ctx, clusterScope); err != nil {
		reason := infrav1.ProxmoxClusterProxmoxAvailableProxmoxUnreachableReason
		if apierrors.IsNotFound(err) {
			reason = infrav1.ProxmoxClusterProxmoxAvailableCredentialsNotFoundReason
		}
		conditions.Set(clusterScope.ProxmoxCluster, metav1.Condition{
			Type:    infrav1.ProxmoxClusterProxmoxAvailableCondition,
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: err.Error(),
		})
		return reconcile.Result{}, err
	}

	conditions.Set(clusterScope.ProxmoxCluster, metav1.Condition{
		Type:   infrav1.ProxmoxClusterProxmoxAvailableCondition,
		Status: metav1.ConditionTrue,
		Reason: clusterv1.ProvisionedReason,
	})

	clusterScope.SetReady()

	return ctrl.Result{}, nil
}

func (r *ProxmoxClusterReconciler) reconcileIPAM(ctx context.Context, clusterScope *scope.ClusterScope) (reconcile.Result, error) {
	if err := clusterScope.IPAMHelper.CreateOrUpdateInClusterIPPool(ctx); err != nil {
		if errors.Is(err, ipam.ErrMissingAddresses) {
			clusterScope.Info("Missing addresses in cluster IPAM config, not reconciling")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	proxmoxCluster := clusterScope.ProxmoxCluster
	ipPools := []string{}
	if proxmoxCluster.Spec.IPv4Config != nil {
		ipPools = append(ipPools, ipam.InClusterPoolFormat(proxmoxCluster, nil, infrav1.IPv4Format))
	}
	if proxmoxCluster.Spec.IPv6Config != nil {
		ipPools = append(ipPools, ipam.InClusterPoolFormat(proxmoxCluster, nil, infrav1.IPv6Format))
	}
	for _, zone := range proxmoxCluster.Spec.ZoneConfigs {
		if zone.IPv4Config != nil {
			ipPools = append(ipPools, ipam.InClusterPoolFormat(proxmoxCluster, zone.Zone, infrav1.IPv4Format))
		}
		if zone.IPv6Config != nil {
			ipPools = append(ipPools, ipam.InClusterPoolFormat(proxmoxCluster, zone.Zone, infrav1.IPv6Format))
		}
	}

	for _, poolName := range ipPools {
		pool, err := clusterScope.IPAMHelper.GetIPPool(ctx, corev1.TypedLocalObjectReference{
			APIGroup: consts.GetIPAMInClusterAPIGroup(),
			Name:     poolName,
			Kind:     consts.GetInClusterIPPoolKind(),
		})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{RequeueAfter: infrav1.DefaultReconcilerRequeue}, nil
			}

			return ctrl.Result{}, err
		}
		clusterScope.ProxmoxCluster.SetInClusterIPPoolRef(pool)
	}

	return reconcile.Result{}, nil
}

// reconcileFailureDomains populates Status.FailureDomains from ZoneConfigs.
// When no zones are configured, FailureDomains is set to nil, preserving
// backwards compatibility (KCP will not attempt failure domain placement).
func (r *ProxmoxClusterReconciler) reconcileFailureDomains(clusterScope *scope.ClusterScope) {
	pc := clusterScope.ProxmoxCluster
	if len(pc.Spec.ZoneConfigs) == 0 {
		pc.Status.FailureDomains = nil
		return
	}

	faildoms := make([]clusterv1.FailureDomain, 0, len(pc.Spec.ZoneConfigs))
	for _, zc := range pc.Spec.ZoneConfigs {
		zoneName := ptr.Deref(zc.Zone, "")
		if zoneName == "" {
			continue
		}

		faildoms = append(faildoms, clusterv1.FailureDomain{
			Name:         zoneName,
			ControlPlane: new(ptr.Deref(zc.ControlPlaneEligible, true)),
		})
	}

	sort.Slice(faildoms, func(i, j int) bool { return faildoms[i].Name < faildoms[j].Name })
	pc.Status.FailureDomains = faildoms
}

// reconcileNormalCredentialsSecret makes the ProxmoxCluster an owner of every
// credentials Secret it reads, its own and each zone's, and protects each
// with the Secret finalizer so none can be deleted under the machines that
// are reached through it. A Secret the cluster owns but no longer reads,
// because its zone or reference went, is released again.
func (r *ProxmoxClusterReconciler) reconcileNormalCredentialsSecret(ctx context.Context, clusterScope *scope.ClusterScope) error {
	proxmoxCluster := clusterScope.ProxmoxCluster
	ownerRef := credentialsOwnerRef(proxmoxCluster)
	referenced := make(map[client.ObjectKey]bool)
	for _, secretKey := range credentialsSecretKeys(proxmoxCluster) {
		referenced[secretKey] = true
		secret := &corev1.Secret{}
		if err := r.Client.Get(ctx, secretKey, secret); err != nil {
			return err
		}

		helper, err := patch.NewHelper(secret, r.Client)
		if err != nil {
			return err
		}

		// Ensure the ProxmoxCluster is an owner and that the APIVersion is up-to-date.
		secret.SetOwnerReferences(clusterutil.EnsureOwnerRef(secret.GetOwnerReferences(), ownerRef))

		// Ensure the finalizer is added.
		if !ctrlutil.ContainsFinalizer(secret, infrav1.SecretFinalizer) {
			ctrlutil.AddFinalizer(secret, infrav1.SecretFinalizer)
		}

		if err := helper.Patch(ctx, secret); err != nil {
			return err
		}
	}

	return r.releaseUnreferencedCredentialsSecrets(ctx, proxmoxCluster, referenced)
}

// releaseUnreferencedCredentialsSecrets releases the Secrets of the
// ProxmoxCluster's namespace that carry its owner reference but are no
// longer among the ones it reads credentials from: a zone or a reference
// dropped from the spec must not leave its Secret owned and finalized, or a
// later deletion of the cluster would leave it terminating forever.
func (r *ProxmoxClusterReconciler) releaseUnreferencedCredentialsSecrets(ctx context.Context, proxmoxCluster *infrav1.ProxmoxCluster, referenced map[client.ObjectKey]bool) error {
	ownerRef := credentialsOwnerRef(proxmoxCluster)
	secrets := &corev1.SecretList{}
	if err := r.Client.List(ctx, secrets, client.InNamespace(proxmoxCluster.GetNamespace())); err != nil {
		return err
	}
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		if referenced[client.ObjectKeyFromObject(secret)] || !clusterutil.HasOwnerRef(secret.GetOwnerReferences(), ownerRef) {
			continue
		}
		// The cluster lives on without this Secret: the owner reference goes
		// too, or the Secret would be collected with the cluster later.
		if err := r.releaseCredentialsSecret(ctx, secret, ownerRef, true); err != nil {
			return err
		}
	}
	return nil
}

// reconcileDeleteCredentialsSecret releases every credentials Secret the
// ProxmoxCluster owns, its own and each zone's: the owner reference goes, and
// the finalizer with it when no other owner remains.
func (r *ProxmoxClusterReconciler) reconcileDeleteCredentialsSecret(ctx context.Context, clusterScope *scope.ClusterScope) error {
	proxmoxCluster := clusterScope.ProxmoxCluster
	ownerRef := credentialsOwnerRef(proxmoxCluster)

	for _, secretKey := range credentialsSecretKeys(proxmoxCluster) {
		secret := &corev1.Secret{}
		if err := r.Client.Get(ctx, secretKey, secret); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if err := r.releaseCredentialsSecret(ctx, secret, ownerRef, false); err != nil {
			return err
		}
	}

	return nil
}

// releaseCredentialsSecret takes the ProxmoxCluster off a Secret it owns:
// the owner reference goes when other owners remain, and the finalizer goes
// when the ProxmoxCluster was the only owner. On deletion the sole owner
// reference stays, so the Secret is collected with the cluster as before;
// dropOwnerRef removes it as well, for a Secret the living cluster stopped
// reading.
func (r *ProxmoxClusterReconciler) releaseCredentialsSecret(ctx context.Context, secret *corev1.Secret, ownerRef metav1.OwnerReference, dropOwnerRef bool) error {
	helper, err := patch.NewHelper(secret, r.Client)
	if err != nil {
		return err
	}

	switch {
	case len(secret.GetOwnerReferences()) > 1:
		// Remove the ProxmoxCluster from the OwnerRef.
		secret.SetOwnerReferences(clusterutil.RemoveOwnerRef(secret.GetOwnerReferences(), ownerRef))
	case clusterutil.HasOwnerRef(secret.GetOwnerReferences(), ownerRef):
		// There is only one OwnerRef, the current ProxmoxCluster. Remove the Finalizer (if present).
		if ctrlutil.ContainsFinalizer(secret, infrav1.SecretFinalizer) {
			ctrl.LoggerFrom(ctx).Info(fmt.Sprintf("Removing finalizer %s", infrav1.SecretFinalizer), "Secret", klog.KObj(secret))
			ctrlutil.RemoveFinalizer(secret, infrav1.SecretFinalizer)
		}
		if dropOwnerRef {
			secret.SetOwnerReferences(clusterutil.RemoveOwnerRef(secret.GetOwnerReferences(), ownerRef))
		}
	}

	return helper.Patch(ctx, secret)
}

// credentialsOwnerRef is the owner reference the ProxmoxCluster sets on the
// credentials Secrets it reads.
func credentialsOwnerRef(proxmoxCluster *infrav1.ProxmoxCluster) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: infrav1.GroupVersion.String(),
		Kind:       "ProxmoxCluster",
		Name:       proxmoxCluster.Name,
		UID:        proxmoxCluster.UID,
	}
}

// credentialsSecretKeys lists every Secret the ProxmoxCluster reads
// credentials from, its own credentialsRef first and then each zone's, each
// once; a reference without a namespace is in the ProxmoxCluster's.
func credentialsSecretKeys(proxmoxCluster *infrav1.ProxmoxCluster) []client.ObjectKey {
	if proxmoxCluster == nil {
		return nil
	}
	refs := make([]*corev1.SecretReference, 0, 1+len(proxmoxCluster.Spec.ZoneConfigs))
	refs = append(refs, proxmoxCluster.Spec.CredentialsRef)
	for _, zc := range proxmoxCluster.Spec.ZoneConfigs {
		refs = append(refs, zc.CredentialsRef)
	}

	keys := make([]client.ObjectKey, 0, len(refs))
	seen := make(map[client.ObjectKey]bool, len(refs))
	for _, ref := range refs {
		if ref == nil || ref.Name == "" {
			continue
		}
		key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
		if key.Namespace == "" {
			key.Namespace = proxmoxCluster.GetNamespace()
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	return keys
}
