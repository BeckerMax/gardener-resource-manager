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
	"fmt"

	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/source"

	resourcemanagercmd "github.com/gardener/gardener-resource-manager/pkg/cmd"
)

// ControllerName is the name of the controller.
const ControllerName = "tokenrequestor_controller"

// defaultControllerConfig is the default config for the controller.
var defaultControllerConfig ControllerConfig

// ControllerOptions are options for adding the controller to a Manager.
type ControllerOptions struct {
}

// ControllerConfig is the completed configuration for the controller.
type ControllerConfig struct {
	TargetClientConfig resourcemanagercmd.TargetClientConfig
}

// AddToManagerWithOptions adds the controller to a Manager with the given config.
func AddToManagerWithOptions(mgr manager.Manager, conf ControllerConfig) error {
	ctrl, err := crcontroller.New(ControllerName, mgr, crcontroller.Options{
		MaxConcurrentReconciles: 5, //TODO configurable
		Reconciler: &reconciler{
			targetClient: conf.TargetClientConfig.Client,
		},
	})
	if err != nil {
		return fmt.Errorf("unable to set up tokenRequestor controller: %w", err)
	}

	return ctrl.Watch(
		&source.Kind{Type: &corev1.Secret{}},
		&handler.EnqueueRequestForObject{},
		// TOOD filter by label
	)
}

// AddToManager adds the controller to a Manager using the default config.
func AddToManager(mgr manager.Manager) error {
	return AddToManagerWithOptions(mgr, defaultControllerConfig)
}

// AddFlags adds the needed command line flags to the given FlagSet.
func (o *ControllerOptions) AddFlags(fs *pflag.FlagSet) {
	//fs.DurationVar(&o.syncPeriod, "token-collector-sync-period", 0, "duration how often the garbage collection should be performed (default: 0, i.e., gc is disabled)")
}

// Complete completes the given command line flags and set the defaultControllerConfig accordingly.
func (o *ControllerOptions) Complete() error {
	defaultControllerConfig = ControllerConfig{}
	return nil
}

// Completed returns the completed ControllerConfig.
func (o *ControllerOptions) Completed() *ControllerConfig {
	return &defaultControllerConfig
}
