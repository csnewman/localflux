# localflux
Fast local k8s development

## Demo

Install localflux:
```bash
go install github.com/csnewman/localflux/cmd/localflux
```

Clone demo:
```bash
git clone https://github.com/csnewman/localflux.git
cd localflux/demo
```

Start minikube cluster:
```bash
localflux cluster start
```

Deploy demo (named `simple`)
```bash
localflux deploy simple
```
