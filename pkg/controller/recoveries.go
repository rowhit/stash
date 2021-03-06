package controller

import (
	"fmt"

	"github.com/appscode/go/log"
	api "github.com/appscode/stash/apis/stash/v1alpha1"
	stash_util "github.com/appscode/stash/client/typed/stash/v1alpha1/util"
	stash_listers "github.com/appscode/stash/listers/stash/v1alpha1"
	"github.com/appscode/stash/pkg/eventer"
	"github.com/appscode/stash/pkg/util"
	"github.com/golang/glog"
	core "k8s.io/api/core/v1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	rt "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

func (c *StashController) initRecoveryWatcher() {
	lw := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (rt.Object, error) {
			return c.stashClient.Recoveries(core.NamespaceAll).List(options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return c.stashClient.Recoveries(core.NamespaceAll).Watch(options)
		},
	}

	// create the workqueue
	c.recQueue = workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "recovery")

	// Bind the workqueue to a cache with the help of an informer. This way we make sure that
	// whenever the cache is updated, the pod key is added to the workqueue.
	// Note that when we finally process the item from the workqueue, we might see a newer version
	// of the Recovery than the version which was responsible for triggering the update.
	c.recIndexer, c.recInformer = cache.NewIndexerInformer(lw, &api.Recovery{}, c.options.ResyncPeriod, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if r, ok := obj.(*api.Recovery); ok {
				if err := r.IsValid(); err != nil {
					c.recorder.Eventf(
						r.ObjectReference(),
						core.EventTypeWarning,
						eventer.EventReasonInvalidRecovery,
						"Reason %v",
						err,
					)
					return
				} else {
					key, err := cache.MetaNamespaceKeyFunc(obj)
					if err == nil {
						c.recQueue.Add(key)
					}
				}
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			oldObj, ok := old.(*api.Recovery)
			if !ok {
				log.Errorln("Invalid Recovery object")
				return
			}
			newObj, ok := new.(*api.Recovery)
			if !ok {
				log.Errorln("Invalid Recovery object")
				return
			}
			if err := newObj.IsValid(); err != nil {
				c.recorder.Eventf(
					newObj.ObjectReference(),
					core.EventTypeWarning,
					eventer.EventReasonInvalidRecovery,
					"Reason %v",
					err,
				)
				return
			} else if !util.RecoveryEqual(oldObj, newObj) {
				key, err := cache.MetaNamespaceKeyFunc(new)
				if err == nil {
					c.recQueue.Add(key)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			// IndexerInformer uses a delta queue, therefore for deletes we have to use this
			// key function.
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				c.recQueue.Add(key)
			}
		},
	}, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	c.recLister = stash_listers.NewRecoveryLister(c.recIndexer)
}

func (c *StashController) runRecoveryWatcher() {
	for c.processNextRecovery() {
	}
}

func (c *StashController) processNextRecovery() bool {
	// Wait until there is a new item in the working queue
	key, quit := c.recQueue.Get()
	if quit {
		return false
	}
	// Tell the queue that we are done with processing this key. This unblocks the key for other workers
	// This allows safe parallel processing because two deployments with the same key are never processed in
	// parallel.
	defer c.recQueue.Done(key)

	// Invoke the method containing the business logic
	err := c.runRecoveryInjector(key.(string))
	if err == nil {
		// Forget about the #AddRateLimited history of the key on every successful synchronization.
		// This ensures that future processing of updates for this key is not delayed because of
		// an outdated error history.
		c.recQueue.Forget(key)
		return true
	}
	log.Errorf("Failed to process Recovery %v. Reason: %s", key, err)

	// This controller retries 5 times if something goes wrong. After that, it stops trying.
	if c.recQueue.NumRequeues(key) < c.options.MaxNumRequeues {
		glog.Infof("Error syncing recovery %v: %v", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.recQueue.AddRateLimited(key)
		return true
	}

	c.recQueue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	runtime.HandleError(err)
	glog.Infof("Dropping recovery %q out of the queue: %v", key, err)
	return true
}

// syncToStdout is the business logic of the controller. In this controller it simply prints
// information about the deployment to stdout. In case an error happened, it has to simply return the error.
// The retry logic should not be part of the business logic.
func (c *StashController) runRecoveryInjector(key string) error {
	obj, exists, err := c.recIndexer.GetByKey(key)
	if err != nil {
		glog.Errorf("Fetching object with key %s from store failed with %v", key, err)
		return err
	}

	if !exists {
		// Below we will warm up our cache with a Recovery, so that we will see a delete for one d
		fmt.Printf("Recovery %s does not exist anymore\n", key)
		return nil
	}

	d := obj.(*api.Recovery)
	fmt.Printf("Sync/Add/Update for Recovery %s\n", d.GetName())
	return c.runRecoveryJob(d)
}

func (c *StashController) runRecoveryJob(rec *api.Recovery) error {
	if rec.Status.Phase == api.RecoverySucceeded || rec.Status.Phase == api.RecoveryRunning {
		return nil
	}

	restic, err := c.stashClient.Restics(rec.Namespace).Get(rec.Spec.Restic, metav1.GetOptions{})
	if err != nil {
		log.Errorln(err)
		stash_util.SetRecoveryStatusPhase(c.stashClient, rec, api.RecoveryFailed)
		c.recorder.Event(rec.ObjectReference(), core.EventTypeWarning, eventer.EventReasonFailedToRecover, err.Error())
		return err
	}

	if err = restic.IsValid(); err != nil {
		log.Errorln(err)
		stash_util.SetRecoveryStatusPhase(c.stashClient, rec, api.RecoveryFailed)
		c.recorder.Event(rec.ObjectReference(), core.EventTypeWarning, eventer.EventReasonFailedToRecover, err.Error())
		return err
	}

	job := util.CreateRecoveryJob(rec, restic, c.options.SidecarImageTag)
	if c.options.EnableRBAC {
		if err = c.ensureRecoveryRBAC(job.Name, job.Namespace); err != nil {
			return fmt.Errorf("error ensuring rbac for recovery job %s, reason: %s\n", job.Name, err)
		}
		job.Spec.Template.Spec.ServiceAccountName = job.Name
	}
	if job, err = c.k8sClient.BatchV1().Jobs(rec.Namespace).Create(job); err != nil {
		if kerr.IsAlreadyExists(err) {
			return nil
		}
		log.Errorln(err)
		stash_util.SetRecoveryStatusPhase(c.stashClient, rec, api.RecoveryFailed)
		c.recorder.Event(rec.ObjectReference(), core.EventTypeWarning, eventer.EventReasonFailedToRecover, err.Error())
		return err
	}

	log.Infoln("Recovery job created:", job.Name)
	c.recorder.Eventf(rec.ObjectReference(), core.EventTypeNormal, eventer.EventReasonJobCreated, "Recovery job created: %s", job.Name)
	stash_util.SetRecoveryStatusPhase(c.stashClient, rec, api.RecoveryRunning)

	return nil
}
