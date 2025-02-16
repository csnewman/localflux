package cluster

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sync/errgroup"
	"io"
	"path/filepath"
	"slices"
	"strings"

	sourcev1b2 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/go-logr/logr"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apimachinery/pkg/watch"
	memory "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientsetscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	controllerclient "sigs.k8s.io/controller-runtime/pkg/client"
	controllerlog "sigs.k8s.io/controller-runtime/pkg/log"
)

const LFNamespace = "localflux"

var decUnstructured = yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)

func DefaultKubeConfigPath() string {
	if home := homedir.HomeDir(); home != "" {
		return filepath.Join(home, ".kube", "config")
	}

	return ""
}

func init() {
	controllerlog.SetLogger(logr.Discard())
}

type K8sClient struct {
	clientset  *kubernetes.Clientset
	dyn        *dynamic.DynamicClient
	mapper     *restmapper.DeferredDiscoveryRESTMapper
	controller controllerclient.Client
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

	if err := sourcev1b2.AddToScheme(clientsetscheme.Scheme); err != nil {
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

	//clientset.CoreV1().Pods(.)

	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create: %w", err)
	}

	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(clientset.Discovery()))

	return &K8sClient{
		clientset:  clientset,
		dyn:        dyn,
		mapper:     mapper,
		controller: controller,
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
	egroup, ctx := errgroup.WithContext(ctx)

	type cbd struct {
		ns   string
		name string
		evt  int
	}

	events := make(chan cbd)

	for _, n := range ns {
		egroup.Go(func() error {
			err := c.waitNamespaceReadyDS(ctx, n, func(name string, state bool) {
				evt := 0
				if state {
					evt = 1
				}

				events <- cbd{
					ns:   n,
					name: name,
					evt:  evt,
				}
			})

			events <- cbd{
				ns:  n,
				evt: -1,
			}

			return err
		})

		egroup.Go(func() error {
			err := c.waitNamespaceReadyRS(ctx, n, func(name string, state bool) {
				evt := 0
				if state {
					evt = 1
				}

				events <- cbd{
					ns:   n,
					name: name,
					evt:  evt,
				}
			})

			events <- cbd{
				ns:  n,
				evt: -1,
			}

			return err
		})
	}

	egroup.Go(func() error {
		done := 0

		var notReady []string

		for {
			select {
			case evt := <-events:
				if evt.evt == -1 {
					done++
				} else if evt.evt == 0 {
					name := evt.ns + "/" + evt.name

					if !slices.Contains(notReady, name) {
						notReady = append(notReady, name)
						slices.Sort(notReady)
					}
				} else if evt.evt == 1 {
					var newReady []string

					for _, s := range notReady {
						if s == evt.ns+"/"+evt.name {
							continue
						}

						newReady = append(newReady, s)
					}

					notReady = newReady
				}

				if done == len(ns)*2 {
					return nil
				}

				cb(slices.Clone(notReady))

			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})

	return egroup.Wait()
}

func (c *K8sClient) waitNamespaceReadyDS(ctx context.Context, name string, cb func(name string, state bool)) error {
	wi, err := c.clientset.AppsV1().DaemonSets(name).Watch(ctx, metav1.ListOptions{
		Watch:                true,
		AllowWatchBookmarks:  true,
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
		TimeoutSeconds:       nil,
		SendInitialEvents:    ptr(true),
	})
	if err != nil {
		return err
	}

	defer wi.Stop()

	statuses := make(map[string]bool)
	hadBookmark := false

	rc := wi.ResultChan()
	for {
		select {
		case v, ok := <-rc:
			if !ok {
				return errors.New("watch channel closed")
			}

			switch v.Type {
			case watch.Added, watch.Modified:
				ds, ok := v.Object.(*v1.DaemonSet)
				if !ok {
					continue
				}

				statuses[ds.Name] = ds.Status.DesiredNumberScheduled == ds.Status.NumberReady
				cb(ds.Name, statuses[ds.Name])

			case watch.Deleted:
				ds, ok := v.Object.(*v1.DaemonSet)
				if !ok {
					continue
				}

				delete(statuses, ds.Name)
				cb(ds.Name, true)

			case watch.Error:
				return fmt.Errorf("watch err: %v", v.Object)

			case watch.Bookmark:
				hadBookmark = true
			}

			if hadBookmark {
				notReady := false

				for _, b := range statuses {
					if !b {
						notReady = true
						break
					}
				}

				if !notReady {
					return nil
				}
			}
		case <-ctx.Done():
		}
	}
}

func (c *K8sClient) waitNamespaceReadyRS(ctx context.Context, name string, cb func(name string, state bool)) error {
	wi, err := c.clientset.AppsV1().ReplicaSets(name).Watch(ctx, metav1.ListOptions{
		Watch:                true,
		AllowWatchBookmarks:  true,
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
		TimeoutSeconds:       nil,
		SendInitialEvents:    ptr(true),
	})
	if err != nil {
		return err
	}

	defer wi.Stop()

	statuses := make(map[string]bool)
	hadBookmark := false

	rc := wi.ResultChan()
	for {
		select {
		case v, ok := <-rc:
			if !ok {
				return errors.New("watch channel closed")
			}

			switch v.Type {
			case watch.Added, watch.Modified:
				ds, ok := v.Object.(*v1.ReplicaSet)
				if !ok {
					continue
				}

				statuses[ds.Name] = ds.Status.Replicas == ds.Status.ReadyReplicas
				cb(ds.Name, statuses[ds.Name])

			case watch.Deleted:
				ds, ok := v.Object.(*v1.ReplicaSet)
				if !ok {
					continue
				}

				delete(statuses, ds.Name)

			case watch.Error:
				return fmt.Errorf("watch err: %v", v.Object)

			case watch.Bookmark:
				hadBookmark = true
			}

			if hadBookmark {
				notReady := false

				for _, b := range statuses {
					if !b {
						notReady = true
						break
					}
				}

				if !notReady {
					return nil
				}
			}
		case <-ctx.Done():
		}
	}
}

func ptr[T any](v T) *T {
	return &v
}
