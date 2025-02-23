package crds

import _ "embed"

var All = Configs + Deployments

//go:embed flux.local_configs.yaml
var Configs string

//go:embed flux.local_deployments.yaml
var Deployments string
