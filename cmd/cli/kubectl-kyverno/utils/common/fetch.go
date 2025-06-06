package common

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/go-git/go-billy/v5"
	kyvernov1 "github.com/kyverno/kyverno/api/kyverno/v1"
	"github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/apis/v1alpha1"
	"github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/log"
	"github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/resource"
	"github.com/kyverno/kyverno/pkg/admissionpolicy"
	"github.com/kyverno/kyverno/pkg/autogen"
	"github.com/kyverno/kyverno/pkg/clients/dclient"
	engineapi "github.com/kyverno/kyverno/pkg/engine/api"
	kubeutils "github.com/kyverno/kyverno/pkg/utils/kube"
	utils "github.com/kyverno/kyverno/pkg/utils/restmapper"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GetResources gets matched resources by the given policies
// the resources are fetched from
// - local paths to resources, if given
// - the k8s cluster, if given
func GetResources(
	out io.Writer,
	policies []engineapi.GenericPolicy,
	resourcePaths []string,
	dClient dclient.Interface,
	cluster bool,
	namespace string,
	policyReport bool,
	clusterWideResources bool,
) ([]*unstructured.Unstructured, error) {
	resources := make([]*unstructured.Unstructured, 0)
	var err error

	if cluster && dClient != nil {
		resources, err = fetchResourcesFromPolicy(out, policies, resourcePaths, dClient, namespace, policyReport, clusterWideResources)
		if err != nil {
			return resources, err
		}
	} else if len(resourcePaths) > 0 {
		resources, err = whenClusterIsFalse(out, resourcePaths, policyReport)
		if err != nil {
			return resources, err
		}
	}
	return resources, err
}

func fetchResourcesFromPolicy(
	out io.Writer,
	policies []engineapi.GenericPolicy,
	resourcePaths []string,
	dClient dclient.Interface,
	namespace string,
	policyReport bool,
	clusterWideResources bool,
) ([]*unstructured.Unstructured, error) {
	var resources []*unstructured.Unstructured
	var err error

	resourceTypesMap := make(map[schema.GroupVersionKind]bool)
	var subresourceMap map[schema.GroupVersionKind]v1alpha1.Subresource

	for _, policy := range policies {
		if kpol := policy.AsKyvernoPolicy(); kpol != nil {
			for _, rule := range autogen.Default.ComputeRules(kpol, "") {
				var resourceTypesInRule map[schema.GroupVersionKind]bool
				resourceTypesInRule, subresourceMap = GetKindsFromRule(rule, dClient, clusterWideResources)
				for resourceKind := range resourceTypesInRule {
					resourceTypesMap[resourceKind] = true
				}
			}
		} else if vap := policy.AsValidatingAdmissionPolicy(); vap != nil {
			definition := vap.GetDefinition()
			var resourceTypesInRule map[schema.GroupVersionKind]bool
			resourceTypesInRule, subresourceMap = getKindsFromValidatingAdmissionPolicy(*definition, dClient, clusterWideResources)
			if err != nil {
				return resources, err
			}
			for resourceKind := range resourceTypesInRule {
				resourceTypesMap[resourceKind] = true
			}
		}
	}

	resourceTypes := make([]schema.GroupVersionKind, 0, len(resourceTypesMap))
	for kind := range resourceTypesMap {
		resourceTypes = append(resourceTypes, kind)
	}

	resources, err = whenClusterIsTrue(out, resourceTypes, subresourceMap, dClient, namespace, resourcePaths, policyReport)

	return resources, err
}

func whenClusterIsTrue(out io.Writer, resourceTypes []schema.GroupVersionKind, subresourceMap map[schema.GroupVersionKind]v1alpha1.Subresource, dClient dclient.Interface, namespace string, resourcePaths []string, policyReport bool) ([]*unstructured.Unstructured, error) {
	resources := make([]*unstructured.Unstructured, 0)
	resourceMap, err := getResourcesOfTypeFromCluster(out, resourceTypes, subresourceMap, dClient, namespace)
	if err != nil {
		return nil, err
	}
	if len(resourcePaths) == 0 {
		for _, rr := range resourceMap {
			resources = append(resources, rr)
		}
	} else {
		for _, resourcePath := range resourcePaths {
			lenOfResource := len(resources)
			for rn, rr := range resourceMap {
				s := strings.Split(rn, "-")
				if s[2] == resourcePath {
					resources = append(resources, rr)
				}
			}
			if lenOfResource >= len(resources) {
				if policyReport {
					log.Log.V(3).Info(fmt.Sprintf("%s not found in cluster", resourcePath))
				} else {
					fmt.Fprintf(out, "\n----------------------------------------------------------------------\nresource %s not found in cluster\n----------------------------------------------------------------------\n", resourcePath)
				}
				return nil, fmt.Errorf("%s not found in cluster", resourcePath)
			}
		}
	}
	return resources, nil
}

func whenClusterIsFalse(out io.Writer, resourcePaths []string, policyReport bool) ([]*unstructured.Unstructured, error) {
	resources := make([]*unstructured.Unstructured, 0)
	for _, resourcePath := range resourcePaths {
		resourceBytes, err := resource.GetFileBytes(resourcePath)
		if err != nil {
			if policyReport {
				log.Log.V(3).Info(fmt.Sprintf("failed to load resources: %s.", resourcePath), "error", err)
			} else {
				fmt.Fprintf(out, "\n----------------------------------------------------------------------\nfailed to load resources: %s. \nerror: %s\n----------------------------------------------------------------------\n", resourcePath, err)
			}
			continue
		}

		getResources, err := resource.GetUnstructuredResources(resourceBytes)
		if err != nil {
			return nil, err
		}

		resources = append(resources, getResources...)
	}
	return resources, nil
}

// GetResourcesWithTest with gets matched resources by the given policies
func GetResourcesWithTest(out io.Writer, fs billy.Filesystem, resourcePaths []string, policyResourcePath string) ([]*unstructured.Unstructured, error) {
	resources := make([]*unstructured.Unstructured, 0)
	if len(resourcePaths) > 0 {
		for _, resourcePath := range resourcePaths {
			var resourceBytes []byte
			var err error
			if fs != nil {
				filep, err := fs.Open(filepath.Join(policyResourcePath, resourcePath))
				if err != nil {
					fmt.Fprintf(out, "Unable to open resource file: %s. error: %s", resourcePath, err)
					continue
				}
				resourceBytes, _ = io.ReadAll(filep)
			} else {
				resourceBytes, err = resource.GetFileBytes(resourcePath)
			}
			if err != nil {
				fmt.Fprintf(out, "\n----------------------------------------------------------------------\nfailed to load resources: %s. \nerror: %s\n----------------------------------------------------------------------\n", resourcePath, err)
				continue
			}

			getResources, err := resource.GetUnstructuredResources(resourceBytes)
			if err != nil {
				return nil, err
			}

			resources = append(resources, getResources...)
		}
	}
	return resources, nil
}

func getResourcesOfTypeFromCluster(out io.Writer, resourceTypes []schema.GroupVersionKind, subresourceMap map[schema.GroupVersionKind]v1alpha1.Subresource, dClient dclient.Interface, namespace string) (map[string]*unstructured.Unstructured, error) {
	r := make(map[string]*unstructured.Unstructured)
	for _, kind := range resourceTypes {
		resourceList, err := dClient.ListResource(context.TODO(), kind.GroupVersion().String(), kind.Kind, namespace, nil)
		if err != nil {
			continue
		}
		gvk := resourceList.GroupVersionKind()
		for _, resource := range resourceList.Items {
			key := kind.Kind + "-" + resource.GetNamespace() + "-" + resource.GetName()
			resource.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   gvk.Group,
				Version: gvk.Version,
				Kind:    kind.Kind,
			})
			r[key] = resource.DeepCopy()
		}
	}
	for _, subresource := range subresourceMap {
		parentGV := schema.GroupVersion{Group: subresource.ParentResource.Group, Version: subresource.ParentResource.Version}
		resourceList, err := dClient.ListResource(context.TODO(), parentGV.String(), subresource.ParentResource.Kind, namespace, nil)
		if err != nil {
			continue
		}
		parentResourceNames := make([]string, 0)
		for _, resource := range resourceList.Items {
			parentResourceNames = append(parentResourceNames, resource.GetName())
		}
		for _, parentResourceName := range parentResourceNames {
			subresourceName := strings.Split(subresource.Subresource.Name, "/")[1]
			resource, err := dClient.GetResource(context.TODO(), parentGV.String(), subresource.ParentResource.Kind, namespace, parentResourceName, subresourceName)
			if err != nil {
				fmt.Fprintf(out, "Error: %s", err.Error())
				continue
			}
			key := subresource.Subresource.Kind + "-" + resource.GetNamespace() + "-" + resource.GetName()
			resource.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   subresource.Subresource.Group,
				Version: subresource.Subresource.Version,
				Kind:    subresource.Subresource.Kind,
			})
			r[key] = resource.DeepCopy()
		}
	}
	return r, nil
}

// GetPatchedAndGeneratedResource converts raw bytes to unstructured object
func GetPatchedAndGeneratedResource(resourceBytes []byte) (unstructured.Unstructured, error) {
	getResource, err := resource.GetUnstructuredResources(resourceBytes)
	if err != nil {
		return unstructured.Unstructured{}, err
	}
	if len(getResource) > 0 && getResource[0] != nil {
		resource := *getResource[0]
		return resource, nil
	}
	return unstructured.Unstructured{}, err
}

// GetKindsFromRule will return the kinds from policy match block
func GetKindsFromRule(rule kyvernov1.Rule, client dclient.Interface, clusterWideResources bool) (map[schema.GroupVersionKind]bool, map[schema.GroupVersionKind]v1alpha1.Subresource) {
	resourceTypesMap := make(map[schema.GroupVersionKind]bool)
	subresourceMap := make(map[schema.GroupVersionKind]v1alpha1.Subresource)
	for _, kind := range rule.MatchResources.Kinds {
		addGVKToResourceTypesMap(kind, resourceTypesMap, subresourceMap, client, clusterWideResources)
	}
	if rule.MatchResources.Any != nil {
		for _, resFilter := range rule.MatchResources.Any {
			for _, kind := range resFilter.ResourceDescription.Kinds {
				addGVKToResourceTypesMap(kind, resourceTypesMap, subresourceMap, client, clusterWideResources)
			}
		}
	}
	if rule.MatchResources.All != nil {
		for _, resFilter := range rule.MatchResources.All {
			for _, kind := range resFilter.ResourceDescription.Kinds {
				addGVKToResourceTypesMap(kind, resourceTypesMap, subresourceMap, client, clusterWideResources)
			}
		}
	}
	return resourceTypesMap, subresourceMap
}

func getKindsFromValidatingAdmissionPolicy(policy admissionregistrationv1.ValidatingAdmissionPolicy, client dclient.Interface, clusterWideResources bool) (map[schema.GroupVersionKind]bool, map[schema.GroupVersionKind]v1alpha1.Subresource) {
	resourceTypesMap := make(map[schema.GroupVersionKind]bool)
	subresourceMap := make(map[schema.GroupVersionKind]v1alpha1.Subresource)

	restMapper, err := utils.GetRESTMapper(client, false)
	if err != nil {
		log.Log.V(3).Info("failed to get rest mapper", "error", err)
		return resourceTypesMap, subresourceMap
	}
	kinds, err := admissionpolicy.GetKinds(policy.Spec.MatchConstraints, restMapper)
	if err != nil {
		log.Log.V(3).Info("failed to get kinds from validating admission policy", "error", err)
		return resourceTypesMap, subresourceMap
	}
	for _, kind := range kinds {
		addGVKToResourceTypesMap(kind, resourceTypesMap, subresourceMap, client, clusterWideResources)
	}

	return resourceTypesMap, subresourceMap
}

func addGVKToResourceTypesMap(kind string, resourceTypesMap map[schema.GroupVersionKind]bool, subresourceMap map[schema.GroupVersionKind]v1alpha1.Subresource, client dclient.Interface, clusterWideResources bool) {
	group, version, kind, subresource := kubeutils.ParseKindSelector(kind)
	gvrss, err := client.Discovery().FindResources(group, version, kind, subresource)
	if err != nil {
		log.Log.V(2).Info("failed to find resource", "kind", kind, "error", err)
		return
	}
	for parent, child := range gvrss {
		if clusterWideResources && child.Namespaced {
			continue
		}

		// The resource is not a subresource
		if parent.SubResource == "" {
			resourceTypesMap[parent.GroupVersionKind()] = true
		} else {
			gvk := schema.GroupVersionKind{
				Group: child.Group, Version: child.Version, Kind: child.Kind,
			}
			subresourceMap[gvk] = v1alpha1.Subresource{
				Subresource: child,
				ParentResource: metav1.APIResource{
					Group:   parent.Group,
					Version: parent.Version,
					Kind:    parent.Kind,
					Name:    parent.Resource,
				},
			}
		}
	}
}
