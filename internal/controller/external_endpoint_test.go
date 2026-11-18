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

	kubevirtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/harvester/cluster-api-provider-harvester/api/v1beta1"
	hvfake "github.com/harvester/cluster-api-provider-harvester/pkg/clientset/versioned/fake"
)

// spec.loadBalancerConfig.ipamType "external": the control plane endpoint is provided by the
// user. The controller creates nothing for it in Harvester and never writes the endpoint.
var _ = Describe("HarvesterCluster with a user-provided control plane endpoint", func() {
	const targetNamespace = "tenant-ns"

	userEndpoint := clusterv1.APIEndpoint{Host: "192.0.2.10", Port: 6443}

	// loadBalancerRequests returns the requests the controller made for the objects that
	// make up a control plane load balancer in Harvester.
	loadBalancerRequests := func(hv *hvfake.Clientset) []string {
		requests := []string{}

		for _, action := range hv.Actions() {
			switch action.GetResource().Resource {
			case "services", "loadbalancers", "ippools":
				requests = append(requests, action.GetVerb()+" "+action.GetResource().Resource)
			}
		}

		return requests
	}

	// A Harvester as a namespace tenant sees it: everything about load balancers is refused,
	// and so is everything outside the namespace.
	newTenantHarvester := func() *hvfake.Clientset {
		hv := hvfake.NewSimpleClientset()
		hv.PrependReactor(forbid("*", "services"))
		hv.PrependReactor(forbid("*", "loadbalancers"))
		hv.PrependReactor(forbid("*", "ippools"))
		hv.PrependReactor(forbid("get", "namespaces"))
		hv.PrependReactor(forbid("list", "nodes"))

		return hv
	}

	newScope := func(hv *hvfake.Clientset, endpoint clusterv1.APIEndpoint, objects ...runtime.Object) (*HarvesterClusterReconciler, *ClusterScope) {
		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(infrav1.AddToScheme(scheme)).To(Succeed())
		Expect(clusterv1.AddToScheme(scheme)).To(Succeed())

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()

		hvCluster := &infrav1.HarvesterCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "capi-ns", Finalizers: []string{infrav1.ClusterFinalizer}},
			Spec: infrav1.HarvesterClusterSpec{
				TargetNamespace:      targetNamespace,
				LoadBalancerConfig:   infrav1.LoadBalancerConfig{IPAMType: infrav1.EXTERNAL},
				ControlPlaneEndpoint: endpoint,
			},
		}
		conditions.Set(hvCluster, metav1.Condition{
			Type: infrav1.HarvesterConnectionReadyCondition, Status: metav1.ConditionTrue, Reason: infrav1.HarvesterConnectionReadyReason,
		})

		return &HarvesterClusterReconciler{Client: fakeClient, Scheme: scheme}, &ClusterScope{
			Ctx:              context.TODO(),
			Logger:           log.FromContext(context.TODO()),
			Cluster:          &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "capi-ns"}},
			HarvesterCluster: hvCluster,
			HarvesterClient:  hv,
			ReconcileClient:  fakeClient,
		}
	}

	It("is ready at once, keeps the endpoint and asks Harvester for no load balancer", func() {
		hv := newTenantHarvester()
		r, scope := newScope(hv, userEndpoint)

		result, err := r.ReconcileNormal(scope)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())
		Expect(scope.HarvesterCluster.Status.Ready).To(BeTrue())
		Expect(scope.HarvesterCluster.Spec.ControlPlaneEndpoint).To(Equal(userEndpoint))
		Expect(conditions.IsTrue(scope.HarvesterCluster, infrav1.InfrastructureReadyCondition)).To(BeTrue())
		Expect(conditions.GetReason(scope.HarvesterCluster, infrav1.LoadBalancerReadyCondition)).
			To(Equal(infrav1.LoadBalancerExternallyManagedReason))
		Expect(loadBalancerRequests(hv)).To(BeEmpty())
	})

	It("keeps the endpoint once control plane machines exist", func() {
		hv := newTenantHarvester()

		_, err := hv.KubevirtV1().VirtualMachines(targetNamespace).Create(context.TODO(), &kubevirtv1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "c1-cp-0", Namespace: targetNamespace},
		}, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())

		r, scope := newScope(hv, userEndpoint, &infrav1.HarvesterMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name: "c1-cp-0", Namespace: "capi-ns",
				Labels: map[string]string{clusterv1.ClusterNameLabel: "c1", clusterv1.MachineControlPlaneLabel: ""},
			},
		})

		for range 3 {
			_, err = r.ReconcileNormal(scope)
			Expect(err).ToNot(HaveOccurred())
		}

		Expect(scope.HarvesterCluster.Status.Ready).To(BeTrue())
		Expect(scope.HarvesterCluster.Spec.ControlPlaneEndpoint).To(Equal(userEndpoint))
		Expect(loadBalancerRequests(hv)).To(BeEmpty())
	})

	It("is not ready without an endpoint, and says why", func() {
		hv := newTenantHarvester()
		r, scope := newScope(hv, clusterv1.APIEndpoint{})

		_, err := r.ReconcileNormal(scope)
		Expect(err).ToNot(HaveOccurred())
		Expect(scope.HarvesterCluster.Status.Ready).To(BeFalse())
		Expect(conditions.GetReason(scope.HarvesterCluster, infrav1.InfrastructureReadyCondition)).
			To(Equal(infrav1.ControlPlaneEndpointNotSetReason))
		Expect(loadBalancerRequests(hv)).To(BeEmpty())
	})

	It("deletes without asking Harvester for load balancer objects", func() {
		hv := newTenantHarvester()
		r, scope := newScope(hv, userEndpoint)

		result, err := r.ReconcileDelete(scope)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())
		Expect(scope.HarvesterCluster.Finalizers).To(BeEmpty())
		Expect(loadBalancerRequests(hv)).To(BeEmpty())
	})

	It("still replaces a user value with the load balancer address in dhcp mode, as before", func() {
		hv := hvfake.NewSimpleClientset()
		_, err := hv.CoreV1().Services(targetNamespace).Create(context.TODO(), &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "capi-ns-c1-lb", Namespace: targetNamespace},
			Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{{IP: "198.51.100.7"}},
			}},
		}, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())

		r, scope := newScope(hv, userEndpoint)
		scope.HarvesterCluster.Spec.LoadBalancerConfig.IPAMType = infrav1.DHCP

		_, err = r.ReconcileNormal(scope)
		Expect(err).ToNot(HaveOccurred())
		Expect(scope.HarvesterCluster.Spec.ControlPlaneEndpoint.Host).To(Equal("198.51.100.7"))
	})
})

// A tenant ServiceAccount can be given rights on load balancers in its namespace (dhcp mode),
// but IP pools are cluster scoped and stay out of reach.
var _ = Describe("HarvesterCluster deletion in dhcp mode with a namespace-scoped identity", func() {
	It("deletes the LoadBalancer and its Service and does not ask for IP pools", func() {
		hv := hvfake.NewSimpleClientset()
		hv.PrependReactor(forbid("*", "ippools"))

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

		_, err := hv.CoreV1().Services("tenant-ns").Create(context.TODO(), &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "capi-ns-c1-lb", Namespace: "tenant-ns"},
		}, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())

		r := &HarvesterClusterReconciler{Client: fakeClient, Scheme: scheme}

		result, err := r.ReconcileDelete(&ClusterScope{
			Ctx:              context.TODO(),
			Logger:           log.FromContext(context.TODO()),
			Cluster:          &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "capi-ns"}},
			HarvesterCluster: hvCluster,
			HarvesterClient:  hv,
			ReconcileClient:  fakeClient,
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())
		Expect(hvCluster.Finalizers).To(BeEmpty())

		_, err = hv.CoreV1().Services("tenant-ns").Get(context.TODO(), "capi-ns-c1-lb", metav1.GetOptions{})
		Expect(err).To(HaveOccurred(), "the placeholder Service is deleted")

		for _, action := range hv.Actions() {
			Expect(action.GetResource().Resource).ToNot(Equal("ippools"))
		}
	})
})
