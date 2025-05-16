// +kubebuilder:object:generate=true
// +groupName=flux.local
package v1alpha1

import (
	"github.com/fluxcd/pkg/apis/kustomize"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "flux.local", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(&Config{}, &ConfigList{})
}

// Config represents the project config.
//
// +kubebuilder:object:root=true
type Config struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// DefaultCluster is the name of the cluster to use if one is not specified.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	DefaultCluster string `json:"defaultCluster"`

	// Clusters is the list of clusters to connect to.
	// +kubebuilder:validation:MinItems=1
	Clusters []*Cluster `json:"clusters"`

	// +optional
	Deployments []*Deployment `json:"deployments"`
}

// ConfigList contains a list of Config
//
// +kubebuilder:object:root=true
type ConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Config `json:"items"`
}

type Cluster struct {
	// Name is the cluster name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// +optional
	Minikube *Minikube `json:"minikube"`
	// +optional
	BuildKit *BuildKit `json:"buildkit"`
	// +optional
	KubeConfig string `json:"kubeConfig"`
	// +optional
	Relay *Relay `json:"relay"`
}

type Minikube struct {
	// +optional
	Profile string `json:"profile"`
	// +optional
	RegistryAliases []string `json:"registryAliases"`
	// +optional
	Addons []string `json:"addons"`
	// +optional
	CNI string `json:"cni"`
	// +optional
	CustomArgs []string `json:"customArgs"`
}

type BuildKit struct {
	// +optional
	Address string `json:"address"`
	// +optional
	RegistryAuthTLSContext []string `json:"registryAuthTLSContext"`
	// +optional
	DockerConfig string `json:"dockerConfig"`
}

type Relay struct {
	Enabled bool `json:"enabled"`
	// +optional
	DisableClient bool `json:"disableClient"`
	// +optional
	ClusterNetworking bool `json:"clusterNetworking"`
}

type Deployment struct {
	// Name is the deployment name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// +optional
	Images []*Image `json:"images"`
	// +optional
	Steps []*Step `json:"steps"`
	// +optional
	PortForward []*PortForward `json:"portForward"`
}

type Image struct {
	Image string `json:"image"`
	// +optional
	Context string `json:"context"`
	// +optional
	File string `json:"file"`
	// +optional
	Target string `json:"target"`
	// +optional
	BuildArgs map[string]string `json:"buildArgs"`
}

type Step struct {
	// Name is the step name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// +optional
	Kustomize *Kustomize `json:"kustomize"`
	// +optional
	Helm *Helm `json:"helm"`
}

type Kustomize struct {
	Context string `json:"context"`
	// +optional
	IgnorePaths []string `json:"ignorePaths"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Namespace string `json:"namespace"`
	// +optional
	Wait *bool `json:"wait"`
	// +optional
	Path string `json:"path"`
	// +optional
	Components []string `json:"components"`
	// +optional
	Substitute map[string]string `json:"substitute"`
	// +optional
	Patches []kustomize.Patch `json:"patches"`
}

type Helm struct {
	// +optional
	Repo string `json:"repo"`
	// +optional
	Context string `json:"context"`
	// +optional
	IgnorePaths []string `json:"ignorePaths"`
	Chart       string   `json:"chart"`
	Version     string   `json:"version"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Namespace string `json:"namespace"`
	// +optional
	Wait *bool `json:"wait"`
	// +optional
	Patches []kustomize.Patch `json:"patches"`
	// +optional
	Values *apiextensionsv1.JSON `json:"values"`
	// +optional
	ValueFiles []string `json:"valueFiles"`
}

type PortForward struct {
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// +optional
	Network string `json:"network"`
	Port    int    `json:"port"`
	// +optional
	LocalPort *int `json:"localPort"`
}
