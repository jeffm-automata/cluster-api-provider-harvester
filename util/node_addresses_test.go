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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/go-logr/logr"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// statusSubresource is the path suffix of the status subresource of a node.
const statusSubresource = "/status"

// recordingNodeServer simulates the node API of a workload cluster, applies merge patches to
// the node and to its status subresource, and records every write in order.
type recordingNodeServer struct {
	mu     sync.Mutex
	node   *v1.Node
	writes []string
	// failStatus makes PATCH on the status subresource fail with 403.
	failStatus bool
}

func (s *recordingNodeServer) start() (*httptest.Server, *rest.Config) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")

		path, ok := strings.CutPrefix(r.URL.Path, "/api/v1/nodes/"+s.node.Name)
		if !ok || (path != "" && path != statusSubresource) {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		if r.Method == http.MethodPatch {
			body, _ := io.ReadAll(r.Body)

			var patch struct {
				Spec *struct {
					ProviderID *string     `json:"providerID"`
					Taints     *[]v1.Taint `json:"taints"`
				} `json:"spec"`
				Status *struct {
					Addresses []v1.NodeAddress `json:"addresses"`
				} `json:"status"`
			}

			_ = json.Unmarshal(body, &patch)

			switch {
			case path == statusSubresource && s.failStatus:
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403}`))

				return
			case path == statusSubresource && patch.Status != nil:
				s.node.Status.Addresses = patch.Status.Addresses
				s.writes = append(s.writes, "status.addresses")
			case path == "" && patch.Spec != nil && patch.Spec.ProviderID != nil:
				s.node.Spec.ProviderID = *patch.Spec.ProviderID
				s.writes = append(s.writes, "spec.providerID")
			case path == "" && patch.Spec != nil && patch.Spec.Taints != nil:
				s.node.Spec.Taints = *patch.Spec.Taints
				s.writes = append(s.writes, "spec.taints")
			default:
				// A status change sent to the main resource is ignored by the API server.
				s.writes = append(s.writes, "ignored")
			}
		}

		//nolint:errchkjson // test helper, unsafe type acceptable
		_ = json.NewEncoder(w).Encode(s.node)
	}))

	return server, &rest.Config{Host: server.URL}
}

var _ = Describe("InitializeWorkloadNodeWithAddresses", func() {
	vmAddresses := []v1.NodeAddress{
		{Type: v1.NodeInternalIP, Address: "192.0.2.21"},
		{Type: v1.NodeHostName, Address: "node-1"},
	}

	newNode := func() *v1.Node {
		return &v1.Node{
			TypeMeta:   metav1.TypeMeta{Kind: "Node", APIVersion: "v1"},
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Spec: v1.NodeSpec{Taints: []v1.Taint{
				{Key: uninitializedTaintKey, Value: "true", Effect: v1.TaintEffectNoSchedule},
			}},
		}
	}

	It("publishes the VM addresses on a node that has none, before the taint is removed", func() {
		s := &recordingNodeServer{node: newNode()}
		server, config := s.start()

		defer server.Close()

		Expect(InitializeWorkloadNodeWithAddresses(context.Background(), logr.Discard(), config,
			"node-1", "harvester://uid", vmAddresses)).To(BeTrue())
		Expect(s.node.Status.Addresses).To(Equal(vmAddresses))
		Expect(s.node.Spec.ProviderID).To(Equal("harvester://uid"))
		Expect(s.node.Spec.Taints).To(BeEmpty())
		Expect(s.writes).To(Equal([]string{"status.addresses", "spec.providerID", "spec.taints"}))
	})

	It("publishes them on a node that is otherwise initialized already", func() {
		s := &recordingNodeServer{node: newNode()}
		s.node.Spec.ProviderID = "harvester://uid"
		s.node.Spec.Taints = nil

		server, config := s.start()

		defer server.Close()

		Expect(InitializeWorkloadNodeWithAddresses(context.Background(), logr.Discard(), config,
			"node-1", "harvester://uid", vmAddresses)).To(BeTrue())
		Expect(s.node.Status.Addresses).To(Equal(vmAddresses))
		Expect(s.writes).To(Equal([]string{"status.addresses"}))
	})

	It("publishes them on a node whose kubelet has published its host name only", func() {
		s := &recordingNodeServer{node: newNode()}
		s.node.Status.Addresses = []v1.NodeAddress{{Type: v1.NodeHostName, Address: "node-1"}}

		server, config := s.start()

		defer server.Close()

		Expect(InitializeWorkloadNodeWithAddresses(context.Background(), logr.Discard(), config,
			"node-1", "harvester://uid", vmAddresses)).To(BeTrue())
		Expect(s.node.Status.Addresses).To(Equal(vmAddresses))
		Expect(s.writes).To(Equal([]string{"status.addresses", "spec.providerID", "spec.taints"}))
	})

	It("never touches IP addresses that something else has published", func() {
		published := []v1.NodeAddress{
			{Type: v1.NodeInternalIP, Address: "198.51.100.5"},
			{Type: v1.NodeHostName, Address: "node-1"},
		}

		s := &recordingNodeServer{node: newNode()}
		s.node.Status.Addresses = published

		server, config := s.start()

		defer server.Close()

		Expect(InitializeWorkloadNodeWithAddresses(context.Background(), logr.Discard(), config,
			"node-1", "harvester://uid", vmAddresses)).To(BeTrue())
		Expect(s.node.Status.Addresses).To(Equal(published))
		Expect(s.writes).To(Equal([]string{"spec.providerID", "spec.taints"}))
	})

	It("leaves a node alone that has an external IP address only", func() {
		published := []v1.NodeAddress{{Type: v1.NodeExternalIP, Address: "203.0.113.9"}}

		s := &recordingNodeServer{node: newNode()}
		s.node.Status.Addresses = published

		server, config := s.start()

		defer server.Close()

		Expect(InitializeWorkloadNodeWithAddresses(context.Background(), logr.Discard(), config,
			"node-1", "harvester://uid", vmAddresses)).To(BeTrue())
		Expect(s.node.Status.Addresses).To(Equal(published))
	})

	It("writes nothing to the status when no address is known", func() {
		s := &recordingNodeServer{node: newNode()}
		server, config := s.start()

		defer server.Close()

		Expect(InitializeWorkloadNodeWithAddresses(context.Background(), logr.Discard(), config,
			"node-1", "harvester://uid", nil)).To(BeTrue())
		Expect(s.node.Status.Addresses).To(BeEmpty())
		Expect(s.writes).To(Equal([]string{"spec.providerID", "spec.taints"}))
	})

	It("reports the node as not initialized, and keeps the taint, when the addresses cannot be written", func() {
		s := &recordingNodeServer{node: newNode(), failStatus: true}
		server, config := s.start()

		defer server.Close()

		Expect(InitializeWorkloadNodeWithAddresses(context.Background(), logr.Discard(), config,
			"node-1", "harvester://uid", vmAddresses)).To(BeFalse())
		Expect(s.node.Spec.Taints).To(HaveLen(1))
		Expect(s.writes).To(BeEmpty())
	})

	It("InitializeWorkloadNode keeps its behaviour: no status write", func() {
		s := &recordingNodeServer{node: newNode()}
		server, config := s.start()

		defer server.Close()

		Expect(InitializeWorkloadNode(context.Background(), logr.Discard(), config, "node-1", "harvester://uid")).To(BeTrue())
		Expect(s.writes).To(Equal([]string{"spec.providerID", "spec.taints"}))
	})
})
