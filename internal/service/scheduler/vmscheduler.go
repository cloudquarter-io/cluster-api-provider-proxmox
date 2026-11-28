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

// Package scheduler implements scheduling algorithms for Proxmox VMs.
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/go-logr/logr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cluster-api/util"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/scope"
)

// InsufficientMemoryError is used when the scheduler cannot assign a VM to a node because it would
// exceed the node's memory limit.
type InsufficientMemoryError struct {
	node      string
	available uint64
	requested uint64
}

func (err InsufficientMemoryError) Error() string {
	return fmt.Sprintf("cannot reserve %dB of memory on node %s: %dB available memory left",
		err.requested, err.node, err.available)
}

// NoFreeNodeError is returned for a control plane when nodes free of its
// cluster's control planes exist but none has room. Unlike
// InsufficientMemoryError it does not fail the machine: the reconcile is
// retried until one of them has room.
type NoFreeNodeError struct {
	node      string
	available uint64
	requested uint64
}

func (err NoFreeNodeError) Error() string {
	return fmt.Sprintf("waiting for a node free of the cluster's control planes: cannot reserve %dB of memory on node %s, the one with the most: %dB available memory left",
		err.requested, err.node, err.available)
}

// ScheduleVM decides which node to a ProxmoxMachine should be scheduled on.
// It requires the machine's ProxmoxCluster to have at least 1 allowed node.
func ScheduleVM(ctx context.Context, machineScope *scope.MachineScope) (string, error) {
	client := machineScope.ProxmoxClient()
	cluster := machineScope.InfraCluster.ProxmoxCluster
	schedulerHints := cluster.Spec.SchedulerHints
	controlPlane := util.IsControlPlaneMachine(machineScope.Machine)
	locations := cluster.Status.NodeLocations.Workers
	if controlPlane {
		locations = onEndpoint(cluster, ptr.Deref(machineScope.Zone(), ""), cluster.Status.NodeLocations.ControlPlane)
	}

	return selectNode(ctx, client, machineScope.ProxmoxMachine, locations, machineScope.AllowedNodes(), schedulerHints, controlPlane)
}

func selectNode(
	ctx context.Context,
	client resourceClient,
	machine *infrav1.ProxmoxMachine,
	locations []infrav1.NodeLocation,
	allowedNodes []string,
	schedulerHints *infrav1.SchedulerHints,
	controlPlane bool,
) (string, error) {
	byMemory := make(sortByAvailableMemory, len(allowedNodes))
	for i, nodeName := range allowedNodes {
		mem, err := client.GetReservableMemoryBytes(ctx, nodeName, schedulerHints.GetMemoryAdjustment())
		if err != nil {
			return "", err
		}
		byMemory[i] = nodeInfo{Name: nodeName, AvailableMemory: mem}
	}

	sort.Sort(byMemory)

	requestedMemory := uint64(ptr.Deref(machine.Spec.MemoryMiB, 0)) * 1024 * 1024 // convert to bytes
	if requestedMemory > byMemory[0].AvailableMemory {
		// no more space on the node with the highest amount of available memory
		return "", InsufficientMemoryError{
			node:      byMemory[0].Name,
			available: byMemory[0].AvailableMemory,
			requested: requestedMemory,
		}
	}

	// count the existing vms per node
	nodeCounter := make(map[string]int)
	for _, nl := range locations {
		nodeCounter[nl.Node]++
	}

	for i, info := range byMemory {
		info.ScheduledVMs = nodeCounter[info.Name]
		byMemory[i] = info
	}

	byReplicas := make(sortByReplicas, len(byMemory))
	copy(byReplicas, byMemory)

	sort.Sort(byReplicas)

	var decision string
	if free := withoutReplicas(byMemory); controlPlane && len(free) > 0 {
		// A control plane never joins a node that holds another of its cluster's
		// while a node free of them exists: if none of those has room, it waits.
		// Two control planes on one node fail together, and Proxmox HA negative
		// resource affinity would live-migrate one of them, the serving one
		// included, as soon as the new one is registered.
		if requestedMemory >= free[0].AvailableMemory {
			return "", NoFreeNodeError{
				node:      free[0].Name,
				available: free[0].AvailableMemory,
				requested: requestedMemory,
			}
		}
		decision = free[0].Name
	} else {
		decision = byMemory[0].Name
		for _, info := range byReplicas {
			// distribute round-robin when memory allows it
			if requestedMemory < info.AvailableMemory {
				decision = info.Name
				break
			}
		}
	}

	if logger := logr.FromContextOrDiscard(ctx); logger.V(4).Enabled() {
		// only construct values when message should actually be logged
		logger.Info("Scheduler decision",
			"byReplicas", byReplicas.String(),
			"byMemory", byMemory.String(),
			"requestedMemory", requestedMemory,
			"resultNode", decision,
		)
	}

	return decision, nil
}

// onEndpoint keeps the locations on the Proxmox cluster the zone reaches:
// node names repeat across Proxmox clusters, so a control plane elsewhere
// says nothing about a node here. Zones sharing a credentialsRef share a
// Proxmox cluster; a zone without one, and a location without a zone, reach
// the ProxmoxCluster's.
func onEndpoint(cluster *infrav1.ProxmoxCluster, zone string, locations []infrav1.NodeLocation) []infrav1.NodeLocation {
	want := endpointOf(cluster, zone)
	var kept []infrav1.NodeLocation
	for _, loc := range locations {
		if endpointOf(cluster, ptr.Deref(loc.Zone, "")) == want {
			kept = append(kept, loc)
		}
	}
	return kept
}

// endpointOf names the credentials Secret that reaches the zone's Proxmox
// cluster, "" for the controller's own credentials.
func endpointOf(cluster *infrav1.ProxmoxCluster, zone string) string {
	ref := cluster.Spec.CredentialsRef
	if zc := cluster.GetZoneConfig(zone); zc != nil && zc.CredentialsRef != nil {
		ref = zc.CredentialsRef
	}
	if ref == nil {
		return ""
	}
	namespace := ref.Namespace
	if namespace == "" {
		namespace = cluster.Namespace
	}
	return namespace + "/" + ref.Name
}

// withoutReplicas returns the nodes that hold none of the scheduled VMs, in
// the order given.
func withoutReplicas(nodes []nodeInfo) []nodeInfo {
	var free []nodeInfo
	for _, info := range nodes {
		if info.ScheduledVMs == 0 {
			free = append(free, info)
		}
	}
	return free
}

type resourceClient interface {
	GetReservableMemoryBytes(context.Context, string, int64) (uint64, error)
}

type nodeInfo struct {
	Name            string `json:"node"`
	AvailableMemory uint64 `json:"mem"`
	ScheduledVMs    int    `json:"vms"`
}

type sortByReplicas []nodeInfo

func (a sortByReplicas) Len() int      { return len(a) }
func (a sortByReplicas) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a sortByReplicas) Less(i, j int) bool {
	return a[i].ScheduledVMs < a[j].ScheduledVMs
}

func (a sortByReplicas) String() string {
	o, _ := json.Marshal(a)
	return string(o)
}

type sortByAvailableMemory []nodeInfo

func (a sortByAvailableMemory) Len() int      { return len(a) }
func (a sortByAvailableMemory) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a sortByAvailableMemory) Less(i, j int) bool {
	// more available memory = lower index
	return a[i].AvailableMemory > a[j].AvailableMemory
}

func (a sortByAvailableMemory) String() string {
	o, _ := json.Marshal(a)
	return string(o)
}
