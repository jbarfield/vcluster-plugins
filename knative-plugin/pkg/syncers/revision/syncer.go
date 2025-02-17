package revision

import (
	plaincontext "context"

	"github.com/loft-sh/vcluster-sdk/syncer"
	"github.com/loft-sh/vcluster-sdk/syncer/context"
	"github.com/loft-sh/vcluster-sdk/syncer/mapper"
	"github.com/loft-sh/vcluster-sdk/syncer/translator"
	"github.com/loft-sh/vcluster-sdk/translate"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ksvcv1 "knative.dev/serving/pkg/apis/serving/v1"
)

const (
	REGISTER_CONTEXT = "REGISTER_CONTEXT"
)

func New(ctx *context.RegisterContext) syncer.Syncer {
	return &revisionSyncer{
		NamespacedTranslator: translator.NewNamespacedTranslator(ctx, "revision", &ksvcv1.Revision{}),
		physicalClient:       ctx.PhysicalManager.GetClient(),
		virtualClient:        ctx.VirtualManager.GetClient(),
		physicalNamespace:    ctx.TargetNamespace,

		// nameCache: make(map[types.NamespacedName]types.NamespacedName),
	}
}

type revisionSyncer struct {
	translator.NamespacedTranslator

	physicalClient    client.Client
	virtualClient     client.Client
	physicalNamespace string
}

var _ syncer.Initializer = &revisionSyncer{}
var _ syncer.UpSyncer = &revisionSyncer{}
var _ mapper.Reverse = &revisionSyncer{}

func (r *revisionSyncer) Init(ctx *context.RegisterContext) error {
	// add reverse mappings
	r.AddReverseMapper(ctx,
		&ksvcv1.Configuration{},
		IndexByConfiguration,
		func(rawObj client.Object) []string {
			return filterRevisionFromConfiguration(ctx.TargetNamespace, rawObj)
		},
		func(obj client.Object) []reconcile.Request {
			return mapconfigs(ctx, obj)
		},
	)

	return translate.EnsureCRDFromPhysicalCluster(ctx.Context,
		ctx.PhysicalManager.GetConfig(),
		ctx.VirtualManager.GetConfig(),
		ksvcv1.SchemeGroupVersion.WithKind("Revision"),
	)
}

func (r *revisionSyncer) SyncDown(ctx *context.SyncContext, vObj client.Object) (ctrl.Result, error) {
	ctx.Log.Debugf("SyncDown called for %s:%s", vObj.GetObjectKind().GroupVersionKind().Kind, vObj.GetName())

	ctx.Log.Debugf("Deleting virtual Revision Object %s because physical no longer exists", vObj.GetName())
	err := ctx.VirtualClient.Delete(ctx.Context, vObj)
	if err != nil {
		ctx.Log.Errorf("Error deleting virtual revision object: %v", err)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *revisionSyncer) Sync(ctx *context.SyncContext, pObj, vObj client.Object) (ctrl.Result, error) {
	ctx.Log.Debugf("Sync called for Revision %s : %s", pObj.GetName(), vObj.GetName())

	pRevision := pObj.(*ksvcv1.Revision)
	vRevision := vObj.(*ksvcv1.Revision)

	// since revisions are immutable and are created by config
	// we are never interested in sync down events for revisions
	if !equality.Semantic.DeepEqual(vRevision.Spec, pRevision.Spec) {
		newRevision := vRevision.DeepCopy()
		newRevision.Spec = pRevision.Spec
		ctx.Log.Debugf("Update virtual revision %s:%s, because spec is out of sync", vRevision.Namespace, vRevision.Name)
		err := ctx.VirtualClient.Update(ctx.Context, newRevision)
		if err != nil {
			ctx.Log.Errorf("Error updating virtual kconfig spec for %s:%s, %v", vRevision.Namespace, vRevision.Name, err)
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	if !equality.Semantic.DeepEqual(vRevision.Status, pRevision.Status) {
		newRevision := vRevision.DeepCopy()
		newRevision.Status = pRevision.Status
		ctx.Log.Errorf("Update virtual revision %s:%s, because status is out of sync", vRevision.Namespace, vRevision.Name)
		err := ctx.VirtualClient.Status().Update(ctx.Context, newRevision)
		if err != nil {
			ctx.Log.Errorf("Error updating virtual kconfig status for %s:%s, %v", vRevision.Namespace, vRevision.Name, err)
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *revisionSyncer) SyncUp(ctx *context.SyncContext, pObj client.Object) (ctrl.Result, error) {
	ctx.Log.Debugf("SyncUp called for revision ", pObj.GetName())
	newObj := pObj.DeepCopyObject().(client.Object)

	return r.SyncUpCreate(ctx, newObj)
}

func (r *revisionSyncer) SyncUpCreate(ctx *context.SyncContext, pObj client.Object) (ctrl.Result, error) {
	ctx.Log.Debugf("SyncUpCreate called for %s:%s", pObj.GetName(), pObj.GetNamespace())
	ctx.Log.Debugf("reverse name should be ", r.PhysicalToVirtual(pObj))

	// TODO: find relevant parent of object
	pObj = r.ReverseTranslateMetadata(ctx, pObj, nil)

	err := ctx.VirtualClient.Create(ctx.Context, pObj)
	if err != nil {
		ctx.Log.Errorf("error creating virtual revision object %s/%s, %v", pObj.GetNamespace(), pObj.GetName(), err)
		r.NamespacedTranslator.EventRecorder().Eventf(pObj, "Warning", "SyncError", "Error syncing to virtual cluster: %v", err)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *revisionSyncer) IsManaged(obj client.Object) (bool, error) {
	managed, err := r.NamespacedTranslator.IsManaged(obj)
	if err == nil && managed {
		return managed, err
	}

	// else try to check if this revision belongs to a configuration
	// which is managed by a vcluster

	metaAccessor, err := meta.Accessor(obj)
	if err != nil {
		return false, err
	}

	owners := metaAccessor.GetOwnerReferences()

	for _, owner := range owners {
		parent, err := r.physicalClient.Scheme().New(schema.FromAPIVersionAndKind(owner.APIVersion, owner.Kind))
		if err != nil {
			klog.Errorf("error converting %s/%s to a runtime object %v", owner.Kind, owner.APIVersion)
			continue
		}

		err = r.physicalClient.Get(plaincontext.Background(), client.ObjectKey{
			Name:      owner.Name,
			Namespace: metaAccessor.GetNamespace(),
		}, parent.(client.Object))
		if err != nil {
			klog.Infof("cannot get physical object %s %s/%s: %v",
				parent.GetObjectKind().GroupVersionKind().Kind,
				owner.Name,
				metaAccessor.GetNamespace(),
				err)
			continue
		}

		parentMetaAccessor, err := meta.Accessor(parent)
		if err != nil {
			klog.Infof("error checking parent meta accessor object %s %s/%s: %v",
				parent.GetObjectKind().GroupVersionKind().Kind,
				owner.Name,
				metaAccessor.GetNamespace(),
				err)
			continue
		}

		if v, ok := parentMetaAccessor.GetLabels()[translate.MarkerLabel]; ok {
			if v == translate.Suffix {
				return true, nil
			}
		}
	}

	return false, nil
}
