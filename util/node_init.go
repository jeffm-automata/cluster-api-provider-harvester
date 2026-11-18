/*
Copyright 2025 SUSE.

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

package util

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// uninitializedTaintKey is the taint added by kubelet when cloudProviderName=external.
	uninitializedTaintKey = "node.cloudprovider.kubernetes.io/uninitialized"
)

// InitializeWorkloadNode sets the providerID and removes the cloud-provider
// uninitialized taint on a workload cluster node. This bypasses the
// chicken-and-egg problem where the cloud-provider-harvester pod cannot
// schedule because CNI is blocked by the uninitialized taint.
//
// This is a best-effort operation: all errors are logged as warnings
// and do not propagate, so it never blocks the reconcile loop.
func InitializeWorkloadNode(ctx context.Context, logger logr.Logger, workloadConfig *rest.Config, nodeName, providerID string) bool {
	return InitializeWorkloadNodeWithAddresses(ctx, logger, workloadConfig, nodeName, providerID, nil)
}

// InitializeWorkloadNodeWithAddresses does what InitializeWorkloadNode does and, in addition,
// publishes the given addresses in the status of a node that has no IP address there.
//
// A kubelet started with cloud-provider=external and no node IP of its own publishes only its
// host name and leaves the IP addresses to a cloud controller manager. Where none runs (the
// Harvester cloud provider needs an identity with cluster-wide rights on Harvester, which a
// namespace tenant cannot create), the node keeps no InternalIP: its host-network pods have no
// hostIP or podIP, and whatever addresses the kubelet by InternalIP, such as the metrics server
// behind "kubectl top" and autoscaling, cannot reach it. The provider already knows the
// addresses of the VM, so it fills them in, as it does for the providerID. IP addresses that are
// already there are never touched: a kubelet that sets them itself, or a cloud controller
// manager, stays in charge.
func InitializeWorkloadNodeWithAddresses(
	ctx context.Context,
	logger logr.Logger,
	workloadConfig *rest.Config,
	nodeName, providerID string,
	addresses []v1.NodeAddress,
) bool {
	if providerID == "" {
		return false
	}

	clientset, err := kubernetes.NewForConfig(workloadConfig)
	if err != nil {
		logger.Info("Warning: failed to create workload client for node init", "error", err)

		return false
	}

	node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Node not yet registered in workload cluster: report it so the caller requeues
			return false
		}

		logger.Info("Warning: failed to get workload node for init", "error", err, "node", nodeName)

		return false
	}

	needsProviderID := node.Spec.ProviderID == ""
	needsTaintRemoval := hasUninitializedTaint(node)
	needsAddresses := !hasIPAddress(node) && len(addresses) > 0

	if !needsProviderID && !needsTaintRemoval && !needsAddresses {
		return true
	}

	if needsAddresses {
		// Before the taint is removed, so that pods never start on a node without an address.
		if !setNodeAddresses(ctx, logger, clientset, nodeName, addresses) {
			return false
		}
	}

	if needsProviderID {
		patch := fmt.Sprintf(`{"spec":{"providerID":%q}}`, providerID)

		_, err = clientset.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
		if err != nil {
			logger.Info("Warning: failed to set providerID on workload node",
				"error", err, "node", nodeName, "providerID", providerID)

			return false
		}

		logger.Info("Set providerID on workload node", "node", nodeName, "providerID", providerID)
	}

	if needsTaintRemoval {
		// Re-fetch node to avoid conflict after providerID patch
		node, err = clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			logger.Info("Warning: failed to re-fetch node after providerID patch", "error", err)

			return false
		}

		newTaints := removeTaint(node.Spec.Taints)
		if len(newTaints) != len(node.Spec.Taints) {
			taintsJSON, err := json.Marshal(newTaints)
			if err != nil {
				logger.Info("Warning: failed to marshal taints", "error", err, "node", nodeName)

				return false
			}

			patch := fmt.Sprintf(`{"spec":{"taints":%s}}`, taintsJSON)

			_, err = clientset.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
			if err != nil {
				logger.Info("Warning: failed to remove uninitialized taint",
					"error", err, "node", nodeName)

				return false
			}

			logger.Info("Removed cloud-provider uninitialized taint", "node", nodeName)
		}
	}

	return true
}

// hasIPAddress returns true if the node status carries an internal or external IP address.
// The host name alone, which a kubelet without a cloud provider's help publishes, does not count.
func hasIPAddress(node *v1.Node) bool {
	for _, address := range node.Status.Addresses {
		if address.Type == v1.NodeInternalIP || address.Type == v1.NodeExternalIP {
			return true
		}
	}

	return false
}

// setNodeAddresses writes status.addresses of a workload cluster node that has no IP address there.
func setNodeAddresses(ctx context.Context, logger logr.Logger, clientset kubernetes.Interface, nodeName string, addresses []v1.NodeAddress) bool {
	addressesJSON, err := json.Marshal(addresses)
	if err != nil {
		logger.Info("Warning: failed to marshal node addresses", "error", err, "node", nodeName)

		return false
	}

	patch := fmt.Sprintf(`{"status":{"addresses":%s}}`, addressesJSON)

	_, err = clientset.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, []byte(patch), metav1.PatchOptions{}, "status")
	if err != nil {
		logger.Info("Warning: failed to set addresses on workload node", "error", err, "node", nodeName)

		return false
	}

	logger.Info("Set addresses on workload node", "node", nodeName, "addresses", addresses)

	return true
}

// hasUninitializedTaint returns true if the node has the cloud-provider uninitialized taint.
func hasUninitializedTaint(node *v1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == uninitializedTaintKey {
			return true
		}
	}

	return false
}

// removeTaint returns a copy of the taint slice with the uninitialized taint key removed.
func removeTaint(taints []v1.Taint) []v1.Taint {
	result := make([]v1.Taint, 0, len(taints))

	for _, t := range taints {
		if t.Key != uninitializedTaintKey {
			result = append(result, t)
		}
	}

	return result
}
