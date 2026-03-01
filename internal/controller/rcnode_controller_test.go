/*
Copyright 2026.

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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	reclusteriov1 "github.com/lorenzocereser/recluster4/api/v1"
)

var _ = Describe("RcNode Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-rcnode"

		ctx := context.Background()

		// RcNode is cluster-scoped, so no namespace is needed.
		typeNamespacedName := types.NamespacedName{
			Name: resourceName,
		}
		rcnode := &reclusteriov1.RcNode{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind RcNode")
			err := k8sClient.Get(ctx, typeNamespacedName, rcnode)
			if err != nil && errors.IsNotFound(err) {
				resource := &reclusteriov1.RcNode{
					ObjectMeta: metav1.ObjectMeta{
						Name: resourceName,
					},
					Spec: reclusteriov1.RcNodeSpec{
						NodeGroup:    "test-group",
						DesiredPhase: reclusteriov1.NodePhaseOffline,
						Activation: reclusteriov1.ActivationSpec{
							WakeMethod: reclusteriov1.WakeMethodManual,
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &reclusteriov1.RcNode{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if errors.IsNotFound(err) {
				return // already cleaned up
			}
			Expect(err).NotTo(HaveOccurred())

			By("Removing finalizers so the resource can be fully deleted")
			if len(resource.Finalizers) > 0 {
				resource.Finalizers = nil
				Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			}

			By("Cleanup the specific resource instance RcNode")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &RcNodeReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should set finalizer on the RcNode", func() {
			By("Reconciling to add the finalizer")
			controllerReconciler := &RcNodeReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the finalizer is present")
			updated := &reclusteriov1.RcNode{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Finalizers).To(ContainElement("recluster.io/node-cleanup"))
		})

		It("should update status after reconciliation", func() {
			By("Reconciling the resource")
			controllerReconciler := &RcNodeReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying status was updated")
			updated := &reclusteriov1.RcNode{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			// After reconcile with DesiredPhase=Offline, the status should reflect that
			Expect(updated.Status.CurrentPhase).To(Equal(reclusteriov1.NodePhaseOffline))
		})
	})
})
