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
	"strconv"
	"time"

	"github.com/go-logr/logr"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1clientset "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	serviceAccountName                   = "serviceaccount.shoot.gardener.cloud/name"
	serviceAccountNamespace              = "serviceaccount.shoot.gardener.cloud/namespace"
	serviceAccountTokenExpirationSeconds = "serviceaccount.shoot.gardener.cloud/token-expiration-seconds"
	serviceAccountTokenRenewTimestamp    = "serviceaccount.shoot.gardener.cloud/token-renew-timestamp"
	// TODO use constant also in Gardenlet
	dataKeyToken = "token"
	layout       = "2006-01-02T15:04:05.000Z"
)

type reconciler struct {
	log                logr.Logger
	targetClient       client.Client
	targetCoreV1Client *corev1clientset.CoreV1Client
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

	if v, ok := secret.Annotations[serviceAccountTokenRenewTimestamp]; ok {
		renewTimestamp, err := time.Parse(layout, v)
		if err != nil {
			// maybe continue
			return reconcile.Result{}, fmt.Errorf("could not fetch Secret: %w", err)
		}
		if time.Now().UTC().Before(renewTimestamp) {
			return reconcile.Result{Requeue: true, RequeueAfter: time.Until(renewTimestamp)}, nil
		}
	}

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secret.Annotations[serviceAccountName],
			Namespace: secret.Annotations[serviceAccountNamespace],
		},
	}

	if _, err := controllerutil.CreateOrUpdate(ctx, r.targetClient, sa, func() error {
		sa.AutomountServiceAccountToken = pointer.Bool(false)
		return nil
	}); err != nil {
		return reconcile.Result{}, err
	}

	var (
		expirationSeconds int64 = 60 * 60
		err               error
	)
	if v, ok := secret.Annotations[serviceAccountTokenExpirationSeconds]; ok {
		expirationSeconds, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	tokenRequest := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{"kubernetes"},
			ExpirationSeconds: &expirationSeconds,
		},
	}

	result, err := r.targetCoreV1Client.ServiceAccounts(sa.Namespace).CreateToken(ctx, sa.Name, tokenRequest, metav1.CreateOptions{})
	if err != nil {
		return reconcile.Result{}, err
	}

	patch := client.MergeFrom(secret.DeepCopy())
	if secret.Data == nil {
		secret.Data = make(map[string][]byte, 1)
	}

	expirationTimestamp := result.Status.ExpirationTimestamp.Time.UTC()
	expirationDuration := expirationTimestamp.Sub(time.Now().UTC())
	renewDuration := expirationDuration * 80 / 100

	secret.Data[dataKeyToken] = []byte(result.Status.Token)
	metav1.SetMetaDataAnnotation(&secret.ObjectMeta, serviceAccountTokenRenewTimestamp, time.Now().UTC().Add(renewDuration).Format(layout))
	if err := r.client.Patch(ctx, secret, patch); err != nil {
		return reconcile.Result{}, fmt.Errorf("could not update Secret with token: %w", err)
	}

	return reconcile.Result{Requeue: true, RequeueAfter: renewDuration}, nil
}
