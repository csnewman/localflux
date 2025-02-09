package v1alpha1

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
	Address string `json:"address"`
}

type Deployment struct {
	Name   string   `json:"name"`
	Images []*Image `json:"images"`
}

type Image struct {
	Image     string            `json:"image"`
	Context   string            `json:"context"`
	File      string            `json:"file"`
	Target    string            `json:"target"`
	BuildArgs map[string]string `json:"buildArgs"`
}
