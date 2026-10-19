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

// Package scope defines the capmox scopes used for reconciliation.
package scope

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/go-logr/logr"
	"github.com/luthermonson/go-proxmox"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/internal/tlshelper"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/kubernetes/ipam"
	capmox "github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox/goproxmox"
)

// ClusterScopeParams defines the input parameters used to create a new Scope.
type ClusterScopeParams struct {
	Client         client.Client
	Logger         *logr.Logger
	Cluster        *clusterv1.Cluster
	ProxmoxCluster *infrav1.ProxmoxCluster
	ProxmoxClient  capmox.Client
	ControllerName string
	IPAMHelper     *ipam.Helper
}

// ClusterScope defines the basic context for an actuator to operate upon.
type ClusterScope struct {
	*logr.Logger
	client      client.Client
	patchHelper *patch.Helper

	Cluster        *clusterv1.Cluster
	ProxmoxCluster *infrav1.ProxmoxCluster

	ProxmoxClient  capmox.Client
	controllerName string

	// newClient builds a client from a credentials Secret reference;
	// newProxmoxClient unless a test injects one.
	newClient func(ctx context.Context, credentialsRef *corev1.SecretReference) (capmox.Client, error)

	IPAMHelper *ipam.Helper
}

// NewClusterScope creates a new Scope from the supplied parameters.
// This is meant to be called for each reconcile iteration.
func NewClusterScope(params ClusterScopeParams) (*ClusterScope, error) {
	if params.Client == nil {
		return nil, errors.New("Client is required when creating a ClusterScope")
	}
	if params.Cluster == nil {
		return nil, errors.New("Cluster is required when creating a ClusterScope")
	}
	if params.ProxmoxCluster == nil {
		return nil, errors.New("ProxmoxCluster is required when creating a ClusterScope")
	}
	if params.IPAMHelper == nil {
		return nil, errors.New("IPAMHelper is required when creating a ClusterScope")
	}
	if params.Logger == nil {
		logger := log.FromContext(context.Background())
		params.Logger = &logger
	}

	clusterScope := &ClusterScope{
		Logger:         params.Logger,
		client:         params.Client,
		Cluster:        params.Cluster,
		ProxmoxCluster: params.ProxmoxCluster,
		controllerName: params.ControllerName,
		ProxmoxClient:  params.ProxmoxClient,
		IPAMHelper:     params.IPAMHelper,
	}

	helper, err := patch.NewHelper(params.ProxmoxCluster, params.Client)
	if err != nil {
		return nil, errors.Wrap(err, "failed to init patch helper")
	}

	clusterScope.patchHelper = helper

	if clusterScope.ProxmoxClient == nil {
		switch {
		case clusterScope.ProxmoxCluster.Spec.CredentialsRef != nil:
			// using proxmoxcluster.spec.credentialsRef
			pmoxClient, err := clusterScope.newProxmoxClient(context.TODO(), clusterScope.ProxmoxCluster.Spec.CredentialsRef)
			if err != nil {
				return nil, errors.Wrap(err, "Unable to initialize ProxmoxClient")
			}
			clusterScope.ProxmoxClient = pmoxClient
		case defaultProxmoxClientNeeded(clusterScope.ProxmoxCluster):
			// Fail the cluster if no credentials found.
			conditions.Set(clusterScope.ProxmoxCluster, metav1.Condition{
				Type:    infrav1.ProxmoxClusterProxmoxAvailableCondition,
				Status:  metav1.ConditionFalse,
				Reason:  infrav1.ProxmoxClusterProxmoxAvailableCredentialsNotFoundReason,
				Message: "No credentials found, ProxmoxCluster missing credentialsRef",
			})

			if err = clusterScope.Close(); err != nil {
				return nil, err
			}
			return nil, errors.New("No credentials found, ProxmoxCluster missing credentialsRef")
		}
	}

	return clusterScope, nil
}

// defaultProxmoxClientNeeded reports whether the ProxmoxCluster needs a client
// of its own: it does unless every zone it declares carries its own
// credentialsRef, in which case every machine placed in a zone is reached
// through that zone's endpoint. A machine placed in no zone still needs the
// default client, and ProxmoxClientForZone refuses it when there is none.
func defaultProxmoxClientNeeded(proxmoxCluster *infrav1.ProxmoxCluster) bool {
	if len(proxmoxCluster.Spec.ZoneConfigs) == 0 {
		return true
	}
	for _, zc := range proxmoxCluster.Spec.ZoneConfigs {
		if zc.CredentialsRef == nil {
			return true
		}
	}
	return false
}

// zonesWithOwnCredentials reports whether any zone of the ProxmoxCluster
// carries its own credentialsRef: once one does, a machine's endpoint is
// decided by its zone and an undeclared zone cannot fall back to the
// ProxmoxCluster's client without risking the wrong Proxmox cluster.
func zonesWithOwnCredentials(proxmoxCluster *infrav1.ProxmoxCluster) bool {
	for _, zc := range proxmoxCluster.Spec.ZoneConfigs {
		if zc.CredentialsRef != nil {
			return true
		}
	}
	return false
}

// ProxmoxClientForZone returns the client that reaches the Proxmox cluster
// backing the named zone: a client built from the zone's own credentialsRef,
// or the ProxmoxCluster's client when the zone is empty, is the default
// zone, or declares no credentials of its own. It fails when the
// ProxmoxCluster's client is needed and none was configured, and when the
// zone is not declared while another zone carries its own credentials: that
// machine's VM lives on an endpoint the ProxmoxCluster can no longer name,
// and reaching another one would find, or delete, the wrong VM.
//
// The scope lives for one reconcile, so a zone's client is built once per
// machine reconcile, as the ProxmoxCluster's own client is.
func (s *ClusterScope) ProxmoxClientForZone(ctx context.Context, zone string) (capmox.Client, error) {
	zc := s.ProxmoxCluster.GetZoneConfig(zone)
	if zc != nil && zc.CredentialsRef != nil {
		newClient := s.newClient
		if newClient == nil {
			newClient = s.newProxmoxClient
		}
		pmoxClient, err := newClient(ctx, zc.CredentialsRef)
		if err != nil {
			return nil, errors.Wrapf(err, "unable to initialize ProxmoxClient for zone %q", zone)
		}
		return pmoxClient, nil
	}

	if zc == nil && zone != "" && zone != infrav1.DefaultZone && zonesWithOwnCredentials(s.ProxmoxCluster) {
		return nil, errors.Errorf("zone %q is not declared by the ProxmoxCluster while other zones carry their own credentialsRef: the machine cannot be reached until the zone is declared again", zone)
	}

	if s.ProxmoxClient == nil && !defaultProxmoxClientNeeded(s.ProxmoxCluster) {
		// The cluster runs without a default client because every zone
		// reaches its own endpoint, and this machine is in none of them.
		return nil, errors.Errorf("no ProxmoxClient reaches zone %q: every zone of the ProxmoxCluster carries its own credentialsRef and the machine is placed in none of them", zone)
	}
	return s.ProxmoxClient, nil
}

// newProxmoxClient builds a client from the credentials Secret the reference
// names; a reference without a namespace is read from the ProxmoxCluster's.
func (s *ClusterScope) newProxmoxClient(ctx context.Context, credentialsRef *corev1.SecretReference) (capmox.Client, error) {
	// get the credentials secret
	secret := corev1.Secret{}
	namespace := credentialsRef.Namespace
	if len(namespace) == 0 {
		namespace = s.ProxmoxCluster.GetNamespace()
	}
	err := s.client.Get(ctx, client.ObjectKey{
		Namespace: namespace,
		Name:      credentialsRef.Name,
	}, &secret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			conditions.Set(s.ProxmoxCluster, metav1.Condition{
				Type:    infrav1.ProxmoxClusterProxmoxAvailableCondition,
				Status:  metav1.ConditionFalse,
				Reason:  infrav1.ProxmoxClusterProxmoxAvailableCredentialsNotFoundReason,
				Message: fmt.Sprintf("credentials secret %s/%s not found", namespace, credentialsRef.Name),
			})
		}
		return nil, errors.Wrap(err, "failed to get credentials secret")
	}

	token := string(secret.Data["token"])
	tokenSecret := string(secret.Data["secret"])
	url := string(secret.Data["url"])

	tlsInsecure, tlsInsecureSet := secret.Data["insecure"]
	tlsRootCA := secret.Data["root_ca"]

	rootCerts, err := tlshelper.SystemRootsWithCert(tlsRootCA)
	if err != nil {
		return nil, fmt.Errorf("loading cert pool: %w", err)
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			// When "insecure" is unset we retain the pre-v0.7 behavior of
			// setting the connection insecure. If it is set we compare
			// against YAML true-ish values.
			//
			// #nosec:G402 // Intended to enable insecure mode for unknown CAs
			InsecureSkipVerify: !tlsInsecureSet || slices.Contains([]string{"1", "on", "true", "yes", "y"}, strings.ToLower(string(tlsInsecure))),
			RootCAs:            rootCerts,
		},
	}

	httpClient := &http.Client{Transport: tr}
	return goproxmox.NewAPIClient(ctx, *s.Logger, url,
		proxmox.WithHTTPClient(httpClient),
		proxmox.WithAPIToken(token, tokenSecret),
	)
}

// Name returns the CAPI cluster name.
func (s *ClusterScope) Name() string {
	return s.Cluster.Name
}

// Namespace returns the cluster namespace.
func (s *ClusterScope) Namespace() string {
	return s.Cluster.Namespace
}

// InfraClusterName returns the name of the Proxmox cluster.
func (s *ClusterScope) InfraClusterName() string {
	return s.ProxmoxCluster.Name
}

// KubernetesClusterName is the name of the Kubernetes cluster. For the cluster
// scope this is the same as the CAPI cluster name.
func (s *ClusterScope) KubernetesClusterName() string {
	return s.Cluster.Name
}

// PatchObject persists the cluster configuration and status.
func (s *ClusterScope) PatchObject() error {
	// always update the readyCondition.
	_ = conditions.SetSummaryCondition(s.ProxmoxCluster, s.ProxmoxCluster, "Ready",
		conditions.ForConditionTypes{infrav1.ProxmoxClusterProxmoxAvailableCondition},
	)

	return s.patchHelper.Patch(context.TODO(), s.ProxmoxCluster,
		patch.WithOwnedConditions{Conditions: []string{
			"Ready",
			infrav1.ProxmoxClusterProxmoxAvailableCondition,
		}})
}

// ListProxmoxMachinesForCluster returns all the ProxmoxMachines that belong to this cluster.
func (s *ClusterScope) ListProxmoxMachinesForCluster(ctx context.Context) ([]infrav1.ProxmoxMachine, error) {
	var machineList infrav1.ProxmoxMachineList

	err := s.client.List(ctx, &machineList, client.InNamespace(s.Namespace()), client.MatchingLabels{
		clusterv1.ClusterNameLabel: s.Name(),
	})
	if err != nil {
		return nil, err
	}

	return machineList.Items, nil
}

// Close closes the current scope persisting the cluster configuration and status.
func (s *ClusterScope) Close() error {
	return s.PatchObject()
}

// SetReady sets the ProxmoxCluster as provisioned.
func (s *ClusterScope) SetReady() {
	s.ProxmoxCluster.Status.Initialization.Provisioned = new(true)
}
