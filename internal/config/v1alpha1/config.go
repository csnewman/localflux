package v1alpha1

const Version string = "localflux/v1alpha1"

type Config struct {
	APIVersion     string     `json:"apiVersion"`
	Kind           string     `json:"kind"`
	DefaultCluster string     `json:"defaultCluster"`
	Clusters       []*Cluster `json:"clusters"`
}

type Cluster struct {
	Name     string    `json:"name"`
	Minikube *Minikube `json:"minikube"`
}

type Minikube struct {
	Profile     string   `json:"profile"`
	PortForward bool     `json:"portForward"`
	Registry    string   `json:"registry"`
	Addons      []string `json:"addons"`
}
