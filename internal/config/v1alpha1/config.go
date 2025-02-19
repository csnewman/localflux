package v1alpha1

import (
	"github.com/fluxcd/pkg/apis/kustomize"
)

const Version string = "localflux/v1alpha1"

type Config struct {
	APIVersion     string        `json:"apiVersion"`
	Kind           string        `json:"kind"`
	DefaultCluster string        `json:"defaultCluster"`
	Clusters       []*Cluster    `json:"clusters"`
	Deployments    []*Deployment `json:"deployments"`
}

type Cluster struct {
	Name     string    `json:"name"`
	Minikube *Minikube `json:"minikube"`
	BuildKit *BuildKit `json:"buildkit"`
}

type Minikube struct {
	Profile         string   `json:"profile"`
	PortForward     bool     `json:"portForward"`
	RegistryAliases []string `json:"registryAliases"`
	Addons          []string `json:"addons"`
}

type BuildKit struct {
	Address                string   `json:"address"`
	RegistryAuthTLSContext []string `json:"registryAuthTLSContext"`
	DockerConfig           string   `json:"dockerConfig"`
}

type Deployment struct {
	Name   string   `json:"name"`
	Images []*Image `json:"images"`
	Steps  []*Step  `json:"steps"`
}

type Image struct {
	Image     string            `json:"image"`
	Context   string            `json:"context"`
	File      string            `json:"file"`
	Target    string            `json:"target"`
	BuildArgs map[string]string `json:"buildArgs"`
}

type Step struct {
	Name      string     `json:"name"`
	Kustomize *Kustomize `json:"kustomize"`
	Helm      *Helm      `json:"helm"`
}

type Kustomize struct {
	Context     string            `json:"context"`
	IgnorePaths []string          `json:"ignorePaths"`
	Namespace   string            `json:"namespace"`
	Path        string            `json:"path"`
	Components  []string          `json:"components"`
	Substitute  map[string]string `json:"substitute"`
	Patches     []kustomize.Patch `json:"patches"`
}

type Helm struct {
	Repo        string            `json:"repo"`
	Context     string            `json:"context"`
	IgnorePaths []string          `json:"ignorePaths"`
	Chart       string            `json:"chart"`
	Version     string            `json:"version"`
	Namespace   string            `json:"namespace"`
	Patches     []kustomize.Patch `json:"patches"`
	Values      map[string]any    `json:"values"`
	ValueFiles  []string          `json:"valueFiles"`
}
