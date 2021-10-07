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

	"github.com/gardener/gardener/pkg/controllerutils"
	"github.com/go-logr/logr"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/clock"
	corev1clientset "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	finalizerName                         = "resources.gardener.cloud/tokenrequestor-controller"
	serviceAccountName                    = "serviceaccount.shoot.gardener.cloud/name"
	serviceAccountNamespace               = "serviceaccount.shoot.gardener.cloud/namespace"
	serviceAccountTokenExpirationDuration = "serviceaccount.shoot.gardener.cloud/token-expiration-duration"
	serviceAccountTokenRenewTimestamp     = "serviceaccount.shoot.gardener.cloud/token-renew-timestamp"
	serviceAccountIgnoreOnDeletion        = "serviceaccount.shoot.gardener.cloud/ignore-on-deletion"
	// TODO use constant also in Gardenlet
	dataKeyToken = "token"
	layout       = "2006-01-02T15:04:05.000Z"
)

type reconciler struct {
	clock              clock.Clock
	log                logr.Logger
	targetClient       client.Client
	targetCoreV1Client corev1clientset.CoreV1Interface
	client             client.Client
}

func (r *reconciler) InjectLogger(l logr.Logger) error {
	r.log = l.WithName(ControllerName)
	return nil
}

func (r *reconciler) InjectClient(c client.Client) error {
	r.client = c
	return nil
}

func (r *reconciler) Reconcile(reconcileCtx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx, cancel := context.WithTimeout(reconcileCtx, time.Minute)
	defer cancel()

	log := r.log.WithValues("object", req)

	secret := &corev1.Secret{}
	if err := r.client.Get(ctx, req.NamespacedName, secret); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Stopping reconciliation of Secret, as it has been deleted")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("could not fetch Secret: %w", err)
	}

	if secret.DeletionTimestamp != nil {
		if shouldDeleteServiceAccount(secret) {
			if err := r.deleteServiceAccount(ctx, secret); err != nil {
				return reconcile.Result{}, err
			}
		}

		if err := controllerutils.PatchRemoveFinalizers(ctx, r.client, secret, finalizerName); err != nil {
			return reconcile.Result{}, fmt.Errorf("error removing finalizer from Secret: %+v", err)
		}

		return reconcile.Result{}, nil
	}

	if err := controllerutils.PatchAddFinalizers(ctx, r.client, secret, finalizerName); err != nil {
		return reconcile.Result{}, err
	}

	mustRequeue, requeueAfter, err := r.requeue(secret.Annotations[serviceAccountTokenRenewTimestamp])
	if err != nil {
		return reconcile.Result{}, err
	}
	if mustRequeue {
		return reconcile.Result{Requeue: true, RequeueAfter: requeueAfter}, nil
	}

	serviceAccount, err := r.reconcileServiceAccount(ctx, secret)
	if err != nil {
		return reconcile.Result{}, err
	}

	expirationSeconds, err := tokenExpirationSeconds(secret)
	if err != nil {
		return reconcile.Result{}, err
	}

	tokenRequest, err := r.createServiceAccountToken(ctx, serviceAccount, expirationSeconds)
	if err != nil {
		return reconcile.Result{}, err
	}

	renewDuration := r.renewDuration(tokenRequest.Status.ExpirationTimestamp.Time)

	if err := r.reconcileSecret(ctx, secret, tokenRequest.Status.Token, renewDuration); err != nil {
		return reconcile.Result{}, fmt.Errorf("could not update Secret with token: %w", err)
	}

	return reconcile.Result{Requeue: true, RequeueAfter: renewDuration}, nil
}

func shouldDeleteServiceAccount(secret *corev1.Secret) bool {
	v, ok := secret.Annotations[serviceAccountIgnoreOnDeletion]
	if ok && v == "true" {
		return false
	}
	return true
}

func (r *reconciler) reconcileServiceAccount(ctx context.Context, secret *corev1.Secret) (*corev1.ServiceAccount, error) {
	serviceAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secret.Annotations[serviceAccountName],
			Namespace: secret.Annotations[serviceAccountNamespace],
		},
	}

	if _, err := controllerutil.CreateOrUpdate(ctx, r.targetClient, serviceAccount, func() error {
		serviceAccount.AutomountServiceAccountToken = pointer.Bool(false)
		return nil
	}); err != nil {
		return nil, err
	}

	return serviceAccount, nil
}

func (r *reconciler) deleteServiceAccount(ctx context.Context, secret *corev1.Secret) error {
	serviceAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secret.Annotations[serviceAccountName],
			Namespace: secret.Annotations[serviceAccountNamespace],
		},
	}

	return client.IgnoreNotFound(r.targetClient.Delete(ctx, serviceAccount))
}

func (r *reconciler) reconcileSecret(ctx context.Context, secret *corev1.Secret, token string, renewDuration time.Duration) error {
	patch := client.MergeFrom(secret.DeepCopy())

	if secret.Data == nil {
		secret.Data = make(map[string][]byte, 1)
	}

	secret.Data[dataKeyToken] = []byte(token)
	metav1.SetMetaDataAnnotation(&secret.ObjectMeta, serviceAccountTokenRenewTimestamp, r.clock.Now().UTC().Add(renewDuration).Format(layout))

	return r.client.Patch(ctx, secret, patch)
}

func (r *reconciler) createServiceAccountToken(ctx context.Context, sa *corev1.ServiceAccount, expirationSeconds int64) (*authenticationv1.TokenRequest, error) {
	tokenRequest := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{"kubernetes"},
			ExpirationSeconds: &expirationSeconds,
		},
	}

	return r.targetCoreV1Client.ServiceAccounts(sa.Namespace).CreateToken(ctx, sa.Name, tokenRequest, metav1.CreateOptions{})
}

func (r *reconciler) requeue(renewTimestamp string) (bool, time.Duration, error) {
	if len(renewTimestamp) == 0 {
		return false, 0, nil
	}

	renewTime, err := time.Parse(layout, renewTimestamp)
	if err != nil {
		return false, 0, fmt.Errorf("could not parse renew timestamp: %w", err)
	}

	if r.clock.Now().UTC().Before(renewTime) {
		return true, renewTime.Sub(r.clock.Now().UTC()), nil
	}

	return false, 0, nil
}

func (r *reconciler) renewDuration(expirationTimestamp time.Time) time.Duration {
	expirationDuration := expirationTimestamp.UTC().Sub(r.clock.Now().UTC())
	return expirationDuration * 80 / 100
}

func tokenExpirationSeconds(secret *corev1.Secret) (int64, error) {
	var (
		expirationDuration = time.Hour
		err                error
	)

	if v, ok := secret.Annotations[serviceAccountTokenExpirationDuration]; ok {
		expirationDuration, err = time.ParseDuration(v)
		if err != nil {
			return 0, err
		}
	}

	return int64(expirationDuration / time.Second), nil
}
