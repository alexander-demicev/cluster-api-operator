package controller

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pkg/errors"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/repository"
	utilyaml "sigs.k8s.io/cluster-api/util/yaml"

	addonsv1 "sigs.k8s.io/cluster-api/api/addons/v1beta1"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1beta1"
	expv1 "sigs.k8s.io/cluster-api/exp/api/v1beta1"
)

var (
	// Scheme contains a set of API resources used by clusterctl.
	newscheme = runtime.NewScheme()
)

func init() {
	_ = clientgoscheme.AddToScheme(newscheme)
	_ = clusterctlv1.AddToScheme(newscheme)
	_ = clusterv1.AddToScheme(newscheme)
	_ = apiextensionsv1.AddToScheme(newscheme)
	_ = apiextensionsv1beta1.AddToScheme(newscheme)
	_ = admissionregistrationv1.AddToScheme(newscheme)
	_ = admissionregistrationv1beta1.AddToScheme(newscheme)
	_ = controlplanev1.AddToScheme(newscheme)
	_ = expv1.AddToScheme(newscheme)
	_ = addonsv1.AddToScheme(newscheme)
}

const (
	clusterRoleKind                    = "ClusterRole"
	clusterRoleBindingKind             = "ClusterRoleBinding"
	roleBindingKind                    = "RoleBinding"
	certificateKind                    = "Certificate"
	mutatingWebhookConfigurationKind   = "MutatingWebhookConfiguration"
	validatingWebhookConfigurationKind = "ValidatingWebhookConfiguration"
	customResourceDefinitionKind       = "CustomResourceDefinition"
)

// components is a struct that implements the Components interface.
type components struct {
	config.Provider
	version         string
	variables       []string
	images          []string
	targetNamespace string
	objs            []unstructured.Unstructured
}

func (c *components) Version() string {
	return c.version
}

func (c *components) Variables() []string {
	return c.variables
}

func (c *components) Images() []string {
	return c.images
}

func (c *components) TargetNamespace() string {
	return c.targetNamespace
}

func (c *components) InventoryObject() clusterctlv1.Provider {
	labels := getCommonLabels(c.Provider)
	labels[clusterctlv1.ClusterctlCoreLabel] = clusterctlv1.ClusterctlCoreLabelInventoryValue

	return clusterctlv1.Provider{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterctlv1.GroupVersion.String(),
			Kind:       "Provider",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: c.targetNamespace,
			Name:      c.ManifestLabel(),
			Labels:    labels,
		},
		ProviderName: c.Name(),
		Type:         string(c.Type()),
		Version:      c.version,
	}
}

func (c *components) Objs() []unstructured.Unstructured {
	return c.objs
}

func (c *components) Yaml() ([]byte, error) {
	return utilyaml.FromUnstructured(c.objs)
}

// ensure components implement Components.
var _ repository.Components = &components{}

// NewComponents returns a new objects embedding a component YAML file
//
// It is important to notice that clusterctl applies a set of processing steps to the “raw” component YAML read
// from the provider repositories:
// 1. Checks for all the variables in the component YAML file and replace with corresponding config values
// 2. The variables replacement can be skipped using the SkipTemplateProcess flag in the input options
// 3. Ensure all the provider components are deployed in the target namespace (apply only to namespaced objects)
// 4. Ensure all the ClusterRoleBinding which are referencing namespaced objects have the name prefixed with the namespace name
// 5. Adds labels to all the components in order to allow easy identification of the provider objects.
func NewComponents(input repository.ComponentsInput) (repository.Components, error) {
	cache := GetComponentCache()
	if cachedComponents, found := cache.Get(input); found {
		deepCopiedObjs := make([]unstructured.Unstructured, len(cachedComponents.Objs()))
		for i, obj := range cachedComponents.Objs() {
			objCopy := obj.DeepCopy()
			deepCopiedObjs[i] = *objCopy
		}

		// Create a new components instance with the deep-copied objects
		cleanComponents := &components{
			Provider:        cachedComponents.(*components).Provider,
			version:         cachedComponents.(*components).version,
			variables:       cachedComponents.(*components).variables,
			images:          cachedComponents.(*components).images,
			targetNamespace: cachedComponents.(*components).targetNamespace,
			objs:            deepCopiedObjs,
		}

		return cleanComponents, nil
	}

	cache.LockProcess()
	defer cache.UnlockProcess()

	variables, err := input.Processor.GetVariables(input.RawYaml)
	if err != nil {
		return nil, err
	}

	// If requested, we are skipping the call to the template processor; however, it is important to
	// notice that this could work only if the rawYaml is a valid yaml by itself.
	processedYaml := input.RawYaml
	if !input.Options.SkipTemplateProcess {
		processedYaml, err = input.Processor.Process(input.RawYaml, input.ConfigClient.Variables().Get)
		if err != nil {
			return nil, errors.Wrap(err, "failed to perform variable substitution")
		}
	}

	// Transform the yaml in a list of objects, so following transformation can work on typed objects (instead of working on a string/slice of bytes)
	objs, err := utilyaml.ToUnstructured(processedYaml)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse yaml")
	}

	// Apply image overrides, if defined
	objs, err = fixImages(objs, func(image string) (string, error) {
		return input.ConfigClient.ImageMeta().AlterImage(input.Provider.ManifestLabel(), image)
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to apply image overrides")
	}

	// Inspect the list of objects for the images required by the provider component.
	images, err := inspectImages(objs)
	if err != nil {
		return nil, errors.Wrap(err, "failed to detect required images")
	}

	// inspect the list of objects for the default target namespace
	defaultTargetNamespace, err := inspectTargetNamespace(objs)
	if err != nil {
		return nil, errors.Wrap(err, "failed to detect default target namespace")
	}

	// Ensures all the provider components are deployed in the target namespace (apply only to namespaced objects)
	if input.Options.TargetNamespace == "" {
		input.Options.TargetNamespace = defaultTargetNamespace
	}

	if input.Options.TargetNamespace == "" {
		return nil, errors.New("target namespace can't be defaulted. Please specify a target namespace")
	}

	// add a Namespace object if missing
	objs = addNamespaceIfMissing(objs, input.Options.TargetNamespace)

	// fix Namespace name in all the objects
	objs, err = fixTargetNamespace(objs, input.Options.TargetNamespace)
	if err != nil {
		return nil, errors.Wrap(err, "failed to set the TargetNamespace on the components")
	}

	// Add common labels
	objs = addCommonLabels(objs, input.Provider)

	// Deploying cert-manager objects and especially Certificates before Mutating-
	// ValidatingWebhookConfigurations and CRDs ensures cert-manager's ca-injector
	// receives the event for the objects at the right time to inject the new CA.
	sort.SliceStable(objs, func(i, j int) bool {
		// First prioritize Namespaces over everything.
		if objs[i].GetKind() == "Namespace" {
			return true
		}
		if objs[j].GetKind() == "Namespace" {
			return false
		}

		// Second prioritize cert-manager objects.
		isCertManager := objs[i].GroupVersionKind().Group == "cert-manager.io"
		return isCertManager
	})

	// Store the original objects in the components for caching
	cacheComponents := &components{
		Provider:        input.Provider,
		version:         input.Options.Version,
		variables:       variables,
		images:          images,
		targetNamespace: input.Options.TargetNamespace,
		objs:            objs,
	}

	cache.Put(input, cacheComponents)

	deepCopiedObjs := make([]unstructured.Unstructured, len(objs))
	for i, obj := range objs {
		objCopy := obj.DeepCopy() // Use copy because something later mutates objects
		deepCopiedObjs[i] = *objCopy
	}

	returnComponents := &components{
		Provider:        input.Provider,
		version:         input.Options.Version,
		variables:       variables,
		images:          images,
		targetNamespace: input.Options.TargetNamespace,
		objs:            deepCopiedObjs,
	}

	return returnComponents, nil
}

func inspectImages(objs []unstructured.Unstructured) ([]string, error) {
	images := []string{}

	for i := range objs {
		o := objs[i]

		var podSpec corev1.PodSpec

		switch o.GetKind() {
		case deploymentKind:
			d := &appsv1.Deployment{}
			if err := newscheme.Convert(&o, d, nil); err != nil {
				return nil, err
			}
			podSpec = d.Spec.Template.Spec
		case daemonSetKind:
			d := &appsv1.DaemonSet{}
			if err := newscheme.Convert(&o, d, nil); err != nil {
				return nil, err
			}
			podSpec = d.Spec.Template.Spec
		default:
			continue
		}

		for _, c := range podSpec.Containers {
			images = append(images, c.Image)
		}

		for _, c := range podSpec.InitContainers {
			images = append(images, c.Image)
		}
	}

	return images, nil
}

// inspectTargetNamespace identifies the name of the namespace object contained in the components YAML, if any.
// In case more than one Namespace object is identified, an error is returned.
func inspectTargetNamespace(objs []unstructured.Unstructured) (string, error) {
	namespace := ""
	for _, o := range objs {
		// if the object has Kind Namespace
		if o.GetKind() == namespaceKind {
			// grab the name (or error if there is more than one Namespace object)
			if namespace != "" {
				return "", errors.New("Invalid manifest. There should be no more than one resource with Kind Namespace in the provider components yaml")
			}
			namespace = o.GetName()
		}
	}
	return namespace, nil
}

// addNamespaceIfMissing adda a Namespace object if missing (this ensure the targetNamespace will be created).
func addNamespaceIfMissing(objs []unstructured.Unstructured, targetNamespace string) []unstructured.Unstructured {
	namespaceObjectFound := false
	for _, o := range objs {
		// if the object has Kind Namespace, fix the namespace name
		if o.GetKind() == namespaceKind {
			namespaceObjectFound = true
		}
	}

	// if there isn't an object with Kind Namespace, add it
	if !namespaceObjectFound {
		objs = append(objs, unstructured.Unstructured{
			Object: map[string]interface{}{
				"kind": namespaceKind,
				"metadata": map[string]interface{}{
					"name": targetNamespace,
				},
			},
		})
	}

	return objs
}

// fixTargetNamespace ensures all the provider components are deployed in the target namespace (apply only to namespaced objects).
func fixTargetNamespace(objs []unstructured.Unstructured, targetNamespace string) ([]unstructured.Unstructured, error) {
	for i := range objs {
		o := objs[i]

		// if the object has Kind Namespace, fix the namespace name
		if o.GetKind() == namespaceKind {
			o.SetName(targetNamespace)
		}

		originalNamespace := o.GetNamespace()

		// if the object is namespaced, set the namespace name
		if IsResourceNamespaced(o.GetKind()) {
			o.SetNamespace(targetNamespace)
		}

		switch o.GetKind() {
		case clusterRoleBindingKind:
			// Convert Unstructured into a typed object
			binding := &rbacv1.ClusterRoleBinding{}
			if err := newscheme.Convert(&o, binding, nil); err != nil {
				return nil, err
			}

			// ensure that namespaced subjects refers to targetNamespace
			for s := range binding.Subjects {
				if binding.Subjects[s].Namespace != "" {
					binding.Subjects[s].Namespace = targetNamespace
				}
			}

			// Convert ClusterRoleBinding back to Unstructured
			if err := newscheme.Convert(binding, &o, nil); err != nil {
				return nil, err
			}

		case roleBindingKind:
			binding := &rbacv1.RoleBinding{}
			if err := newscheme.Convert(&o, binding, nil); err != nil {
				return nil, err
			}

			// ensure that namespaced subjects refers to targetNamespace
			for k := range binding.Subjects {
				if binding.Subjects[k].Namespace != "" {
					binding.Subjects[k].Namespace = targetNamespace
				}
			}

			// Convert RoleBinding back to Unstructured
			if err := newscheme.Convert(binding, &o, nil); err != nil {
				return nil, err
			}

		case mutatingWebhookConfigurationKind, validatingWebhookConfigurationKind, customResourceDefinitionKind:
			var err error
			o, err = fixWebhookNamespaceReferences(o, targetNamespace)
			if err != nil {
				return nil, err
			}

		case certificateKind:
			var err error
			o, err = fixCertificate(o, originalNamespace, targetNamespace)
			if err != nil {
				return nil, err
			}
		}

		objs[i] = o
	}
	return objs, nil
}

func fixWebhookNamespaceReferences(o unstructured.Unstructured, targetNamespace string) (unstructured.Unstructured, error) {
	annotations := o.GetAnnotations()
	secretNamespacedName, ok := annotations["cert-manager.io/inject-ca-from"]
	if ok {
		secretNameSplit := strings.Split(secretNamespacedName, "/")
		if len(secretNameSplit) != 2 {
			return o, fmt.Errorf("object %s %s does not have a correct value for cert-manager.io/inject-ca-from", o.GetKind(), o.GetName())
		}
		annotations["cert-manager.io/inject-ca-from"] = targetNamespace + "/" + secretNameSplit[1]
		o.SetAnnotations(annotations)
	}

	switch o.GetKind() {
	case "MutatingWebhookConfiguration", "ValidatingWebhookConfiguration":
		return fixWebhookNamespace(&o, targetNamespace)

	case "CustomResourceDefinition":
		return fixCRDWebhookNamespaceReference(&o, targetNamespace)

	default:
		return unstructured.Unstructured{}, fmt.Errorf("unsupported kind %s", o.GetKind())
	}
}

func fixWebhookNamespace(o *unstructured.Unstructured, targetNamespace string) (unstructured.Unstructured, error) {
	webhooksPath := "webhooks"

	webhooks, found, err := unstructured.NestedSlice(o.Object, webhooksPath)
	if err != nil || !found {
		return unstructured.Unstructured{}, fmt.Errorf("failed to find webhooks in %s %s", o.GetKind(), o.GetName())
	}

	for i, w := range webhooks {
		wh, ok := w.(map[string]interface{})
		if !ok {
			continue
		}

		servicePath := []string{"clientConfig", "service", "namespace"}
		if _, found, _ := unstructured.NestedString(wh, servicePath...); found {
			if err := unstructured.SetNestedField(wh, targetNamespace, servicePath...); err != nil {
				return unstructured.Unstructured{}, fmt.Errorf("failed to update service namespace for webhook %d in %s %s", i, o.GetKind(), o.GetName())
			}
		}
		webhooks[i] = wh
	}

	if err := unstructured.SetNestedSlice(o.Object, webhooks, webhooksPath); err != nil {
		return unstructured.Unstructured{}, fmt.Errorf("failed to set updated webhooks in %s %s", o.GetKind(), o.GetName())
	}

	return *o, nil
}

func fixCRDWebhookNamespaceReference(o *unstructured.Unstructured, targetNamespace string) (unstructured.Unstructured, error) {
	conversionPath := []string{"spec", "conversion", "webhook", "clientConfig", "service", "namespace"}

	if _, found, _ := unstructured.NestedString(o.Object, conversionPath...); found {
		if err := unstructured.SetNestedField(o.Object, targetNamespace, conversionPath...); err != nil {
			return unstructured.Unstructured{}, fmt.Errorf("failed to update CRD webhook namespace in %s %s", o.GetKind(), o.GetName())
		}
	}

	return *o, nil
}

// fixCertificate fixes the dnsNames of cert-manager Certificates. The DNS names contain the dns names of the provider
// services (including the namespace) and thus have to be modified to use the target namespace instead.
func fixCertificate(o unstructured.Unstructured, originalNamespace, targetNamespace string) (unstructured.Unstructured, error) {
	dnsNames, ok, err := unstructured.NestedStringSlice(o.UnstructuredContent(), "spec", "dnsNames")
	if err != nil {
		return o, errors.Wrapf(err, "failed to get .spec.dnsNames from Certificate %s/%s", o.GetNamespace(), o.GetName())
	}
	// Return if we don't find .spec.dnsNames.
	if !ok {
		return o, nil
	}

	// Iterate through dnsNames and adjust the namespace.
	// The dnsNames slice usually looks like this:
	// - $(SERVICE_NAME).$(SERVICE_NAMESPACE).svc
	// - $(SERVICE_NAME).$(SERVICE_NAMESPACE).svc.cluster.local
	for i, dnsName := range dnsNames {
		dnsNames[i] = strings.Replace(dnsName, fmt.Sprintf(".%s.", originalNamespace), fmt.Sprintf(".%s.", targetNamespace), 1)
	}

	if err := unstructured.SetNestedStringSlice(o.UnstructuredContent(), dnsNames, "spec", "dnsNames"); err != nil {
		return o, errors.Wrapf(err, "failed to set .spec.dnsNames to Certificate %s/%s", o.GetNamespace(), o.GetName())
	}

	return o, nil
}

// addCommonLabels ensures all the provider components have a consistent set of labels.
func addCommonLabels(objs []unstructured.Unstructured, provider config.Provider) []unstructured.Unstructured {
	for _, o := range objs {
		labels := o.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		for k, v := range getCommonLabels(provider) {
			labels[k] = v
		}
		o.SetLabels(labels)
	}

	return objs
}

func getCommonLabels(provider config.Provider) map[string]string {
	return map[string]string{
		clusterctlv1.ClusterctlLabel: "",
		clusterv1.ProviderNameLabel:  provider.ManifestLabel(),
	}
}

// IsResourceNamespaced returns true if the resource kind is namespaced.
func IsResourceNamespaced(kind string) bool {
	switch kind {
	case "Namespace",
		"Node",
		"PersistentVolume",
		"PodSecurityPolicy",
		"CertificateSigningRequest",
		"ClusterRoleBinding",
		"ClusterRole",
		"VolumeAttachment",
		"StorageClass",
		"CSIDriver",
		"CSINode",
		"ValidatingWebhookConfiguration",
		"MutatingWebhookConfiguration",
		"CustomResourceDefinition",
		"PriorityClass",
		"RuntimeClass":
		return false
	default:
		return true
	}
}

// ComponentsAlterFn defines the function that is used to alter the components.Objs().
type ComponentsAlterFn func(objs []unstructured.Unstructured) ([]unstructured.Unstructured, error)

// AlterComponents provides a mechanism to alter the component.Objs from outside
// the repository module.
func AlterComponents(comps repository.Components, alterFn ComponentsAlterFn) error {
	c, ok := comps.(*components)
	if !ok {
		return errors.New("could not alter components as Components is not of the correct type")
	}

	alteredObjs, err := alterFn(c.Objs())
	if err != nil {
		return err
	}
	c.objs = alteredObjs
	return nil
}
