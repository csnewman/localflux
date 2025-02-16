package deployment

import (
	"context"
	"errors"
	"fmt"
	"github.com/fluxcd/pkg/apis/kustomize"
	"log/slog"
	"time"

	"github.com/csnewman/localflux/internal/cluster"
	"github.com/csnewman/localflux/internal/config"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	ociclient "github.com/fluxcd/pkg/oci/client"
	sourcev1b2 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/google/go-containerregistry/pkg/crane"
	conname "github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	ErrNotFound = errors.New("deployment not found")
	ErrInvalid  = errors.New("invalid deployment")
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

	b, err := NewBuilder(ctx, m.logger, provider.BuildKitConfig())
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

	regTrans, regAuth, err := provider.RegistryConn(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to cluster registry: %w", err)
	}

	replacementImages := make([]kustomize.Image, 0, len(deployment.Images))

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
				return fmt.Errorf("failed to build image: %w", err)
			}

			cb.BuildStatus(image.Image, nil)

			img, err := crane.Load(artifact.File.Name())
			if err != nil {
				artifact.Delete()

				return fmt.Errorf("failed to load image: %w", err)
			}

			tag, err := conname.NewTag(image.Image, conname.Insecure)
			if err != nil {
				artifact.Delete()

				return fmt.Errorf("failed to create tag: %w", err)
			}

			h, err := img.Digest()
			if err != nil {
				artifact.Delete()

				return fmt.Errorf("failed to get image digest: %w", err)
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

				return fmt.Errorf("pushing artifact failed: %w", err)
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

	m.logger.Info("Pushing manifests")

	start := time.Now()

	cb.State("Building manifests", "", start)

	if deployment.Kustomize == nil {
		return fmt.Errorf("%w: no kustomize configuration provided", ErrInvalid)
	}

	ociClient := ociclient.NewClient([]crane.Option{crane.WithTransport(regTrans), crane.WithAuth(regAuth), crane.Insecure})

	manTag, err := conname.NewTag(provider.Registry() + "/localflux/" + deployment.Name)
	if err != nil {
		return fmt.Errorf("failed to create tag: %w", err)
	}

	pushedTag, err := ociClient.Push(
		ctx,
		manTag.String(),
		deployment.Kustomize.Context,
		ociclient.WithPushIgnorePaths(deployment.Kustomize.IgnorePaths...),
		ociclient.WithPushMetadata(ociclient.Metadata{
			Source:  "localflux",
			Created: time.Unix(0, 0).Format(time.RFC3339),
		}),
	)
	if err != nil {
		return fmt.Errorf("failed to push manifests: %w", err)
	}

	cb.Completed("Built manifests", time.Since(start))

	start = time.Now()

	m.logger.Info("Deploying")

	cb.State("Deploying", "namespace", start)

	kc, err := cluster.NewK8sClientForCtx(cluster.DefaultKubeConfigPath(), provider.ContextName())
	if err != nil {
		return fmt.Errorf("failed to create k8s client: %w", err)
	}

	parsedDigest, err := conname.NewDigest(pushedTag)
	if err != nil {
		return fmt.Errorf("failed to parse pushed tag: %w", err)
	}

	if err := kc.CreateNamespace(ctx, cluster.LFNamespace); err != nil {
		return fmt.Errorf("failed to create namespace: %w", err)
	}

	if deployment.Kustomize.Namespace != "" {
		if err := kc.CreateNamespace(ctx, deployment.Kustomize.Namespace); err != nil {
			return fmt.Errorf("failed to create namespace: %w", err)
		}
	}

	cb.State("Deploying", "oci", start)

	if err := kc.PatchSSA(ctx, &sourcev1b2.OCIRepository{
		TypeMeta: metav1.TypeMeta{
			Kind:       sourcev1b2.OCIRepositoryKind,
			APIVersion: sourcev1b2.GroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployment.Name,
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

	cb.State("Deploying", "kustomize", start)

	if err := kc.PatchSSA(ctx, &kustomizev1.Kustomization{
		TypeMeta: metav1.TypeMeta{
			APIVersion: kustomizev1.GroupVersion.String(),
			Kind:       kustomizev1.KustomizationKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployment.Name,
			Namespace: cluster.LFNamespace,
		},
		Spec: kustomizev1.KustomizationSpec{
			Interval: metav1.Duration{
				Duration: time.Minute,
			},
			Path: deployment.Kustomize.Path,
			PostBuild: &kustomizev1.PostBuild{
				Substitute: deployment.Kustomize.Substitute,
			},
			Prune:   true,
			Patches: deployment.Kustomize.Patches,
			Images:  replacementImages,
			SourceRef: kustomizev1.CrossNamespaceSourceReference{
				Namespace: cluster.LFNamespace,
				Kind:      sourcev1b2.OCIRepositoryKind,
				Name:      deployment.Name,
			},
			TargetNamespace: deployment.Kustomize.Namespace,
			Force:           true,
			Components:      deployment.Kustomize.Components,
		},
	}); err != nil {
		return fmt.Errorf("failed to create kustomization: %w", err)
	}

	cb.Completed("Deployed manifests", time.Since(start))
	cb.State("Done", "", time.Now())

	m.logger.Info("Done")

	return nil
}
