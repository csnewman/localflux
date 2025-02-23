package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/csnewman/localflux/internal/deployment/v1alpha1"
	"github.com/fluxcd/pkg/chartutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
	"slices"
	"strings"
	"time"

	"github.com/csnewman/localflux/internal/cluster"
	"github.com/csnewman/localflux/internal/config"
	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"github.com/fluxcd/pkg/apis/kustomize"
	ociclient "github.com/fluxcd/pkg/oci/client"
	sourcev1b2 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	conname "github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	ErrInvalidCluster = errors.New("invalid cluster")
	ErrNotFound       = errors.New("deployment not found")
	ErrInvalid        = errors.New("invalid deployment")
)

type Manager struct {
	logger   *slog.Logger
	cfg      config.Config
	clusters *cluster.Manager
}

func NewManager(logger *slog.Logger, cfg config.Config, clusters *cluster.Manager) *Manager {
	return &Manager{
		logger:   logger,
		cfg:      cfg,
		clusters: clusters,
	}
}

type Callbacks interface {
	Completed(msg string, dur time.Duration)

	State(msg string, detail string, start time.Time)

	Success(detail string)

	Info(msg string)

	Warn(msg string)

	Error(msg string)

	BuildStatus(name string, graph *BuildGraph)
}

func (m *Manager) Deploy(ctx context.Context, clusterName string, name string, cb Callbacks) error {
	if clusterName == "" {
		clusterName = m.cfg.DefaultCluster
	}

	provider, err := m.clusters.Provider(clusterName)
	if err != nil {
		return err
	}

	var deployment config.Deployment

	for _, d := range m.cfg.Deployments {
		if d.Name != name {
			continue
		}

		deployment = d
	}

	if deployment == nil {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}

	m.logger.Info("Deploying", "name", deployment.Name)

	cb.Info(fmt.Sprintf("Deploying %q to %q", deployment.Name, clusterName))

	clusterStatus, err := provider.Status(ctx)
	if err != nil {
		return fmt.Errorf("failed to check cluster status: %w", err)
	}

	if clusterStatus != cluster.StatusActive {
		cb.Error("Cluster is not in an active state")

		return fmt.Errorf("%w: cluster is not in active state", ErrInvalidCluster)
	}

	regTrans, regAuth, err := provider.RegistryConn(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to cluster registry: %w", err)
	}

	replacementImages, err := m.buildImages(ctx, deployment, provider, cb, regTrans, regAuth)
	if err != nil {
		return fmt.Errorf("failed to build images: %w", err)
	}

	kc, err := cluster.NewK8sClientForCtx(provider.KubeConfig(), provider.ContextName())
	if err != nil {
		return fmt.Errorf("failed to create k8s client: %w", err)
	}

	m.logger.Info("Comparing")

	start := time.Now()

	cb.State("Checking deployment", "Checking existing", start)

	var (
		kustomizeNames []string
		helmNames      []string
	)

	for _, step := range deployment.Steps {
		defined := 0

		if step.Kustomize != nil {
			defined++
		}

		if step.Helm != nil {
			defined++
		}

		if defined == 0 {
			return fmt.Errorf("%w: %q has no action defined", ErrInvalid, step.Name)
		}

		if defined > 1 {
			return fmt.Errorf("%w: %q has multiple actions defined", ErrInvalid, step.Name)
		}

		remoteName := fixName(deployment.Name) + "-" + fixName(step.Name)

		if step.Kustomize != nil {
			kustomizeNames = append(kustomizeNames, remoteName)
		}

		if step.Helm != nil {
			helmNames = append(helmNames, remoteName)
		}
	}

	slices.Sort(kustomizeNames)
	slices.Sort(helmNames)

	cb.State("Checking deployment", "Fetching state", start)

	remoteDeploymentName := fixName(deployment.Name)

	var existingDeployment v1alpha1.Deployment

	if err := kc.Controller().Get(ctx, client.ObjectKey{
		Namespace: cluster.LFNamespace,
		Name:      remoteDeploymentName,
	}, &existingDeployment); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get existing deployment: %w", err)
	}

	for _, depName := range existingDeployment.KustomizeNames {
		if slices.Contains(kustomizeNames, depName) {
			continue
		}

		cb.State("Checking deployment", fmt.Sprintf("Cleaning up %q", depName), start)

		if err := kc.Controller().Delete(
			ctx,
			&kustomizev1.Kustomization{
				TypeMeta: metav1.TypeMeta{
					APIVersion: kustomizev1.GroupVersion.String(),
					Kind:       kustomizev1.KustomizationKind,
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      depName,
					Namespace: cluster.LFNamespace,
				},
			},
		); err != nil && !apierrors.IsNotFound(err) {

			return fmt.Errorf("failed to cleanup deployment: %w", err)
		}

		if err := kc.Controller().Delete(
			ctx,
			&sourcev1b2.OCIRepository{
				TypeMeta: metav1.TypeMeta{
					Kind:       sourcev1b2.OCIRepositoryKind,
					APIVersion: sourcev1b2.GroupVersion.String(),
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      depName,
					Namespace: cluster.LFNamespace,
				},
			},
		); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to cleanup deployment: %w", err)
		}

		cb.Success(fmt.Sprintf("Removed %q", depName))
	}

	for _, depName := range existingDeployment.HelmNames {
		if slices.Contains(helmNames, depName) {
			continue
		}

		cb.State("Checking deployment", fmt.Sprintf("Cleaning up %q", depName), start)

		if err := kc.Controller().Delete(
			ctx,
			&helmv2.HelmRelease{
				TypeMeta: metav1.TypeMeta{
					Kind:       helmv2.HelmReleaseKind,
					APIVersion: helmv2.GroupVersion.String(),
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      depName,
					Namespace: cluster.LFNamespace,
				},
			},
		); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to cleanup deployment: %w", err)
		}

		if err := kc.Controller().Delete(
			ctx,
			&sourcev1b2.HelmRepository{
				TypeMeta: metav1.TypeMeta{
					Kind:       sourcev1b2.HelmRepositoryKind,
					APIVersion: sourcev1b2.GroupVersion.String(),
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      depName,
					Namespace: cluster.LFNamespace,
				},
			},
		); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to cleanup deployment: %w", err)
		}

		if err := kc.Controller().Delete(
			ctx,
			&sourcev1b2.OCIRepository{
				TypeMeta: metav1.TypeMeta{
					Kind:       sourcev1b2.OCIRepositoryKind,
					APIVersion: sourcev1b2.GroupVersion.String(),
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      depName,
					Namespace: cluster.LFNamespace,
				},
			},
		); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to cleanup deployment: %w", err)
		}

		cb.Success(fmt.Sprintf("Removed %q", depName))
	}

	cb.State("Checking deployment", "Storing state", start)

	var mappedPorts []*v1alpha1.PortForward

	for _, forward := range deployment.PortForward {
		net := "tcp"
		if forward.Network != "" {
			net = strings.ToLower(forward.Network)
		}

		mappedPorts = append(mappedPorts, &v1alpha1.PortForward{
			Kind:      forward.Kind,
			Namespace: forward.Namespace,
			Name:      forward.Name,
			Network:   net,
			Port:      forward.Port,
			LocalPort: forward.LocalPort,
		})
	}

	if err := kc.PatchSSA(ctx, &v1alpha1.Deployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       v1alpha1.DeploymentKind,
			APIVersion: v1alpha1.GroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      remoteDeploymentName,
			Namespace: cluster.LFNamespace,
		},
		KustomizeNames: kustomizeNames,
		HelmNames:      helmNames,
		PortForward:    mappedPorts,
	}); err != nil {
		return fmt.Errorf("failed to create deployment: %w", err)
	}

	cb.Completed("Checks completed", time.Since(start))

	for _, step := range deployment.Steps {
		if step.Kustomize != nil {
			if err := m.deployKustomize(ctx, deployment, step, cb, regTrans, regAuth, provider, replacementImages, kc); err != nil {
				return fmt.Errorf("step %q failed: %w", step.Name, err)
			}
		}

		if step.Helm != nil {
			if err := m.deployHelm(ctx, deployment, step, cb, regTrans, regAuth, provider, replacementImages, kc); err != nil {
				return fmt.Errorf("step %q failed: %w", step.Name, err)
			}
		}
	}

	cb.State("Done", "", time.Now())

	m.logger.Info("Done")

	return nil
}

func (m *Manager) buildImages(
	ctx context.Context,
	deployment config.Deployment,
	provider cluster.Provider,
	cb Callbacks,
	regTrans http.RoundTripper,
	regAuth authn.Authenticator,
) ([]kustomize.Image, error) {
	replacementImages := make([]kustomize.Image, 0, len(deployment.Images))

	b, err := NewBuilder(ctx, m.logger, provider.BuildKitConfig())
	if err != nil {
		return nil, err
	}

	if len(deployment.Images) > 0 {
		m.logger.Info("Building images")

		for _, image := range deployment.Images {
			start := time.Now()

			m.logger.Info("Building image", "image", image.Image)

			cb.State("Building images", image.Image, start)

			artifact, err := b.Build(ctx, image, "./", func(bg *BuildGraph) {
				cb.BuildStatus(image.Image, bg)
			})
			if err != nil {
				return nil, fmt.Errorf("failed to build image: %w", err)
			}

			cb.BuildStatus(image.Image, nil)

			img, err := crane.Load(artifact.File.Name())
			if err != nil {
				artifact.Delete()

				return nil, fmt.Errorf("failed to load image: %w", err)
			}

			tag, err := conname.NewTag(image.Image, conname.Insecure)
			if err != nil {
				artifact.Delete()

				return nil, fmt.Errorf("failed to create tag: %w", err)
			}

			h, err := img.Digest()
			if err != nil {
				artifact.Delete()

				return nil, fmt.Errorf("failed to get image digest: %w", err)
			}

			m.logger.Info("Pushing image", "digest", h)

			if err := remote.Push(
				tag,
				img,
				remote.WithContext(ctx),
				remote.WithTransport(regTrans),
				remote.WithAuth(regAuth),
			); err != nil {
				artifact.Delete()

				return nil, fmt.Errorf("pushing artifact failed: %w", err)
			}

			artifact.Delete()

			replacementImages = append(replacementImages, kustomize.Image{
				Name:    image.Image,
				NewName: image.Image,
				Digest:  h.String(),
			})

			cb.Completed(fmt.Sprintf("Built image %q", image.Image), time.Since(start))
		}
	}

	return replacementImages, nil
}

var nameRegex = regexp.MustCompile("[^a-zA-Z0-9]")

func fixName(name string) string {
	return nameRegex.ReplaceAllString(name, "-")
}

func (m *Manager) deployKustomize(
	ctx context.Context,
	deployment config.Deployment,
	step config.Step,
	cb Callbacks,
	regTrans http.RoundTripper,
	regAuth authn.Authenticator,
	provider cluster.Provider,
	replacementImages []kustomize.Image,
	kc *cluster.K8sClient,
) error {
	start := time.Now()

	m.logger.Info("Executing step", "step", step.Name)
	m.logger.Info("Pushing manifests")

	cb.State(fmt.Sprintf("Step %q", step.Name), "Packaging manifests", start)

	ociClient := ociclient.NewClient([]crane.Option{crane.WithTransport(regTrans), crane.WithAuth(regAuth), crane.Insecure})

	remoteName := fixName(deployment.Name) + "-" + fixName(step.Name)

	manTag, err := conname.NewTag(provider.Registry() + "/localflux/" + remoteName)
	if err != nil {
		return fmt.Errorf("failed to create tag: %w", err)
	}

	pushedTag, err := ociClient.Push(
		ctx,
		manTag.String(),
		step.Kustomize.Context,
		ociclient.WithPushIgnorePaths(step.Kustomize.IgnorePaths...),
		ociclient.WithPushMetadata(ociclient.Metadata{
			Source:  "localflux",
			Created: time.Unix(0, 0).Format(time.RFC3339),
		}),
	)
	if err != nil {
		return fmt.Errorf("failed to push manifests: %w", err)
	}

	m.logger.Info("Deploying")

	cb.State(fmt.Sprintf("Step %q", step.Name), "Deploying namespace", start)

	parsedDigest, err := conname.NewDigest(pushedTag)
	if err != nil {
		return fmt.Errorf("failed to parse pushed tag: %w", err)
	}

	if err := kc.CreateNamespace(ctx, cluster.LFNamespace); err != nil {
		return fmt.Errorf("failed to create namespace: %w", err)
	}

	if step.Kustomize.Namespace != "" {
		if err := kc.CreateNamespace(ctx, step.Kustomize.Namespace); err != nil {
			return fmt.Errorf("failed to create namespace: %w", err)
		}
	}

	cb.State(fmt.Sprintf("Step %q", step.Name), "Deploying repo", start)

	if err := kc.PatchSSA(ctx, &sourcev1b2.OCIRepository{
		TypeMeta: metav1.TypeMeta{
			Kind:       sourcev1b2.OCIRepositoryKind,
			APIVersion: sourcev1b2.GroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      remoteName,
			Namespace: cluster.LFNamespace,
		},
		Spec: sourcev1b2.OCIRepositorySpec{
			URL: "oci://" + parsedDigest.Repository.String(),
			Reference: &sourcev1b2.OCIRepositoryRef{
				Digest: parsedDigest.DigestStr(),
			},
			Interval: metav1.Duration{
				Duration: time.Minute,
			},
			Insecure: true,
		},
	}); err != nil {
		return fmt.Errorf("failed to create oci repository: %w", err)
	}

	cb.State(fmt.Sprintf("Step %q", step.Name), "Deploying kustomize", start)

	if err := kc.PatchSSA(ctx, &kustomizev1.Kustomization{
		TypeMeta: metav1.TypeMeta{
			APIVersion: kustomizev1.GroupVersion.String(),
			Kind:       kustomizev1.KustomizationKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      remoteName,
			Namespace: cluster.LFNamespace,
		},
		Spec: kustomizev1.KustomizationSpec{
			Interval: metav1.Duration{
				Duration: time.Minute,
			},
			Path: step.Kustomize.Path,
			PostBuild: &kustomizev1.PostBuild{
				Substitute: step.Kustomize.Substitute,
			},
			Prune:   true,
			Patches: step.Kustomize.Patches,
			Images:  replacementImages,
			SourceRef: kustomizev1.CrossNamespaceSourceReference{
				APIVersion: sourcev1b2.GroupVersion.String(),
				Namespace:  cluster.LFNamespace,
				Kind:       sourcev1b2.OCIRepositoryKind,
				Name:       remoteName,
			},
			TargetNamespace: step.Kustomize.Namespace,
			Force:           true,
			Components:      step.Kustomize.Components,
		},
	}); err != nil {
		return fmt.Errorf("failed to create kustomization: %w", err)
	}

	cb.Completed(fmt.Sprintf("Deployed step %q", step.Name), time.Since(start))

	return nil
}

func (m *Manager) deployHelm(ctx context.Context, deployment config.Deployment, step config.Step, cb Callbacks, regTrans http.RoundTripper, regAuth authn.Authenticator, provider cluster.Provider, replacementImages []kustomize.Image, kc *cluster.K8sClient) error {
	start := time.Now()

	m.logger.Info("Executing step", "step", step.Name)

	cb.State(fmt.Sprintf("Step %q", step.Name), "Reading values", start)

	values := make(map[string]any)

	for _, file := range step.Helm.ValueFiles {
		data, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("failed to read file %q: %w", file, err)
		}

		rawJSON, err := yaml.YAMLToJSON(data)
		if err != nil {
			return fmt.Errorf("failed to read file %q: %w", file, err)
		}

		var extraValues map[string]any

		if err := json.Unmarshal(rawJSON, &extraValues); err != nil {
			return fmt.Errorf("failed to read file %q: %w", file, err)
		}

		values = chartutil.MergeMaps(values, extraValues)
	}

	if step.Helm.Values != nil {
		var extraValues map[string]any

		if err := json.Unmarshal(step.Helm.Values.Raw, &extraValues); err != nil {
			return fmt.Errorf("failed to parse values: %w", err)
		}

		values = chartutil.MergeMaps(values, extraValues)
	}

	encodedValues, err := json.Marshal(values)
	if err != nil {
		return fmt.Errorf("failed to marshal values: %w", err)
	}

	ociClient := ociclient.NewClient([]crane.Option{crane.WithTransport(regTrans), crane.WithAuth(regAuth), crane.Insecure})

	remoteName := fixName(deployment.Name) + "-" + fixName(step.Name)

	if step.Helm.Repo != "" && step.Helm.Context != "" {
		return fmt.Errorf("%w: helm repo and context are mutually exclusive", ErrInvalid)
	}

	var (
		chart    *helmv2.HelmChartTemplate
		chartRef *helmv2.CrossNamespaceSourceReference
	)

	if step.Helm.Repo != "" {
		cb.State(fmt.Sprintf("Step %q", step.Name), "Deploying repo", start)

		repoType := ""

		if strings.HasPrefix(strings.ToLower(step.Helm.Repo), "oci://") {
			repoType = "oci"
		}

		if err := kc.PatchSSA(ctx, &sourcev1b2.HelmRepository{
			TypeMeta: metav1.TypeMeta{
				Kind:       sourcev1b2.HelmRepositoryKind,
				APIVersion: sourcev1b2.GroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      remoteName,
				Namespace: cluster.LFNamespace,
			},
			Spec: sourcev1b2.HelmRepositorySpec{
				URL:  step.Helm.Repo,
				Type: repoType,
				//SecretRef:       nil,
				//CertSecretRef:   nil,
				//PassCredentials: false,
				Interval: metav1.Duration{
					Duration: time.Minute * 5,
				},
			},
		}); err != nil {
			return fmt.Errorf("failed to create oci repository: %w", err)
		}

		chart = &helmv2.HelmChartTemplate{
			Spec: helmv2.HelmChartTemplateSpec{
				Chart:   step.Helm.Chart,
				Version: step.Helm.Version,
				SourceRef: helmv2.CrossNamespaceObjectReference{
					Namespace:  cluster.LFNamespace,
					APIVersion: sourcev1b2.GroupVersion.String(),
					Kind:       sourcev1b2.HelmRepositoryKind,
					Name:       remoteName,
				},
			},
		}
	} else {
		m.logger.Info("Pushing chart")

		cb.State(fmt.Sprintf("Step %q", step.Name), "Packaging chart", start)

		manTag, err := conname.NewTag(provider.Registry() + "/localflux/" + remoteName)
		if err != nil {
			return fmt.Errorf("failed to create tag: %w", err)
		}

		pushedTag, err := ociClient.Push(
			ctx,
			manTag.String(),
			step.Helm.Context,
			ociclient.WithPushIgnorePaths(step.Helm.IgnorePaths...),
			ociclient.WithPushMetadata(ociclient.Metadata{
				Source:  "localflux",
				Created: time.Unix(0, 0).Format(time.RFC3339),
			}),
		)
		if err != nil {
			return fmt.Errorf("failed to push manifests: %w", err)
		}

		parsedDigest, err := conname.NewDigest(pushedTag)
		if err != nil {
			return fmt.Errorf("failed to parse pushed tag: %w", err)
		}

		cb.State(fmt.Sprintf("Step %q", step.Name), "Deploying repo", start)

		if err := kc.PatchSSA(ctx, &sourcev1b2.OCIRepository{
			TypeMeta: metav1.TypeMeta{
				Kind:       sourcev1b2.OCIRepositoryKind,
				APIVersion: sourcev1b2.GroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      remoteName,
				Namespace: cluster.LFNamespace,
			},
			Spec: sourcev1b2.OCIRepositorySpec{
				URL: "oci://" + parsedDigest.Repository.String(),
				Reference: &sourcev1b2.OCIRepositoryRef{
					Digest: parsedDigest.DigestStr(),
				},
				Interval: metav1.Duration{
					Duration: time.Minute,
				},
				Insecure: true,
			},
		}); err != nil {
			return fmt.Errorf("failed to create oci repository: %w", err)
		}

		chartRef = &helmv2.CrossNamespaceSourceReference{
			APIVersion: sourcev1b2.GroupVersion.String(),
			Namespace:  cluster.LFNamespace,
			Kind:       sourcev1b2.OCIRepositoryKind,
			Name:       remoteName,
		}
	}

	cb.State(fmt.Sprintf("Step %q", step.Name), "Deploying namespace", start)

	if err := kc.CreateNamespace(ctx, cluster.LFNamespace); err != nil {
		return fmt.Errorf("failed to create namespace: %w", err)
	}

	if step.Helm.Namespace != "" {
		if err := kc.CreateNamespace(ctx, step.Helm.Namespace); err != nil {
			return fmt.Errorf("failed to create namespace: %w", err)
		}
	}

	cb.State(fmt.Sprintf("Step %q", step.Name), "Deploying chart", start)

	if err := kc.PatchSSA(ctx, &helmv2.HelmRelease{
		TypeMeta: metav1.TypeMeta{
			Kind:       helmv2.HelmReleaseKind,
			APIVersion: helmv2.GroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      remoteName,
			Namespace: cluster.LFNamespace,
		},
		Spec: helmv2.HelmReleaseSpec{
			Chart:    chart,
			ChartRef: chartRef,
			Interval: metav1.Duration{
				Duration: time.Minute,
			},
			ReleaseName:     step.Name,
			TargetNamespace: step.Helm.Namespace,
			Timeout:         nil,
			Install: &helmv2.Install{
				Replace: true,
			},
			Upgrade: &helmv2.Upgrade{
				Force: true,
			},
			Rollback: &helmv2.Rollback{
				Force: true,
			},
			Values: &apiextensionsv1.JSON{Raw: encodedValues},
			PostRenderers: []helmv2.PostRenderer{
				{
					Kustomize: &helmv2.Kustomize{
						Patches: step.Helm.Patches,
						Images:  replacementImages,
					},
				},
			},
		},
	}); err != nil {
		return fmt.Errorf("failed to create kustomization: %w", err)
	}

	cb.Completed(fmt.Sprintf("Deployed step %q", step.Name), time.Since(start))

	return nil
}
