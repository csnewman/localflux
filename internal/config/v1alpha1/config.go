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

	// Deployments contains the list of possible deployments.
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

// Cluster represents a kubernetes cluster. At present only Minikube is supported.
type Cluster struct {
	// Name is the cluster name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// Minikube provides configuration for automatically starting a Minikube cluster.
	// +optional
	Minikube *Minikube `json:"minikube"`
	// BuildKit controls how images are built.
	// +optional
	BuildKit *BuildKit `json:"buildkit"`
	// +optional
	KubeConfig string `json:"kubeConfig"`
	// Relay provides port-forwarding capabilities.
	// +optional
	Relay *Relay `json:"relay"`
}

// Minikube configures a local minikube cluster.
type Minikube struct {
	// Profile maps to "minikube --profile"
	// +optional
	Profile string `json:"profile"`
	// RegistryAliases is a list of hostnames to alias to the internal cluster registry.
	// +optional
	RegistryAliases []string `json:"registryAliases"`
	// Addons is a list of minikube addons to enable.
	// +optional
	Addons []string `json:"addons"`
	// CNI enables the provided CNI plugin. Necessary for netpols.
	// +optional
	CNI string `json:"cni"`
	// CustomArgs are raw arguments to pass to the minikube start command.
	// +optional
	CustomArgs []string `json:"customArgs"`
}

// BuildKit configures image building.
type BuildKit struct {
	// The buildkit builder address.
	// +optional
	Address string `json:"address"`
	// +optional
	RegistryAuthTLSContext []string `json:"registryAuthTLSContext"`
	// +optional
	DockerConfig string `json:"dockerConfig"`
}

// Relay configures port-forwarding.
type Relay struct {
	// Enabled causes the port forwarding in-cluster components to be deployed, alongside a docker container on the
	// host to handle relaying.
	Enabled bool `json:"enabled"`
	// DisableClient prevents the host-side docker container being created. Use "localflux relay" instead.
	// +optional
	DisableClient bool `json:"disableClient"`
	// ClusterNetworking controls whether to use host or cluster networking for the cluster side relay server.
	// +optional
	ClusterNetworking bool `json:"clusterNetworking"`
}

// Deployment is a single deployment with multiple steps.
type Deployment struct {
	// Name is the deployment name. Used to specify this deployment from the command line. Localflux relies on this
	// being a stable identifier.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// Images is a list of images to build.
	// +optional
	Images []*Image `json:"images"`
	// Steps are a list of actions to perform in order.
	// +optional
	Steps []*Step `json:"steps"`
	// PortForward is a list of ports to forward to the cluster.
	// +optional
	PortForward []*PortForward `json:"portForward"`
}

// Image represents a single image to build.
type Image struct {
	// Image is the fully qualified name for the image.
	Image string `json:"image"`
	// Context is the docker build context directory.
	// +optional
	Context string `json:"context"`
	// +optional
	IncludePaths []string `json:"includePaths"`
	// +optional
	ExcludePaths []string `json:"excludePaths"`
	// File is the Dockerfile to use inside the context.
	// +optional
	File string `json:"file"`
	// Target is the target inside the Dockerfile to build.
	// +optional
	Target string `json:"target"`
	// +optional
	BuildArgs map[string]string `json:"buildArgs"`
}

// Step is a single action inside a deployment. Either kustomize or helm may be specified.
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

// Kustomize is a kustomize based action.
type Kustomize struct {
	Context string `json:"context"`
	// +optional
	IncludePaths []string `json:"includePaths"`
	// +optional
	ExcludePaths []string `json:"excludePaths"`
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

// Helm is a helm based action.
type Helm struct {
	// +optional
	Repo string `json:"repo"`
	// +optional
	Context string `json:"context"`
	// +optional
	IncludePaths []string `json:"includePaths"`
	// +optional
	ExcludePaths []string `json:"excludePaths"`
	Chart        string   `json:"chart"`
	Version      string   `json:"version"`
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
