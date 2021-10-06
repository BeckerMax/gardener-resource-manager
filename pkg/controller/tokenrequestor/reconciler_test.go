// Copyright (c) 2021 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tokenrequestor

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gstruct"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/client-go/kubernetes/scheme"
	corev1fake "k8s.io/client-go/kubernetes/typed/core/v1/fake"
	"k8s.io/client-go/testing"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("Reconciler", func() {
	Describe("#Reconcile", func() {
		var (
			ctx = context.TODO()

			logger    logr.Logger
			fakeClock clock.Clock

			sourceClient, targetClient client.Client
			coreV1Client               *corev1fake.FakeCoreV1

			ctrl *reconciler

			secret         *corev1.Secret
			serviceAccount *corev1.ServiceAccount
			request        reconcile.Request

			secretName              = "kube-scheduler"
			serviceAccountName      = "kube-scheduler-serviceacount"
			serviceAccountNamespace = "kube-system"
			expirationDuration      = 100 * time.Minute
			renewDuration           = 80 * time.Minute
			token                   = "foo"
			fakeNow                 = time.Date(2021, 10, 4, 10, 0, 0, 0, time.UTC)

			fakeCreateServiceAccountToken = func() {
				coreV1Client.AddReactor("create", "serviceaccounts", func(action testing.Action) (bool, runtime.Object, error) {
					if action.GetSubresource() != "token" {
						return false, nil, fmt.Errorf("subresource should be 'token'")
					}

					return true, &authenticationv1.TokenRequest{
						Status: authenticationv1.TokenRequestStatus{
							Token:               token,
							ExpirationTimestamp: metav1.Time{Time: fakeNow.Add(expirationDuration)},
						},
					}, nil
				})
			}
		)

		BeforeEach(func() {
			logger = log.Log.WithName("test")
			fakeClock = clock.NewFakeClock(fakeNow)

			sourceClient = fakeclient.NewClientBuilder().WithScheme(scheme.Scheme).Build()
			targetClient = fakeclient.NewClientBuilder().WithScheme(scheme.Scheme).Build()
			coreV1Client = &corev1fake.FakeCoreV1{Fake: &testing.Fake{}}

			ctrl = &reconciler{
				clock:              fakeClock,
				targetClient:       targetClient,
				targetCoreV1Client: coreV1Client,
			}

			Expect(ctrl.InjectLogger(logger)).To(Succeed())
			Expect(ctrl.InjectClient(sourceClient)).To(Succeed())

			secret = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      secretName,
					Namespace: metav1.NamespaceDefault,
					Annotations: map[string]string{
						"serviceaccount.shoot.gardener.cloud/name":      serviceAccountName,
						"serviceaccount.shoot.gardener.cloud/namespace": serviceAccountNamespace,
					},
				},
			}
			serviceAccount = &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceAccountName,
					Namespace: serviceAccountNamespace,
				},
			}
			request = reconcile.Request{NamespacedName: types.NamespacedName{
				Name:      secret.Name,
				Namespace: secret.Namespace,
			}}
		})
		It("should set a finalizer on the secret", func() {
			fakeCreateServiceAccountToken()
			Expect(sourceClient.Create(ctx, secret)).To(Succeed())

			_, err := ctrl.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())

			Expect(sourceClient.Get(ctx, client.ObjectKeyFromObject(secret), secret)).To(Succeed())
			Expect(secret.Finalizers).To(ConsistOf("resources.gardener.cloud/tokenrequestor-controller"))
		})

		It("should remove the finalizer from the secret after deleting the ServiceAccount", func() {
			fakeCreateServiceAccountToken()
			Expect(sourceClient.Create(ctx, secret)).To(Succeed())
			Expect(targetClient.Get(ctx, client.ObjectKeyFromObject(serviceAccount), serviceAccount)).To(MatchError(ContainSubstring("not found")))

			_, err := ctrl.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())

			Expect(targetClient.Get(ctx, client.ObjectKeyFromObject(serviceAccount), serviceAccount)).To(Succeed())
			Expect(serviceAccount.AutomountServiceAccountToken).To(PointTo(BeFalse()))

			Expect(sourceClient.Get(ctx, client.ObjectKeyFromObject(secret), secret)).To(Succeed())
			Expect(sourceClient.Delete(ctx, secret)).To(Succeed())

			result, err := ctrl.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			Expect(targetClient.Get(ctx, client.ObjectKeyFromObject(serviceAccount), serviceAccount)).To(MatchError(ContainSubstring("not found")))
			Expect(targetClient.Get(ctx, client.ObjectKeyFromObject(secret), secret)).To(MatchError(ContainSubstring("not found")))
		})

		// TODO annotation ignore SA

		It("should generate a new service account, a new token and requeue", func() {
			fakeCreateServiceAccountToken()
			Expect(sourceClient.Create(ctx, secret)).To(Succeed())
			Expect(targetClient.Get(ctx, client.ObjectKeyFromObject(serviceAccount), serviceAccount)).To(MatchError(ContainSubstring("not found")))

			result, err := ctrl.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{Requeue: true, RequeueAfter: renewDuration}))

			Expect(targetClient.Get(ctx, client.ObjectKeyFromObject(serviceAccount), serviceAccount)).To(Succeed())
			Expect(serviceAccount.AutomountServiceAccountToken).To(PointTo(BeFalse()))

			Expect(sourceClient.Get(ctx, client.ObjectKeyFromObject(secret), secret)).To(Succeed())
			Expect(secret.Data).To(HaveKeyWithValue("token", []byte(token)))
			Expect(secret.Annotations).To(HaveKeyWithValue("serviceaccount.shoot.gardener.cloud/token-renew-timestamp", fakeNow.Add(renewDuration).Format(layout)))
		})

		It("should requeue because renew timestamp has not been reached", func() {
			delay := time.Minute
			metav1.SetMetaDataAnnotation(&secret.ObjectMeta, "serviceaccount.shoot.gardener.cloud/token-renew-timestamp", fakeNow.Add(delay).Format(layout))

			Expect(sourceClient.Create(ctx, secret)).To(Succeed())

			result, err := ctrl.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{Requeue: true, RequeueAfter: delay}))
		})

		It("should issue a new token since the renew timestamp is in the past", func() {
			expiredSince := time.Minute
			metav1.SetMetaDataAnnotation(&secret.ObjectMeta, "serviceaccount.shoot.gardener.cloud/token-renew-timestamp", fakeNow.Add(-expiredSince).Format(layout))

			token = "new-token"
			fakeCreateServiceAccountToken()

			Expect(sourceClient.Create(ctx, secret)).To(Succeed())
			Expect(targetClient.Create(ctx, serviceAccount)).To(Succeed())

			result, err := ctrl.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{Requeue: true, RequeueAfter: renewDuration}))

			Expect(sourceClient.Get(ctx, client.ObjectKeyFromObject(secret), secret)).To(Succeed())
			Expect(secret.Data).To(HaveKeyWithValue("token", []byte(token)))
			Expect(secret.Annotations).To(HaveKeyWithValue("serviceaccount.shoot.gardener.cloud/token-renew-timestamp", fakeNow.Add(renewDuration).Format(layout)))
		})

		It("should reconcile the service account settings", func() {
			serviceAccount.AutomountServiceAccountToken = pointer.Bool(true)

			fakeCreateServiceAccountToken()
			Expect(sourceClient.Create(ctx, secret)).To(Succeed())
			Expect(targetClient.Create(ctx, serviceAccount)).To(Succeed())

			result, err := ctrl.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{Requeue: true, RequeueAfter: renewDuration}))

			Expect(targetClient.Get(ctx, client.ObjectKeyFromObject(serviceAccount), serviceAccount)).To(Succeed())
			Expect(serviceAccount.AutomountServiceAccountToken).To(PointTo(BeFalse()))
		})

		It("should use the provided token expiration duration", func() {
			expirationDuration = 10 * time.Minute
			renewDuration = 8 * time.Minute
			metav1.SetMetaDataAnnotation(&secret.ObjectMeta, "serviceaccount.shoot.gardener.cloud/token-expiration-duration", expirationDuration.String())
			fakeCreateServiceAccountToken()

			Expect(sourceClient.Create(ctx, secret)).To(Succeed())

			result, err := ctrl.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{Requeue: true, RequeueAfter: renewDuration}))

			Expect(sourceClient.Get(ctx, client.ObjectKeyFromObject(secret), secret)).To(Succeed())
			Expect(secret.Annotations).To(HaveKeyWithValue("serviceaccount.shoot.gardener.cloud/token-renew-timestamp", fakeNow.Add(renewDuration).Format(layout)))
		})

		Context("error", func() {
			It("provided token expiration duration cannot be parsed", func() {
				metav1.SetMetaDataAnnotation(&secret.ObjectMeta, "serviceaccount.shoot.gardener.cloud/token-expiration-duration", "unparseable")

				Expect(sourceClient.Create(ctx, secret)).To(Succeed())

				result, err := ctrl.Reconcile(ctx, request)
				Expect(err).To(MatchError(ContainSubstring("invalid duration")))
				Expect(result).To(Equal(reconcile.Result{}))
			})

			It("renew timestamp has invalid format", func() {
				metav1.SetMetaDataAnnotation(&secret.ObjectMeta, "serviceaccount.shoot.gardener.cloud/token-renew-timestamp", "invalid-format")
				Expect(sourceClient.Create(ctx, secret)).To(Succeed())

				result, err := ctrl.Reconcile(ctx, request)
				Expect(err).To(MatchError(ContainSubstring("could not parse renew timestamp")))
				Expect(result).To(Equal(reconcile.Result{}))
			})
		})
	})
})
