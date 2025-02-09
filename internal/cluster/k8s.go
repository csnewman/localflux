package cluster

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	memory "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var decUnstructured = yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)

func DefaultKubeConfigPath() string {
	if home := homedir.HomeDir(); home != "" {
		return filepath.Join(home, ".kube", "config")
	}

	return ""
}

type K8sClient struct {
	clientset *kubernetes.Clientset
	dyn       *dynamic.DynamicClient
	mapper    *restmapper.DeferredDiscoveryRESTMapper
}

func NewK8sClientForCtx(configPath string, name string) (*K8sClient, error) {
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{
			ExplicitPath: configPath,
		},
		&clientcmd.ConfigOverrides{
			CurrentContext: name,
		},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create: %w", err)
	}

	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create: %w", err)
	}

	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(clientset.Discovery()))

	return &K8sClient{
		clientset: clientset,
		dyn:       dyn,
		mapper:    mapper,
	}, nil
}

func (c *K8sClient) Apply(ctx context.Context, data string) error {
	multidocReader := utilyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(data)))

	for {
		buf, err := multidocReader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read multidoc: %w", err)
		}

		obj := &unstructured.Unstructured{}

		_, gvk, err := decUnstructured.Decode(buf, nil, obj)
		if err != nil {
			return fmt.Errorf("failed to decode doc: %w", err)
		}

		mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return fmt.Errorf("failed to get mapping: %w", err)
		}

		var dr dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			dr = c.dyn.Resource(mapping.Resource).Namespace(obj.GetNamespace())
		} else {
			dr = c.dyn.Resource(mapping.Resource)
		}

		encoded, err := json.Marshal(obj)
		if err != nil {
			return fmt.Errorf("failed to encode doc: %w", err)
		}

		force := true

		if _, err := dr.Patch(ctx, obj.GetName(), types.ApplyPatchType, encoded, metav1.PatchOptions{
			FieldManager: "localflux",
			Force:        &force,
		}); err != nil {
			return fmt.Errorf("failed to patch doc: %w", err)
		}
	}

	return nil
}
