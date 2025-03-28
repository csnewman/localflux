package cluster

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aojea/rwconn"
	"github.com/csnewman/localflux/internal/deployment/v1alpha1"
	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	sourcev1b2 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/go-logr/logr"
	"io"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/httpstream"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	memory "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientsetscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	cmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"net"
	"net/http"
	controllerclient "sigs.k8s.io/controller-runtime/pkg/client"
	controllerlog "sigs.k8s.io/controller-runtime/pkg/log"
	"slices"
	"strings"
	"time"
)

const LFNamespace = "localflux"

var decUnstructured = yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)

func init() {
	controllerlog.SetLogger(logr.Discard())
}

type K8sClient struct {
	clientset       *kubernetes.Clientset
	dyn             *dynamic.DynamicClient
	mapper          *restmapper.DeferredDiscoveryRESTMapper
	controller      controllerclient.Client
	config          *rest.Config
	restClient      *restclient.RESTClient
	cachedDiscovery discovery.CachedDiscoveryInterface
	rawConfig       cmdapi.Config
}

func GetFlattenedConfig(path string, name string) (*cmdapi.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if len(path) > 0 {
		loadingRules = &clientcmd.ClientConfigLoadingRules{ExplicitPath: path}
	}

	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		&clientcmd.ConfigOverrides{
			CurrentContext: name,
		},
	)

	configRaw, err := loader.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load: %w", err)
	}

	if err := cmdapi.MinifyConfig(&configRaw); err != nil {
		return nil, fmt.Errorf("failed to minify: %w", err)
	}

	if err := cmdapi.FlattenConfig(&configRaw); err != nil {
		return nil, fmt.Errorf("failed to flatten: %w", err)
	}

	return &configRaw, nil
}

func NewK8sClientForCtx(configPath string, name string) (*K8sClient, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if len(configPath) > 0 {
		loadingRules = &clientcmd.ClientConfigLoadingRules{ExplicitPath: configPath}
	}

	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		&clientcmd.ConfigOverrides{
			CurrentContext: name,
		},
	)

	rawConfig, err := loader.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load raw config: %w", err)
	}

	config, err := loader.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	return NewK8sClientFromConfig(config, rawConfig)
}

func NewK8sClientFromConfig(config *restclient.Config, rawConfig cmdapi.Config) (*K8sClient, error) {
	if err := sourcev1b2.AddToScheme(clientsetscheme.Scheme); err != nil {
		return nil, fmt.Errorf("failed to load scheme: %w", err)
	}

	if err := v1alpha1.AddToScheme(clientsetscheme.Scheme); err != nil {
		return nil, fmt.Errorf("failed to load scheme: %w", err)
	}

	if err := kustomizev1.AddToScheme(clientsetscheme.Scheme); err != nil {
		return nil, fmt.Errorf("failed to load scheme: %w", err)
	}

	if err := sourcev1b2.AddToScheme(clientsetscheme.Scheme); err != nil {
		return nil, fmt.Errorf("failed to load scheme: %w", err)
	}

	if err := helmv2.AddToScheme(clientsetscheme.Scheme); err != nil {
		return nil, fmt.Errorf("failed to load scheme: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create: %w", err)
	}

	controller, err := controllerclient.New(config, controllerclient.Options{
		Scheme: clientsetscheme.Scheme,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create: %w", err)
	}

	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create: %w", err)
	}

	cachedDiscovery := memory.NewMemCacheClient(clientset.Discovery())
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedDiscovery)

	if err := setKubernetesDefaults(config); err != nil {
		return nil, fmt.Errorf("failed to setKubernetesDefaults: %w", err)
	}

	restClient, err := restclient.RESTClientFor(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create rest client: %w", err)
	}

	return &K8sClient{
		clientset:       clientset,
		dyn:             dyn,
		cachedDiscovery: cachedDiscovery,
		mapper:          mapper,
		controller:      controller,
		config:          config,
		restClient:      restClient,
		rawConfig:       rawConfig,
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

func (c *K8sClient) CreateNamespace(ctx context.Context, name string) error {
	_, err := c.clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: name,
		},
		Spec: corev1.NamespaceSpec{},
	}, metav1.CreateOptions{})

	if apierrors.IsAlreadyExists(err) {
		return nil
	}

	return err
}

func (c *K8sClient) PatchSSA(ctx context.Context, obj controllerclient.Object) error {
	u := &unstructured.Unstructured{}
	u.Object, _ = runtime.DefaultUnstructuredConverter.ToUnstructured(obj)

	return c.controller.Patch(ctx, u, controllerclient.Apply, controllerclient.ForceOwnership, controllerclient.FieldOwner("localflux"))
}

func (c *K8sClient) WaitNamespaceReady(ctx context.Context, ns []string, cb func(names []string)) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second*120)
	defer cancel()

	timer := time.NewTicker(time.Millisecond * 100)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}

		var (
			notReady []string
			readyNS  int
		)

		for _, n := range ns {
			var ready int

			ls, err := c.clientset.AppsV1().DaemonSets(n).List(ctx, metav1.ListOptions{})
			if err != nil {
				return err
			}

			for _, item := range ls.Items {
				if item.Status.DesiredNumberScheduled == item.Status.NumberReady {
					ready++
				} else {
					name := item.Namespace + "/" + item.Name

					if !slices.Contains(notReady, name) {
						notReady = append(notReady, name)
					}
				}
			}

			lsRS, err := c.clientset.AppsV1().ReplicaSets(n).List(ctx, metav1.ListOptions{})
			if err != nil {
				return err
			}

			for _, item := range lsRS.Items {
				if item.Status.Replicas == item.Status.ReadyReplicas {
					ready++
				} else {
					name := item.Namespace + "/" + item.Name

					if !slices.Contains(notReady, name) {
						notReady = append(notReady, name)
					}
				}
			}

			if ready > 0 {
				readyNS++
			}
		}

		slices.Sort(notReady)
		cb(slices.Clone(notReady))

		if len(notReady) == 0 && readyNS == len(ns) {
			return nil
		}
	}
}

func (c *K8sClient) ClientSet() *kubernetes.Clientset {
	return c.clientset
}

func (c *K8sClient) Controller() controllerclient.Client {
	return c.controller
}

func (c *K8sClient) Dyn() *dynamic.DynamicClient {
	return c.dyn
}

func (c *K8sClient) RestClient() *restclient.RESTClient {
	return c.restClient
}

func (c *K8sClient) Mapper() *restmapper.DeferredDiscoveryRESTMapper {
	return c.mapper
}

func (c *K8sClient) ToRESTConfig() (*restclient.Config, error) {
	return c.config, nil
}

func (c *K8sClient) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	return c.cachedDiscovery, nil
}

func (c *K8sClient) ToRESTMapper() (meta.RESTMapper, error) {
	return c.mapper, nil
}

func (c *K8sClient) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return clientcmd.NewNonInteractiveClientConfig(c.rawConfig, c.rawConfig.CurrentContext, &clientcmd.ConfigOverrides{}, nil)
}

func setKubernetesDefaults(config *rest.Config) error {
	config.GroupVersion = &schema.GroupVersion{Group: "", Version: "v1"}

	if config.APIPath == "" {
		config.APIPath = "/api"
	}

	if config.NegotiatedSerializer == nil {
		// This codec factory ensures the resources are not converted. Therefore, resources
		// will not be round-tripped through internal versions. Defaulting does not happen
		// on the client.
		var Codecs = serializer.NewCodecFactory(runtime.NewScheme())

		config.NegotiatedSerializer = Codecs.WithoutConversion()
	}

	return rest.SetKubernetesDefaults(config)
}

func (c *K8sClient) PortForward(namespace string, pod string, port int) (net.Conn, error) {
	url := c.restClient.Post().
		Resource("pods").
		Namespace(namespace).
		Name(pod).
		SubResource("portforward").URL()

	transport, upgrader, err := spdy.RoundTripperFor(c.config)
	if err != nil {
		return nil, err
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, url)

	tunnelingDialer, err := portforward.NewSPDYOverWebsocketDialer(url, c.config)
	if err != nil {
		return nil, err
	}

	// First attempt tunneling (websocket) dialer, then fallback to spdy dialer.
	dialer = portforward.NewFallbackDialer(tunnelingDialer, dialer, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})

	streamConn, protocol, err := dialer.Dial(portforward.PortForwardProtocolV1Name)
	if err != nil {
		return nil, fmt.Errorf("error upgrading connection: %s", err)
	}

	if protocol != portforward.PortForwardProtocolV1Name {
		return nil, fmt.Errorf("unable to negotiate protocol: server returned %q", protocol)
	}

	// create error stream
	headers := http.Header{}
	headers.Set(corev1.StreamType, corev1.StreamTypeError)
	headers.Set(corev1.PortHeader, fmt.Sprintf("%d", port))
	headers.Set(corev1.PortForwardRequestIDHeader, "0")
	errorStream, err := streamConn.CreateStream(headers)
	if err != nil {
		return nil, err
	}

	// we're not writing to this stream
	_ = errorStream.Close()

	// create data stream
	headers.Set(corev1.StreamType, corev1.StreamTypeData)

	dataStream, err := streamConn.CreateStream(headers)
	if err != nil {
		return nil, fmt.Errorf("error creating data stream: %w", err)
	}

	rwConn := rwconn.NewConn(dataStream, dataStream, rwconn.SetCloseHook(func() {
		_ = streamConn.Close()
	}))

	go func() {
		_, _ = io.ReadAll(errorStream)

		_ = rwConn.Close()
		_ = streamConn.Close()
	}()

	return rwConn, nil
}

func ptr[T any](v T) *T {
	return &v
}
