package framework

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/giantswarm/microerror"
	"github.com/giantswarm/micrologger"
	"github.com/giantswarm/micrologger/loggermeta"
	"github.com/prometheus/client_golang/prometheus"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/giantswarm/operatorkit/client/k8scrdclient"
	"github.com/giantswarm/operatorkit/framework/context/reconciliationcanceledcontext"
	"github.com/giantswarm/operatorkit/framework/context/resourcecanceledcontext"
	"github.com/giantswarm/operatorkit/informer"
)

// Config represents the configuration used to create a new operator framework.
type Config struct {
	CRD       *apiextensionsv1beta1.CustomResourceDefinition
	CRDClient *k8scrdclient.CRDClient
	Informer  informer.Interface
	Logger    micrologger.Logger
	// ResourceRouter determines which resource set to use on reconciliation based
	// on its own implementation. A resource router is to decide which resource
	// set to execute. A resource set provides a specific function to initialize
	// the request context and a list of resources to be executed for a
	// reconciliation loop. That way each runtime object being reconciled is
	// executed against a desired list of resources. Since runtime objects may
	// differ in version and/or structure the resource router enables custom
	// inspection before each reconciliation loop. That way the complete list of
	// resources being executed for the received runtime object can be versioned
	// and different resources can be executed depending on the runtime object
	// being reconciled.
	ResourceRouter *ResourceRouter

	BackOffFactory func() backoff.BackOff
}

type Framework struct {
	crd            *apiextensionsv1beta1.CustomResourceDefinition
	crdClient      *k8scrdclient.CRDClient
	informer       informer.Interface
	logger         micrologger.Logger
	resourceRouter *ResourceRouter

	bootOnce sync.Once
	mutex    sync.Mutex

	backOffFactory func() backoff.BackOff
}

// New creates a new configured operator framework.
func New(config Config) (*Framework, error) {
	if config.CRD != nil && config.CRDClient == nil || config.CRD == nil && config.CRDClient != nil {
		return nil, microerror.Maskf(invalidConfigError, "config.CRD and config.CRDClient must not be empty when either given")
	}
	if config.Informer == nil {
		return nil, microerror.Maskf(invalidConfigError, "config.Informer must not be empty")
	}
	if config.Logger == nil {
		return nil, microerror.Maskf(invalidConfigError, "config.Logger must not be empty")
	}
	if config.ResourceRouter == nil {
		return nil, microerror.Maskf(invalidConfigError, "config.ResourceRouter must not be empty")
	}

	if config.BackOffFactory == nil {
		config.BackOffFactory = DefaultBackOffFactory()
	}

	f := &Framework{
		crd:            config.CRD,
		crdClient:      config.CRDClient,
		informer:       config.Informer,
		logger:         config.Logger,
		resourceRouter: config.ResourceRouter,

		bootOnce: sync.Once{},
		mutex:    sync.Mutex{},

		backOffFactory: config.BackOffFactory,
	}

	return f, nil
}

func (f *Framework) Boot() {
	ctx := context.TODO()

	f.bootOnce.Do(func() {
		operation := func() error {
			err := f.bootWithError(ctx)
			if err != nil {
				return microerror.Mask(err)
			}

			return nil
		}

		notifier := func(err error, d time.Duration) {
			f.logger.LogCtx(ctx, "warning", fmt.Sprintf("retrying operator boot due to error: %#v", microerror.Mask(err)))
		}

		err := backoff.RetryNotify(operation, f.backOffFactory(), notifier)
		if err != nil {
			f.logger.LogCtx(ctx, "error", fmt.Sprintf("stop operator boot retries due to too many errors: %#v", microerror.Mask(err)))
			os.Exit(1)
		}
	})
}

// DeleteFunc executes the framework's ProcessDelete function.
func (f *Framework) DeleteFunc(obj interface{}) {
	// DeleteFunc/UpdateFunc is synchronized to make sure only one of them is
	// executed at a time. DeleteFunc/UpdateFunc is not thread safe. This is
	// important because the source of truth for an operator are the reconciled
	// resources. In case we would run the operator logic in parallel, we would
	// run into race conditions.
	f.mutex.Lock()
	defer f.mutex.Unlock()

	resourceSet, err := f.resourceRouter.ResourceSet(obj)
	if err != nil {
		f.logger.Log("error", fmt.Sprintf("%#v", err), "event", "delete")
		return
	}

	ctx, err := resourceSet.InitCtx(context.Background(), obj)
	if err != nil {
		f.logger.Log("error", fmt.Sprintf("%#v", err), "event", "delete")
		return
	}

	f.logger.LogCtx(ctx, "action", "start", "component", "operatorkit", "function", "ProcessDelete")

	err = ProcessDelete(ctx, obj, resourceSet.Resources())
	if err != nil {
		f.logger.LogCtx(ctx, "error", fmt.Sprintf("%#v", err), "event", "delete")
		return
	}

	f.logger.LogCtx(ctx, "action", "end", "component", "operatorkit", "function", "ProcessDelete")
}

// UpdateFunc executes the framework's ProcessUpdate function.
func (f *Framework) UpdateFunc(oldObj, newObj interface{}) {
	obj := newObj

	// DeleteFunc/UpdateFunc is synchronized to make sure only one of them is
	// executed at a time. DeleteFunc/UpdateFunc is not thread safe. This is
	// important because the source of truth for an operator are the reconciled
	// resources. In case we would run the operator logic in parallel, we would
	// run into race conditions.
	f.mutex.Lock()
	defer f.mutex.Unlock()

	resourceSet, err := f.resourceRouter.ResourceSet(obj)
	if err != nil {
		f.logger.Log("error", fmt.Sprintf("%#v", err), "event", "update")
		return
	}

	ctx, err := resourceSet.InitCtx(context.Background(), obj)
	if err != nil {
		f.logger.Log("error", fmt.Sprintf("%#v", err), "event", "update")
		return
	}

	f.logger.LogCtx(ctx, "action", "start", "component", "operatorkit", "function", "ProcessUpdate")

	err = ProcessUpdate(ctx, obj, resourceSet.Resources())
	if err != nil {
		f.logger.LogCtx(ctx, "error", fmt.Sprintf("%#v", err), "event", "update")
		return
	}

	f.logger.LogCtx(ctx, "action", "end", "component", "operatorkit", "function", "ProcessUpdate")
}

// ProcessDelete is a drop-in for an informer's DeleteFunc. It receives the
// custom object observed during custom resource watches and anything that
// implements Resource. ProcessDelete takes care about all necessary
// reconciliation logic for delete events.
//
//     func deleteFunc(obj interface{}) {
//         err := f.ProcessDelete(obj, resources)
//         if err != nil {
//             // error handling here
//         }
//     }
//
//     newResourceEventHandler := &cache.ResourceEventHandlerFuncs{
//         DeleteFunc:    deleteFunc,
//     }
//
func ProcessDelete(ctx context.Context, obj interface{}, resources []Resource) error {
	if len(resources) == 0 {
		return microerror.Maskf(executionFailedError, "resources must not be empty")
	}

	for _, r := range resources {
		var err error

		var currentState interface{}
		{
			if reconciliationcanceledcontext.IsCanceled(ctx) {
				return nil
			}
			if resourcecanceledcontext.IsCanceled(ctx) {
				ctx = resourcecanceledcontext.NewContext(ctx, make(chan struct{}))
				continue
			}

			meta, ok := loggermeta.FromContext(ctx)
			if ok {
				meta.KeyVals["function"] = "GetCurrentState"
				defer delete(meta.KeyVals, "function")
			}
			currentState, err = r.GetCurrentState(ctx, obj)
			if err != nil {
				return microerror.Mask(err)
			}
		}

		var desiredState interface{}
		{
			if reconciliationcanceledcontext.IsCanceled(ctx) {
				return nil
			}
			if resourcecanceledcontext.IsCanceled(ctx) {
				ctx = resourcecanceledcontext.NewContext(ctx, make(chan struct{}))
				continue
			}

			meta, ok := loggermeta.FromContext(ctx)
			if ok {
				meta.KeyVals["function"] = "GetDesiredState"
				defer delete(meta.KeyVals, "function")
			}
			desiredState, err = r.GetDesiredState(ctx, obj)
			if err != nil {
				return microerror.Mask(err)
			}
		}

		var patch *Patch
		{
			if reconciliationcanceledcontext.IsCanceled(ctx) {
				return nil
			}
			if resourcecanceledcontext.IsCanceled(ctx) {
				ctx = resourcecanceledcontext.NewContext(ctx, make(chan struct{}))
				continue
			}

			meta, ok := loggermeta.FromContext(ctx)
			if ok {
				meta.KeyVals["function"] = "NewDeletePatch"
				defer delete(meta.KeyVals, "function")
			}
			patch, err = r.NewDeletePatch(ctx, obj, currentState, desiredState)
			if err != nil {
				return microerror.Mask(err)
			}
		}

		{
			if reconciliationcanceledcontext.IsCanceled(ctx) {
				return nil
			}
			if resourcecanceledcontext.IsCanceled(ctx) {
				ctx = resourcecanceledcontext.NewContext(ctx, make(chan struct{}))
				continue
			}

			if patch != nil {
				createChange, ok := patch.getCreateChange()
				if ok {
					meta, ok := loggermeta.FromContext(ctx)
					if ok {
						meta.KeyVals["function"] = "ApplyCreateChange"
						defer delete(meta.KeyVals, "function")
					}
					err := r.ApplyCreateChange(ctx, obj, createChange)
					if err != nil {
						return microerror.Mask(err)
					}
				}
			}
		}

		{
			if reconciliationcanceledcontext.IsCanceled(ctx) {
				return nil
			}
			if resourcecanceledcontext.IsCanceled(ctx) {
				ctx = resourcecanceledcontext.NewContext(ctx, make(chan struct{}))
				continue
			}

			if patch != nil {
				deleteChange, ok := patch.getDeleteChange()
				if ok {
					meta, ok := loggermeta.FromContext(ctx)
					if ok {
						meta.KeyVals["function"] = "ApplyDeleteChange"
						defer delete(meta.KeyVals, "function")
					}
					err := r.ApplyDeleteChange(ctx, obj, deleteChange)
					if err != nil {
						return microerror.Mask(err)
					}
				}
			}
		}

		{
			if reconciliationcanceledcontext.IsCanceled(ctx) {
				return nil
			}
			if resourcecanceledcontext.IsCanceled(ctx) {
				ctx = resourcecanceledcontext.NewContext(ctx, make(chan struct{}))
				continue
			}

			if patch != nil {
				updateChange, ok := patch.getUpdateChange()
				if ok {
					meta, ok := loggermeta.FromContext(ctx)
					if ok {
						meta.KeyVals["function"] = "ApplyUpdateChange"
						defer delete(meta.KeyVals, "function")
					}
					err := r.ApplyUpdateChange(ctx, obj, updateChange)
					if err != nil {
						return microerror.Mask(err)
					}
				}
			}
		}
	}

	return nil
}

// ProcessEvents takes the event channels created by the operatorkit informer
// and executes the framework's event functions accordingly.
func (f *Framework) ProcessEvents(ctx context.Context, deleteChan chan watch.Event, updateChan chan watch.Event, errChan chan error) {
	operation := func() error {
		for {
			select {
			case e := <-deleteChan:
				t := prometheus.NewTimer(frameworkHistogram.WithLabelValues("delete"))
				f.DeleteFunc(e.Object)
				t.ObserveDuration()
			case e := <-updateChan:
				t := prometheus.NewTimer(frameworkHistogram.WithLabelValues("update"))
				f.UpdateFunc(nil, e.Object)
				t.ObserveDuration()
			case err := <-errChan:
				return microerror.Mask(err)
			case <-ctx.Done():
				return nil
			}
		}
	}

	notifier := func(err error, d time.Duration) {
		f.logger.LogCtx(ctx, "warning", fmt.Sprintf("retrying operator event processing due to error: %#v", microerror.Mask(err)))
	}

	err := backoff.RetryNotify(operation, f.backOffFactory(), notifier)
	if err != nil {
		f.logger.LogCtx(ctx, "error", fmt.Sprintf("stop operator event processing retries due to too many errors: %#v", microerror.Mask(err)))
		os.Exit(1)
	}
}

// ProcessUpdate is a drop-in for an informer's UpdateFunc. It receives the new
// custom object observed during custom resource watches and anything that
// implements Resource. ProcessUpdate takes care about all necessary
// reconciliation logic for update events. For complex resources this means
// state has to be created, deleted and updated eventually, in this order.
//
//     func updateFunc(oldObj, newObj interface{}) {
//         err := f.ProcessUpdate(newObj, resources)
//         if err != nil {
//             // error handling here
//         }
//     }
//
//     newResourceEventHandler := &cache.ResourceEventHandlerFuncs{
//         UpdateFunc:    updateFunc,
//     }
//
func ProcessUpdate(ctx context.Context, obj interface{}, resources []Resource) error {
	if len(resources) == 0 {
		return microerror.Maskf(executionFailedError, "resources must not be empty")
	}

	for _, r := range resources {
		var err error

		var currentState interface{}
		{
			if reconciliationcanceledcontext.IsCanceled(ctx) {
				return nil
			}
			if resourcecanceledcontext.IsCanceled(ctx) {
				ctx = resourcecanceledcontext.NewContext(ctx, make(chan struct{}))
				continue
			}

			meta, ok := loggermeta.FromContext(ctx)
			if ok {
				meta.KeyVals["function"] = "GetCurrentState"
				defer delete(meta.KeyVals, "function")
			}
			currentState, err = r.GetCurrentState(ctx, obj)
			if err != nil {
				return microerror.Mask(err)
			}
		}

		var desiredState interface{}
		{
			if reconciliationcanceledcontext.IsCanceled(ctx) {
				return nil
			}
			if resourcecanceledcontext.IsCanceled(ctx) {
				ctx = resourcecanceledcontext.NewContext(ctx, make(chan struct{}))
				continue
			}

			meta, ok := loggermeta.FromContext(ctx)
			if ok {
				meta.KeyVals["function"] = "GetDesiredState"
				defer delete(meta.KeyVals, "function")
			}
			desiredState, err = r.GetDesiredState(ctx, obj)
			if err != nil {
				return microerror.Mask(err)
			}
		}

		var patch *Patch
		{
			if reconciliationcanceledcontext.IsCanceled(ctx) {
				return nil
			}
			if resourcecanceledcontext.IsCanceled(ctx) {
				ctx = resourcecanceledcontext.NewContext(ctx, make(chan struct{}))
				continue
			}

			meta, ok := loggermeta.FromContext(ctx)
			if ok {
				meta.KeyVals["function"] = "NewUpdatePatch"
				defer delete(meta.KeyVals, "function")
			}
			patch, err = r.NewUpdatePatch(ctx, obj, currentState, desiredState)
			if err != nil {
				return microerror.Mask(err)
			}
		}

		{
			if reconciliationcanceledcontext.IsCanceled(ctx) {
				return nil
			}
			if resourcecanceledcontext.IsCanceled(ctx) {
				ctx = resourcecanceledcontext.NewContext(ctx, make(chan struct{}))
				continue
			}

			if patch != nil {
				createState, ok := patch.getCreateChange()
				if ok {
					meta, ok := loggermeta.FromContext(ctx)
					if ok {
						meta.KeyVals["function"] = "ApplyCreateChange"
						defer delete(meta.KeyVals, "function")
					}
					err := r.ApplyCreateChange(ctx, obj, createState)
					if err != nil {
						return microerror.Mask(err)
					}
				}
			}
		}

		{
			if reconciliationcanceledcontext.IsCanceled(ctx) {
				return nil
			}
			if resourcecanceledcontext.IsCanceled(ctx) {
				ctx = resourcecanceledcontext.NewContext(ctx, make(chan struct{}))
				continue
			}

			if patch != nil {
				deleteState, ok := patch.getDeleteChange()
				if ok {
					meta, ok := loggermeta.FromContext(ctx)
					if ok {
						meta.KeyVals["function"] = "ApplyDeleteChange"
						defer delete(meta.KeyVals, "function")
					}
					err := r.ApplyDeleteChange(ctx, obj, deleteState)
					if err != nil {
						return microerror.Mask(err)
					}
				}
			}
		}

		{
			if reconciliationcanceledcontext.IsCanceled(ctx) {
				return nil
			}
			if resourcecanceledcontext.IsCanceled(ctx) {
				ctx = resourcecanceledcontext.NewContext(ctx, make(chan struct{}))
				continue
			}

			if patch != nil {
				updateState, ok := patch.getUpdateChange()
				if ok {
					meta, ok := loggermeta.FromContext(ctx)
					if ok {
						meta.KeyVals["function"] = "ApplyUpdateChange"
						defer delete(meta.KeyVals, "function")
					}
					err := r.ApplyUpdateChange(ctx, obj, updateState)
					if err != nil {
						return microerror.Mask(err)
					}
				}
			}
		}
	}

	return nil
}

func (f *Framework) bootWithError(ctx context.Context) error {
	if f.crd != nil {
		f.logger.LogCtx(ctx, "debug", "ensuring custom resource definition exists")

		err := f.crdClient.EnsureCreated(ctx, f.crd, f.backOffFactory())
		if err != nil {
			return microerror.Mask(err)
		}

		f.logger.LogCtx(ctx, "debug", "ensured custom resource definition")

		// TODO collect metrics
	}

	f.logger.LogCtx(ctx, "debug", "starting list/watch")

	deleteChan, updateChan, errChan := f.informer.Watch(ctx)
	f.ProcessEvents(ctx, deleteChan, updateChan, errChan)

	return nil
}
