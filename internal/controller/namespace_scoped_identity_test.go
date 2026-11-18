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

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/harvester/cluster-api-provider-harvester/api/v1beta1"
	hvfake "github.com/harvester/cluster-api-provider-harvester/pkg/clientset/versioned/fake"
)

// forbid makes every request for the given verb and resource fail the way the API server
// answers an identity without the right: 403 Forbidden, whether or not the object exists.
func forbid(verb, resource string) (string, string, k8stesting.ReactionFunc) {
	return verb, resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: action.GetResource().Group, Resource: resource}, "", nil)
	}
}

// A tenant of a shared Harvester has rights in one namespace only: a ServiceAccount with a Role
// there, or a Rancher project member reaching Harvester through the Rancher proxy. Such an
// identity may not read harvester-system, and the controller must still accept it.
var _ = Describe("Harvester connection check with a namespace-scoped identity", func() {
	const targetNamespace = "tenant-ns"

	availableDeployment := func(status corev1.ConditionStatus) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: harvesterDeploymentName, Namespace: harvesterNamespace},
			Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: status},
			}},
		}
	}

	It("accepts an identity that may read the Harvester deployment, as before", func(ctx SpecContext) {
		namespaceScoped, err := checkHarvesterConnection(ctx,
			kubefake.NewSimpleClientset(availableDeployment(corev1.ConditionTrue)), hvfake.NewSimpleClientset(), targetNamespace)
		Expect(err).ToNot(HaveOccurred())
		Expect(namespaceScoped).To(BeFalse())
	})

	It("still reports an unavailable Harvester when the deployment can be read", func(ctx SpecContext) {
		_, err := checkHarvesterConnection(ctx,
			kubefake.NewSimpleClientset(availableDeployment(corev1.ConditionFalse)), hvfake.NewSimpleClientset(), targetNamespace)
		Expect(err).To(MatchError(ContainSubstring("unavailable")))
	})

	It("still fails when the deployment does not exist (not a Harvester cluster)", func(ctx SpecContext) {
		_, err := checkHarvesterConnection(ctx, kubefake.NewSimpleClientset(), hvfake.NewSimpleClientset(), targetNamespace)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the cause stays recognisable through the wrapping")
		Expect(err.Error()).To(ContainSubstring("Harvester deployment not found"))
	})

	It("accepts an identity that is forbidden in harvester-system but may list VMs in the target namespace", func(ctx SpecContext) {
		kube := kubefake.NewSimpleClientset(availableDeployment(corev1.ConditionTrue))
		kube.PrependReactor(forbid("get", "deployments"))

		hv := hvfake.NewSimpleClientset()
		listedNamespaces := []string{}

		hv.PrependReactor("list", "virtualmachines", func(action k8stesting.Action) (bool, runtime.Object, error) {
			listedNamespaces = append(listedNamespaces, action.GetNamespace())

			return false, nil, nil
		})

		namespaceScoped, err := checkHarvesterConnection(ctx, kube, hv, targetNamespace)
		Expect(err).ToNot(HaveOccurred())
		Expect(namespaceScoped).To(BeTrue())
		Expect(listedNamespaces).To(Equal([]string{targetNamespace}), "only the target namespace is read")
	})

	It("rejects an identity that is forbidden in harvester-system and in the target namespace", func(ctx SpecContext) {
		kube := kubefake.NewSimpleClientset()
		kube.PrependReactor(forbid("get", "deployments"))

		hv := hvfake.NewSimpleClientset()
		hv.PrependReactor(forbid("list", "virtualmachines"))

		_, err := checkHarvesterConnection(ctx, kube, hv, targetNamespace)
		Expect(err).To(MatchError(ContainSubstring("nor list VirtualMachines in namespace " + targetNamespace)))
	})
})

var _ = Describe("Target namespace check with a namespace-scoped identity", func() {
	newScope := func(connectionReady bool) (*HarvesterClusterReconciler, *ClusterScope) {
		hv := hvfake.NewSimpleClientset()
		hv.PrependReactor(forbid("get", "namespaces"))
		hv.PrependReactor(forbid("list", "nodes"))

		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(infrav1.AddToScheme(scheme)).To(Succeed())
		Expect(clusterv1.AddToScheme(scheme)).To(Succeed())

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

		hvCluster := &infrav1.HarvesterCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "capi-ns", Finalizers: []string{infrav1.ClusterFinalizer}},
			Spec: infrav1.HarvesterClusterSpec{
				TargetNamespace:    "tenant-ns",
				LoadBalancerConfig: infrav1.LoadBalancerConfig{IPAMType: infrav1.DHCP},
			},
		}

		if connectionReady {
			conditions.Set(hvCluster, metav1.Condition{
				Type: infrav1.HarvesterConnectionReadyCondition, Status: metav1.ConditionTrue, Reason: infrav1.HarvesterConnectionReadyReason,
			})
		}

		return &HarvesterClusterReconciler{Client: fakeClient, Scheme: scheme}, &ClusterScope{
			Ctx:              context.TODO(),
			Logger:           log.FromContext(context.TODO()),
			Cluster:          &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "capi-ns"}},
			HarvesterCluster: hvCluster,
			HarvesterClient:  hv,
			ReconcileClient:  fakeClient,
		}
	}

	It("reports the namespace ready when only the Namespace object itself is forbidden", func() {
		r, scope := newScope(true)

		_, err := r.ReconcileNormal(scope)
		Expect(err).ToNot(HaveOccurred())
		Expect(conditions.IsTrue(scope.HarvesterCluster, infrav1.TargetNamespaceReadyCondition)).To(BeTrue())
	})

	It("keeps reporting it not ready when the connection check has not passed", func() {
		r, scope := newScope(false)

		_, err := r.ReconcileNormal(scope)
		Expect(err).ToNot(HaveOccurred())
		Expect(conditions.IsFalse(scope.HarvesterCluster, infrav1.TargetNamespaceReadyCondition)).To(BeTrue())
	})
})
