/*
Copyright 2022 The Kubernetes Authors.

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

package controllers

import (
	"context"
	"time"

	"github.com/pkg/errors"
	infrav1 "github.com/syself/cluster-api-provider-hetzner/api/v1beta1"
	"github.com/syself/cluster-api-provider-hetzner/pkg/scope"
	secretutil "github.com/syself/cluster-api-provider-hetzner/pkg/secrets"
	hcloudclient "github.com/syself/cluster-api-provider-hetzner/pkg/services/hcloud/client"
	"github.com/syself/cluster-api-provider-hetzner/pkg/services/hcloud/machinetemplate"
	"sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// HCloudMachineTemplateReconciler reconciles a HCloudMachineTemplate object.
type HCloudMachineTemplateReconciler struct {
	client.Client
	APIReader           client.Reader
	HCloudClientFactory hcloudclient.Factory
	WatchFilterValue    string
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hcloudmachinetemplates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hcloudmachinetemplates/status,verbs=get;update;patch

// Reconcile manages the lifecycle of an HCloudMachineTemplate object.
func (r *HCloudMachineTemplateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	log := ctrl.LoggerFrom(ctx).WithValues("hcloudmachinetemplate", req.NamespacedName)
	log.Info("Reconcile HCloudMachineTemplate")

	machineTemplate := &infrav1.HCloudMachineTemplate{}
	if err := r.Get(ctx, req.NamespacedName, machineTemplate); err != nil {
		log.Error(err, "unable to fetch HCloudMachineTemplate")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Fetch the Cluster.
	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, machineTemplate.ObjectMeta)
	if err != nil {
		log.Info("Machine is missing cluster label or cluster does not exist")
		return ctrl.Result{}, nil
	}

	log = log.WithValues("cluster", cluster.Name)

	hetznerCluster := &infrav1.HetznerCluster{}

	hetznerClusterName := client.ObjectKey{
		Namespace: machineTemplate.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}
	if err := r.Client.Get(ctx, hetznerClusterName, hetznerCluster); err != nil {
		log.Info("HetznerCluster is not available yet")
		return ctrl.Result{}, nil
	}

	// Create the scope.
	secretManager := secretutil.NewSecretManager(log, r.Client, r.APIReader)
	hcloudToken, _, err := getAndValidateHCloudToken(ctx, req.Namespace, hetznerCluster, secretManager)
	if err != nil {
		return hcloudTokenErrorResult(ctx, err, machineTemplate, infrav1.InstanceReadyCondition, r.Client)
	}

	hcc := r.HCloudClientFactory.NewClient(hcloudToken)

	machineTemplateScope, err := scope.NewHCloudMachineTemplateScope(ctx, scope.HCloudMachineTemplateScopeParams{
		Client:                r.Client,
		Logger:                &log,
		HCloudMachineTemplate: machineTemplate,
		HCloudClient:          hcc,
	})
	if err != nil {
		return reconcile.Result{}, errors.Errorf("failed to create scope: %+v", err)
	}

	// Always close the scope when exiting this function so we can persist any HCloudMachine changes.
	defer func() {
		if err := machineTemplateScope.Close(ctx); err != nil && reterr == nil {
			reterr = err
		}
	}()

	// check whether rate limit has been reached and if so, then wait.
	if wait := reconcileRateLimit(machineTemplate); wait {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	return r.reconcile(ctx, machineTemplateScope)
}

func (r *HCloudMachineTemplateReconciler) reconcile(ctx context.Context, machineTemplateScope *scope.HCloudMachineTemplateScope) (reconcile.Result, error) {
	machineTemplateScope.Info("Reconciling HCloudMachineTemplate")
	hcloudMachine := machineTemplateScope.HCloudMachineTemplate

	// If the HCloudMachineTemplate doesn't have our finalizer, add it.
	controllerutil.AddFinalizer(machineTemplateScope.HCloudMachineTemplate, infrav1.MachineFinalizer)

	// Register the finalizer immediately to avoid orphaning HCloud resources on delete
	if err := machineTemplateScope.PatchObject(ctx); err != nil {
		return ctrl.Result{}, err
	}

	// reconcile machinetemplate
	if result, brk, err := breakReconcile(machinetemplate.NewService(machineTemplateScope).Reconcile(ctx)); brk {
		return result, errors.Wrapf(err, "failed to reconcile machinetemplate for HCloudMachineTemplate %s/%s", hcloudMachine.Namespace, hcloudMachine.Name)
	}

	return reconcile.Result{}, nil
}

func (r *HCloudMachineTemplateReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(options).
		For(&infrav1.HCloudMachineTemplate{}).
		Complete(r)
}
