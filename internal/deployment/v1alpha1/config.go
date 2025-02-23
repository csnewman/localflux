// +kubebuilder:object:generate=true
// +groupName=flux.local
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

const (
	// DeploymentKind is the string representation of a Deployment.
	DeploymentKind = "Deployment"
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
	SchemeBuilder.Register(&Deployment{}, &DeploymentList{})
}

// Deployment represents a deployment.
//
// +kubebuilder:object:root=true
type Deployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +optional
	KustomizeNames []string `json:"kustomizeNames,omitempty"`
	// +optional
	HelmNames []string `json:"helmNames,omitempty"`
	// +optional
	PortForward []*PortForward `json:"portForward,omitempty"`
}

// DeploymentList contains a list of Deployment's
//
// +kubebuilder:object:root=true
type DeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Deployment `json:"items"`
}

type PortForward struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Port      int    `json:"port"`
	Network   string `json:"network"`
	// +optional
	LocalPort *int `json:"localPort,omitempty"`
}
