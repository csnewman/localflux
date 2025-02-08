package v1alpha1

const Version string = "localflux/v1alpha1"

type Config struct {
	APIVersion     string     `yaml:"apiVersion"`
	Kind           string     `yaml:"kind"`
	DefaultCluster string     `yaml:"defaultCluster"`
	Clusters       []*Cluster `yaml:"clusters"`
}

type Cluster struct {
	Name     string    `yaml:"name"`
	Minikube *Minikube `yaml:"minikube"`
}

type Minikube struct {
	Name        string `yaml:"name"`
	PortForward bool   `yaml:"portForward"`
	Registry    string `yaml:"registry"`
}
