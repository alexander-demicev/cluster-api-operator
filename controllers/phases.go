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
	"fmt"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	versionutil "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/pointer"
	operatorv1 "sigs.k8s.io/cluster-api-operator/api/v1alpha1"
	"sigs.k8s.io/cluster-api-operator/controllers/genericprovider"
	"sigs.k8s.io/cluster-api-operator/util"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/cluster"
	configclient "sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/repository"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/yamlprocessor"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const metadataFile = "metadata.yaml"

// phaseReconciler holds all required information for interacting with clusterctl code and
// helps to iterate through provider reconciliation phases.
type phaseReconciler struct {
	provider     genericprovider.GenericProvider
	providerList genericprovider.GenericProviderList

	ctrlClient         client.Client
	ctrlConfig         *rest.Config
	repo               repository.Repository
	contract           string
	options            repository.ComponentsOptions
	providerConfig     configclient.Provider
	configClient       configclient.Client
	components         repository.Components
	clusterctlProvider *clusterctlv1.Provider
}

// reconcilePhaseFn is a function that represent a phase of the reconciliation.
type reconcilePhaseFn func(context.Context) (reconcile.Result, error)

// PhaseError custom error type for phases.
type PhaseError struct {
	Reason   string
	Type     clusterv1.ConditionType
	Severity clusterv1.ConditionSeverity
	Err      error
}

func (p *PhaseError) Error() string {
	return p.Err.Error()
}

func wrapPhaseError(err error, reason string, ctype clusterv1.ConditionType) error {
	if err == nil {
		return nil
	}
	return &PhaseError{
		Err:      err,
		Type:     ctype,
		Reason:   reason,
		Severity: clusterv1.ConditionSeverityWarning,
	}
}

// newReconcilePhases returns phase reconciler for the given provider.
func newReconcilePhases(r GenericProviderReconciler, provider genericprovider.GenericProvider, providerList genericprovider.GenericProviderList) *phaseReconciler {
	return &phaseReconciler{
		ctrlClient:         r.Client,
		ctrlConfig:         r.Config,
		clusterctlProvider: &clusterctlv1.Provider{},
		provider:           provider,
		providerList:       providerList,
	}
}

// preflightChecks a wrapper around the preflight checks.
func (p *phaseReconciler) preflightChecks(ctx context.Context) (reconcile.Result, error) {
	return preflightChecks(ctx, p.ctrlClient, p.provider, p.providerList)
}

// load provider specific configuration into phaseReconciler object.
func (p *phaseReconciler) load(ctx context.Context) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	log.V(2).Info("Loading provider", "name", p.provider.GetName())

	// Load provider's secret and config url.
	reader, err := p.secretReader(ctx, p.provider)
	if err != nil {
		return reconcile.Result{}, wrapPhaseError(err, "failed to load the secret reader", operatorv1.PreflightCheckCondition)
	}

	// Initialize the a client for interacting with the clusterctl configuration.
	p.configClient, err = configclient.New("", configclient.InjectReader(reader))
	if err != nil {
		return reconcile.Result{}, err
	}

	// Get returns the configuration for the provider with a given name/type.
	// This is done using clusterctl internal API types.
	p.providerConfig, err = p.configClient.Providers().Get(p.provider.GetName(), util.ClusterctlProviderType(p.provider))
	if err != nil {
		return reconcile.Result{}, wrapPhaseError(err, operatorv1.UnknownProviderReason, operatorv1.PreflightCheckCondition)
	}

	spec := p.provider.GetSpec()

	// If a configmap selector was specified, use it to find the configmap with provider configuraion. This is
	// a case for "air-gapped" environments. If no selector was specified, use GitHub repository.
	if spec.FetchConfig != nil && spec.FetchConfig.ConfigMap != nil {
		log.V(2).Info("Custom ConfigMap was provided for fetching manifests", "provider", p.provider.GetName())
		p.repo, err = p.configmapRepository(ctx, spec.FetchConfig.ConfigMap)
	} else {
		p.repo, err = repository.NewGitHubRepository(p.providerConfig, p.configClient.Variables())
	}
	if err != nil {
		return reconcile.Result{}, wrapPhaseError(err, "failed to load the repository", operatorv1.PreflightCheckCondition)
	}

	// Store some provider specific inputs for passing it to clusterctl library
	p.options = repository.ComponentsOptions{
		TargetNamespace:     p.provider.GetNamespace(),
		SkipTemplateProcess: false,
		Version:             p.repo.DefaultVersion(),
	}
	if spec.Version != nil {
		p.options.Version = *spec.Version
	}

	if err := p.validateRepoCAPIVersion(p.provider); err != nil {
		return reconcile.Result{}, wrapPhaseError(err, operatorv1.CAPIVersionIncompatibilityReason, operatorv1.PreflightCheckCondition)
	}

	return reconcile.Result{}, nil
}

// secretReader use clusterctl MemoryReader structure to store the configuration variables
// that are obtained from a secret and try to set fetch url config.
func (s *phaseReconciler) secretReader(ctx context.Context, provider genericprovider.GenericProvider) (configclient.Reader, error) {
	log := ctrl.LoggerFrom(ctx)

	mr := configclient.NewMemoryReader()
	err := mr.Init("")
	if err != nil {
		return nil, err
	}

	// Fetch configutation variables from the secret. See API field docs for more info.
	if provider.GetSpec().SecretName != nil {
		secret := &corev1.Secret{}
		key := types.NamespacedName{Namespace: provider.GetNamespace(), Name: *provider.GetSpec().SecretName}
		if err := s.ctrlClient.Get(ctx, key, secret); err != nil {
			return nil, err
		}
		for k, v := range secret.Data {
			mr.Set(k, string(v))
		}
	} else {
		log.V(2).Info("No configuration secret was specified", "provider", provider.GetName())
	}

	// If provided store fetch config url in memory reader.
	if provider.GetSpec().FetchConfig != nil && provider.GetSpec().FetchConfig.URL != nil {
		log.V(2).Info("Custom fetch configuration url was provided", "provider", provider.GetName())
		return mr.AddProvider(provider.GetName(), util.ClusterctlProviderType(provider), *provider.GetSpec().FetchConfig.URL)
	}

	return mr, nil
}

// configmapRepository use clusterctl NewMemoryRepository structure to store the manifests
// and metadata from a given configmap.
func (s *phaseReconciler) configmapRepository(ctx context.Context, configMapRef *corev1.ObjectReference) (repository.Repository, error) {
	if configMapRef == nil {
		return nil, fmt.Errorf("configmap reference is nil")
	}

	mr := repository.NewMemoryRepository()
	mr.WithPaths("", "components.yaml")

	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{
		Namespace: configMapRef.Namespace,
		Name:      configMapRef.Name,
	}

	if err := s.ctrlClient.Get(ctx, key, cm); err != nil {
		return nil, err
	}

	version := cm.Name
	errMsg := "from the Name"
	// try to get version from the configmap label
	if cm.Labels != nil {
		ver, ok := cm.Labels[operatorv1.ConfigMapVersionLabelName]
		if ok {
			version = ver
			errMsg = "from the Label " + operatorv1.ConfigMapVersionLabelName
		}
	}

	if _, err := versionutil.ParseSemantic(version); err != nil {
		return nil, fmt.Errorf("ConfigMap %s/%s has invalid version:%s (%s)", cm.Namespace, cm.Name, version, errMsg)
	}

	metadata, ok := cm.Data["metadata"]
	if !ok {
		return nil, fmt.Errorf("ConfigMap %s/%s has no metadata", cm.Namespace, cm.Name)
	}
	mr.WithFile(version, metadataFile, []byte(metadata))

	components, ok := cm.Data["components"]
	if !ok {
		return nil, fmt.Errorf("ConfigMap %s/%s has no components", cm.Namespace, cm.Name)
	}
	mr.WithFile(version, mr.ComponentsPath(), []byte(components))

	return mr, nil
}

// validateRepoCAPIVersion checks that the repo is using the correct version.
func (s *phaseReconciler) validateRepoCAPIVersion(provider genericprovider.GenericProvider) error {
	name := provider.GetName()
	file, err := s.repo.GetFile(s.options.Version, metadataFile)
	if err != nil {
		return errors.Wrapf(err, "failed to read %q from the repository for provider %q", metadataFile, name)
	}

	// Convert the yaml into a typed object
	latestMetadata := &clusterctlv1.Metadata{}
	codecFactory := serializer.NewCodecFactory(scheme.Scheme)

	if err := runtime.DecodeInto(codecFactory.UniversalDecoder(), file, latestMetadata); err != nil {
		return errors.Wrapf(err, "error decoding %q for provider %q", metadataFile, name)
	}

	// Gets the contract for the current release.
	currentVersion, err := versionutil.ParseSemantic(s.options.Version)
	if err != nil {
		return errors.Wrapf(err, "failed to parse current version for the %s provider", name)
	}

	releaseSeries := latestMetadata.GetReleaseSeriesForVersion(currentVersion)
	if releaseSeries == nil {
		return errors.Errorf("invalid provider metadata: version %s for the provider %s does not match any release series", s.options.Version, name)
	}
	if releaseSeries.Contract != "v1alpha4" && releaseSeries.Contract != "v1beta1" {
		return errors.Errorf(capiVersionIncompatibilityMessage, clusterv1.GroupVersion.Version, releaseSeries.Contract, name)
	}
	s.contract = releaseSeries.Contract
	return nil
}

// fetch fetches the provider components from the repository and processes all yaml manifests.
func (p *phaseReconciler) fetch(ctx context.Context) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	log.V(1).Info("Fetching provider", "name", p.provider.GetName())

	// Fetch the provider components yaml file from the provided repository Github/ConfigMap.
	componentsFile, err := p.repo.GetFile(p.options.Version, p.repo.ComponentsPath())
	if err != nil {
		err = fmt.Errorf("failed to read %q from provider's repository %q: %w", p.repo.ComponentsPath(), p.providerConfig.ManifestLabel(), err)
		return reconcile.Result{}, wrapPhaseError(err, operatorv1.ComponentsFetchErrorReason, operatorv1.PreflightCheckCondition)
	}

	// Generate a set of new objects using the clusterctl library. NewComponents() will do the yaml proccessing,
	// like ensure all the provider components are in proper namespace, replcae variables, etc. See the clusterctl
	// documentation for more details.
	p.components, err = repository.NewComponents(repository.ComponentsInput{
		Provider:     p.providerConfig,
		ConfigClient: p.configClient,
		Processor:    yamlprocessor.NewSimpleProcessor(),
		RawYaml:      componentsFile,
		Options:      p.options})
	if err != nil {
		return reconcile.Result{}, wrapPhaseError(err, operatorv1.ComponentsFetchErrorReason, operatorv1.PreflightCheckCondition)
	}

	// ProviderSpec provides fields for customizing the provider deployment options.
	// We can use clusterctl library to apply this customizations.
	err = repository.AlterComponents(p.components, customizeObjectsFn(p.provider))
	if err != nil {
		return reconcile.Result{}, wrapPhaseError(err, operatorv1.ComponentsFetchErrorReason, operatorv1.PreflightCheckCondition)
	}

	conditions.Set(p.provider, conditions.TrueCondition(operatorv1.PreflightCheckCondition))
	return reconcile.Result{}, nil
}

// preInstall ensure all the clusterctl CRDs are available before installing the provider,
// and delete existing components if required for upgrade.
func (p *phaseReconciler) preInstall(ctx context.Context) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	clusterClient := p.newClusterClient()

	log.V(2).Info("Ensuring clustectl CRDs are installed", "name", p.provider.GetName())
	err := clusterClient.ProviderInventory().EnsureCustomResourceDefinitions()
	if err != nil {
		return reconcile.Result{}, wrapPhaseError(err, "failed installing clusterctl CRDs", operatorv1.ProviderInstalledCondition)
	}

	needPreDelete, err := p.updateRequiresPreDeletion(ctx, p.provider)
	if err != nil || !needPreDelete {
		return reconcile.Result{}, wrapPhaseError(err, "failed getting clusterctl Provider", operatorv1.ProviderInstalledCondition)
	}
	return p.delete(ctx)
}

// updateRequiresPreDeletion get the clusterctl Provider and compare it's version with the Spec.Version
// if different, it's an upgrade.
func (s *phaseReconciler) updateRequiresPreDeletion(ctx context.Context, provider genericprovider.GenericProvider) (bool, error) {
	// TODO: We should replace this with an Installed/Applied version in the providerStatus.
	err := s.ctrlClient.Get(ctx, clusterctlProviderName(provider), s.clusterctlProvider)
	if apierrors.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if s.clusterctlProvider.Version == "" {
		return false, nil
	}

	nextVersion, err := versionutil.ParseSemantic(s.components.Version())
	if err != nil {
		return false, err
	}
	currentVersion, err := versionutil.ParseSemantic(s.clusterctlProvider.Version)
	if err != nil {
		return false, err
	}
	return currentVersion.LessThan(nextVersion), nil
}

// install installs the provider components using clusterctl library.
func (p *phaseReconciler) install(ctx context.Context) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	clusterClient := p.newClusterClient()
	installer := clusterClient.ProviderInstaller()
	installer.Add(p.components)

	log.V(1).Info("Installing provider", "name", p.provider.GetName())
	_, err := installer.Install(cluster.InstallOptions{})
	if err != nil {
		reason := "Install failed"
		if err == wait.ErrWaitTimeout {
			reason = "Timedout waiting for deployment to become ready"
		}
		return reconcile.Result{}, wrapPhaseError(err, reason, operatorv1.ProviderInstalledCondition)
	}

	spec := p.provider.GetSpec()
	if spec.Version == nil {
		// TODO: the proposal says to do this.. but it causes the Generation to bump
		// and thus a repeated Reconcile(). IMHO I think this should really be
		// "status.targetVersion" and then we also have "status.installedVersion" so
		// we can see what we are working towards without having to edit the spec.
		spec.Version = pointer.StringPtr(p.components.Version())
		p.provider.SetSpec(spec)
	}
	status := p.provider.GetStatus()
	status.Contract = &p.contract
	status.ObservedGeneration = p.provider.GetGeneration()
	p.provider.SetStatus(status)

	log.V(1).Info("Provider successfully installed", "name", p.provider.GetName())
	conditions.Set(p.provider, conditions.TrueCondition(operatorv1.ProviderInstalledCondition))
	return reconcile.Result{}, nil
}

// delete deletes the provider components using clusterctl library.
func (p *phaseReconciler) delete(ctx context.Context) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	log.V(1).Info("Deleting provider", "name", p.provider.GetName())

	clusterClient := p.newClusterClient()

	p.clusterctlProvider.Name = clusterctlProviderName(p.provider).Name
	p.clusterctlProvider.Namespace = p.provider.GetNamespace()
	p.clusterctlProvider.Type = string(util.ClusterctlProviderType(p.provider))
	p.clusterctlProvider.ProviderName = p.provider.GetName()
	if p.clusterctlProvider.Version == "" {
		// fake these values to get the delete working in case there is not
		// a real provider (perhaps a failed install).
		p.clusterctlProvider.Version = p.options.Version
	}

	err := clusterClient.ProviderComponents().Delete(cluster.DeleteOptions{
		Provider:         *p.clusterctlProvider,
		IncludeNamespace: false,
		IncludeCRDs:      false,
	})
	return reconcile.Result{}, wrapPhaseError(err, operatorv1.OldComponentsDeletionErrorReason, operatorv1.ProviderInstalledCondition)
}

func clusterctlProviderName(provider genericprovider.GenericProvider) client.ObjectKey {
	prefix := ""
	switch provider.GetObject().(type) {
	case *operatorv1.BootstrapProvider:
		prefix = "bootstrap-"
	case *operatorv1.ControlPlaneProvider:
		prefix = "control-plane-"
	case *operatorv1.InfrastructureProvider:
		prefix = "infrastructure-"
	}

	return client.ObjectKey{Name: prefix + provider.GetName(), Namespace: provider.GetNamespace()}
}

// newClusterClient returns a clusterctl client for interacting with management cluster.
func (s *phaseReconciler) newClusterClient() cluster.Client {
	return cluster.New(cluster.Kubeconfig{}, s.configClient, cluster.InjectProxy(&controllerProxy{
		ctrlClient: s.ctrlClient,
		ctrlConfig: s.ctrlConfig,
	}))
}
