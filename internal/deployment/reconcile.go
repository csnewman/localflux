package deployment

import (
	"context"
	"fmt"
	"time"

	"github.com/csnewman/localflux/internal/cluster"
	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/patch"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kstatus "sigs.k8s.io/cli-utils/pkg/kstatus/status"
)

type Reconcilable interface {
	client.Object
	meta.ObjectWithConditions
	meta.StatusWithHandledReconcileRequest

	AsObject() client.Object
}

type ReconcileKustomization struct {
	kustomizev1.Kustomization
}

func (r *ReconcileKustomization) AsObject() client.Object {
	return &r.Kustomization
}

func (r *ReconcileKustomization) GetLastHandledReconcileRequest() string {
	return r.Status.GetLastHandledReconcileRequest()
}

type ReconcileHelm struct {
	helmv2.HelmRelease
}

func (r *ReconcileHelm) AsObject() client.Object {
	return &r.HelmRelease
}

func (r *ReconcileHelm) GetLastHandledReconcileRequest() string {
	return r.Status.GetLastHandledReconcileRequest()
}

func Reconcile[T Reconcilable](
	ctx context.Context,
	kc *cluster.K8sClient,
	ns string,
	name string,
	tgt string,
	limit time.Duration,
	obj T,
	cb func(string),
) error {
	namespacedName := types.NamespacedName{
		Namespace: ns,
		Name:      name,
	}

	controller := kc.Controller()
	first := true
	timeout := time.After(limit)

	for {
		if !first {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Millisecond * 100):

			case <-timeout:
				return fmt.Errorf("timed out waiting for reconciliation")
			}
		}

		first = false

		if err := controller.Get(ctx, namespacedName, obj.AsObject()); err != nil {
			return err
		}

		readyCond := apimeta.FindStatusCondition(obj.GetConditions(), meta.ReadyCondition)
		if readyCond == nil {
			return fmt.Errorf("status can't be determined")
		}

		if obj.GetLastHandledReconcileRequest() != tgt {
			cb("Awaiting attempt")

			continue
		}

		cb(fmt.Sprintf("%s: %s", readyCond.Reason, readyCond.Message))

		result, err := kstatusCompute(obj.AsObject())
		if err != nil {
			return fmt.Errorf("failed to compute status: %w", err)
		}

		if result.Status == kstatus.CurrentStatus {
			break
		}
	}

	return nil
}

// kstatusCompute returns the kstatus computed result of a given object.
func kstatusCompute(obj client.Object) (result *kstatus.Result, err error) {
	u, err := patch.ToUnstructured(obj)
	if err != nil {
		return result, err
	}

	return kstatus.Compute(u)
}

//func requestReconciliation(ctx context.Context, kubeClient client.Client,
//	namespacedName types.NamespacedName, gvk schema.GroupVersionKind) error {
//	return retry.RetryOnConflict(retry.DefaultBackoff, func() (err error) {
//		object := &metav1.PartialObjectMetadata{}
//		object.SetGroupVersionKind(gvk)
//		object.SetName(namespacedName.Name)
//		object.SetNamespace(namespacedName.Namespace)
//		if err := kubeClient.Get(ctx, namespacedName, object); err != nil {
//			return err
//		}
//
//		patch := client.MergeFrom(object.DeepCopy())
//
//		annotations := object.GetAnnotations()
//
//		if annotations == nil {
//			annotations = make(map[string]string, 1)
//		}
//
//		annotations[meta.ReconcileRequestAnnotation] = uuid.New().String()
//
//		// HelmRelease specific annotations to force or reset a release.
//		//if gvk.Kind == helmv2.HelmReleaseKind {
//		//	if rhrArgs.syncForce {
//		//		annotations[helmv2.ForceRequestAnnotation] = ts
//		//	}
//		//	if rhrArgs.syncReset {
//		//		annotations[helmv2.ResetRequestAnnotation] = ts
//		//	}
//		//}
//
//		object.SetAnnotations(annotations)
//		return kubeClient.Patch(ctx, object, patch)
//	})
//}
