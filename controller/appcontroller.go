package controller

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"reflect"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	clustercache "github.com/argoproj/gitops-engine/pkg/cache"
	"github.com/argoproj/gitops-engine/pkg/diff"
	"github.com/argoproj/gitops-engine/pkg/health"
	synccommon "github.com/argoproj/gitops-engine/pkg/sync/common"
	resourceutil "github.com/argoproj/gitops-engine/pkg/sync/resource"
	"github.com/argoproj/gitops-engine/pkg/utils/kube"
	jsonpatch "github.com/evanphx/json-patch"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/semaphore"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/informers"
	informerv1 "k8s.io/client-go/informers/apps/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"

	commitclient "github.com/argoproj/argo-cd/v3/commitserver/apiclient"
	"github.com/argoproj/argo-cd/v3/common"
	statecache "github.com/argoproj/argo-cd/v3/controller/cache"
	"github.com/argoproj/argo-cd/v3/controller/hydrator"
	hydratortypes "github.com/argoproj/argo-cd/v3/controller/hydrator/types"
	"github.com/argoproj/argo-cd/v3/controller/metrics"
	"github.com/argoproj/argo-cd/v3/controller/sharding"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application"
	appv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	appclientset "github.com/argoproj/argo-cd/v3/pkg/client/clientset/versioned"
	"github.com/argoproj/argo-cd/v3/pkg/client/informers/externalversions/application/v1alpha1"
	applisters "github.com/argoproj/argo-cd/v3/pkg/client/listers/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/reposerver/apiclient"
	applog "github.com/argoproj/argo-cd/v3/util/app/log"
	"github.com/argoproj/argo-cd/v3/util/argo"
	argodiff "github.com/argoproj/argo-cd/v3/util/argo/diff"
	"github.com/argoproj/argo-cd/v3/util/argo/normalizers"
	"github.com/argoproj/argo-cd/v3/util/env"
	"github.com/argoproj/argo-cd/v3/util/stats"

	"github.com/argoproj/argo-cd/v3/pkg/ratelimiter"
	appstatecache "github.com/argoproj/argo-cd/v3/util/cache/appstate"
	"github.com/argoproj/argo-cd/v3/util/db"
	"github.com/argoproj/argo-cd/v3/util/errors"
	"github.com/argoproj/argo-cd/v3/util/glob"
	"github.com/argoproj/argo-cd/v3/util/helm"
	logutils "github.com/argoproj/argo-cd/v3/util/log"
	settings_util "github.com/argoproj/argo-cd/v3/util/settings"
)

const (
	updateOperationStateTimeout             = 1 * time.Second
	defaultDeploymentInformerResyncDuration = 10 * time.Second
	// orphanedIndex contains application which monitor orphaned resources by namespace
	orphanedIndex = "orphaned"
)

type CompareWith int

const (
	// Compare live application state against state defined in latest git revision with no resolved revision caching.
	CompareWithLatestForceResolve CompareWith = 3
	// Compare live application state against state defined in latest git revision.
	CompareWithLatest CompareWith = 2
	// Compare live application state against state defined using revision of most recent comparison.
	CompareWithRecent CompareWith = 1
	// Skip comparison and only refresh application resources tree
	ComparisonWithNothing CompareWith = 0
)

func (a CompareWith) Max(b CompareWith) CompareWith {
	return CompareWith(math.Max(float64(a), float64(b)))
}

func (a CompareWith) Pointer() *CompareWith {
	return &a
}

// ApplicationController is the controller for application resources.
type ApplicationController struct {
	cache                *appstatecache.Cache
	namespace            string
	kubeClientset        kubernetes.Interface
	kubectl              kube.Kubectl
	applicationClientset appclientset.Interface
	auditLogger          *argo.AuditLogger
	// queue contains app namespace/name
	appRefreshQueue workqueue.TypedRateLimitingInterface[string]
	// queue contains app namespace/name/comparisonType and used to request app refresh with the predefined comparison type
	appComparisonTypeRefreshQueue workqueue.TypedRateLimitingInterface[string]
	appOperationQueue             workqueue.TypedRateLimitingInterface[string]
	projectRefreshQueue           workqueue.TypedRateLimitingInterface[string]
	appHydrateQueue               workqueue.TypedRateLimitingInterface[string]
	hydrationQueue                workqueue.TypedRateLimitingInterface[hydratortypes.HydrationQueueKey]
	appInformer                   cache.SharedIndexInformer
	appLister                     applisters.ApplicationLister
	projInformer                  cache.SharedIndexInformer
	appStateManager               AppStateManager
	stateCache                    statecache.LiveStateCache
	statusRefreshTimeout          time.Duration
	statusHardRefreshTimeout      time.Duration
	statusRefreshJitter           time.Duration
	selfHealTimeout               time.Duration
	selfHealBackoff               *wait.Backoff
	selfHealBackoffCooldown       time.Duration
	syncTimeout                   time.Duration
	db                            db.ArgoDB
	settingsMgr                   *settings_util.SettingsManager
	refreshRequestedApps          map[string]CompareWith
	refreshRequestedAppsMutex     *sync.Mutex
	metricsServer                 *metrics.MetricsServer
	metricsClusterLabels          []string
	kubectlSemaphore              *semaphore.Weighted
	clusterSharding               sharding.ClusterShardingCache
	projByNameCache               sync.Map
	applicationNamespaces         []string
	ignoreNormalizerOpts          normalizers.IgnoreNormalizerOpts

	// dynamicClusterDistributionEnabled if disabled deploymentInformer is never initialized
	dynamicClusterDistributionEnabled bool
	deploymentInformer                informerv1.DeploymentInformer

	hydrator *hydrator.Hydrator
}

// NewApplicationController creates new instance of ApplicationController.
func NewApplicationController(
	namespace string,
	settingsMgr *settings_util.SettingsManager,
	kubeClientset kubernetes.Interface,
	applicationClientset appclientset.Interface,
	repoClientset apiclient.Clientset,
	commitClientset commitclient.Clientset,
	argoCache *appstatecache.Cache,
	kubectl kube.Kubectl,
	appResyncPeriod time.Duration,
	appHardResyncPeriod time.Duration,
	appResyncJitter time.Duration,
	selfHealTimeout time.Duration,
	selfHealBackoff *wait.Backoff,
	selfHealBackoffCooldown time.Duration,
	syncTimeout time.Duration,
	repoErrorGracePeriod time.Duration,
	metricsPort int,
	metricsCacheExpiration time.Duration,
	metricsApplicationLabels []string,
	metricsApplicationConditions []string,
	metricsClusterLabels []string,
	kubectlParallelismLimit int64,
	persistResourceHealth bool,
	clusterSharding sharding.ClusterShardingCache,
	applicationNamespaces []string,
	rateLimiterConfig *ratelimiter.AppControllerRateLimiterConfig,
	serverSideDiff bool,
	dynamicClusterDistributionEnabled bool,
	ignoreNormalizerOpts normalizers.IgnoreNormalizerOpts,
	enableK8sEvent []string,
	hydratorEnabled bool,
) (*ApplicationController, error) {
	log.Infof("appResyncPeriod=%v, appHardResyncPeriod=%v, appResyncJitter=%v", appResyncPeriod, appHardResyncPeriod, appResyncJitter)
	db := db.NewDB(namespace, settingsMgr, kubeClientset)
	if rateLimiterConfig == nil {
		rateLimiterConfig = ratelimiter.GetDefaultAppRateLimiterConfig()
		log.Info("Using default workqueue rate limiter config")
	}
	ctrl := ApplicationController{
		cache:                             argoCache,
		namespace:                         namespace,
		kubeClientset:                     kubeClientset,
		kubectl:                           kubectl,
		applicationClientset:              applicationClientset,
		appRefreshQueue:                   workqueue.NewTypedRateLimitingQueueWithConfig(ratelimiter.NewCustomAppControllerRateLimiter[string](rateLimiterConfig), workqueue.TypedRateLimitingQueueConfig[string]{Name: "app_reconciliation_queue"}),
		appOperationQueue:                 workqueue.NewTypedRateLimitingQueueWithConfig(ratelimiter.NewCustomAppControllerRateLimiter[string](rateLimiterConfig), workqueue.TypedRateLimitingQueueConfig[string]{Name: "app_operation_processing_queue"}),
		projectRefreshQueue:               workqueue.NewTypedRateLimitingQueueWithConfig(ratelimiter.NewCustomAppControllerRateLimiter[string](rateLimiterConfig), workqueue.TypedRateLimitingQueueConfig[string]{Name: "project_reconciliation_queue"}),
		appComparisonTypeRefreshQueue:     workqueue.NewTypedRateLimitingQueue(ratelimiter.NewCustomAppControllerRateLimiter[string](rateLimiterConfig)),
		appHydrateQueue:                   workqueue.NewTypedRateLimitingQueueWithConfig(ratelimiter.NewCustomAppControllerRateLimiter[string](rateLimiterConfig), workqueue.TypedRateLimitingQueueConfig[string]{Name: "app_hydration_queue"}),
		hydrationQueue:                    workqueue.NewTypedRateLimitingQueueWithConfig(ratelimiter.NewCustomAppControllerRateLimiter[hydratortypes.HydrationQueueKey](rateLimiterConfig), workqueue.TypedRateLimitingQueueConfig[hydratortypes.HydrationQueueKey]{Name: "manifest_hydration_queue"}),
		db:                                db,
		statusRefreshTimeout:              appResyncPeriod,
		statusHardRefreshTimeout:          appHardResyncPeriod,
		statusRefreshJitter:               appResyncJitter,
		refreshRequestedApps:              make(map[string]CompareWith),
		refreshRequestedAppsMutex:         &sync.Mutex{},
		auditLogger:                       argo.NewAuditLogger(kubeClientset, common.ApplicationController, enableK8sEvent),
		settingsMgr:                       settingsMgr,
		selfHealTimeout:                   selfHealTimeout,
		selfHealBackoff:                   selfHealBackoff,
		selfHealBackoffCooldown:           selfHealBackoffCooldown,
		syncTimeout:                       syncTimeout,
		clusterSharding:                   clusterSharding,
		projByNameCache:                   sync.Map{},
		applicationNamespaces:             applicationNamespaces,
		dynamicClusterDistributionEnabled: dynamicClusterDistributionEnabled,
		ignoreNormalizerOpts:              ignoreNormalizerOpts,
		metricsClusterLabels:              metricsClusterLabels,
	}
	if hydratorEnabled {
		ctrl.hydrator = hydrator.NewHydrator(&ctrl, appResyncPeriod, commitClientset, repoClientset, db)
	}
	if kubectlParallelismLimit > 0 {
		ctrl.kubectlSemaphore = semaphore.NewWeighted(kubectlParallelismLimit)
	}
	kubectl.SetOnKubectlRun(ctrl.onKubectlRun)
	appInformer, appLister := ctrl.newApplicationInformerAndLister()
	indexers := cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}
	projInformer := v1alpha1.NewAppProjectInformer(applicationClientset, namespace, appResyncPeriod, indexers)
	var err error
	_, err = projInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if key, err := cache.MetaNamespaceKeyFunc(obj); err == nil {
				ctrl.projectRefreshQueue.AddRateLimited(key)
				if projMeta, ok := obj.(metav1.Object); ok {
					ctrl.InvalidateProjectsCache(projMeta.GetName())
				}
			}
		},
		UpdateFunc: func(_, new any) {
			if key, err := cache.MetaNamespaceKeyFunc(new); err == nil {
				ctrl.projectRefreshQueue.AddRateLimited(key)
				if projMeta, ok := new.(metav1.Object); ok {
					ctrl.InvalidateProjectsCache(projMeta.GetName())
				}
			}
		},
		DeleteFunc: func(obj any) {
			if key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err == nil {
				// immediately push to queue for deletes
				ctrl.projectRefreshQueue.Add(key)
				if projMeta, ok := obj.(metav1.Object); ok {
					ctrl.InvalidateProjectsCache(projMeta.GetName())
				}
			}
		},
	})
	if err != nil {
		return nil, err
	}

	factory := informers.NewSharedInformerFactoryWithOptions(ctrl.kubeClientset, defaultDeploymentInformerResyncDuration, informers.WithNamespace(settingsMgr.GetNamespace()))

	var deploymentInformer informerv1.DeploymentInformer

	// only initialize deployment informer if dynamic distribution is enabled
	if dynamicClusterDistributionEnabled {
		deploymentInformer = factory.Apps().V1().Deployments()
	}

	readinessHealthCheck := func(_ *http.Request) error {
		if dynamicClusterDistributionEnabled {
			applicationControllerName := env.StringFromEnv(common.EnvAppControllerName, common.DefaultApplicationControllerName)
			appControllerDeployment, err := deploymentInformer.Lister().Deployments(settingsMgr.GetNamespace()).Get(applicationControllerName)
			if err != nil {
				if !apierrors.IsNotFound(err) {
					return fmt.Errorf("error retrieving Application Controller Deployment: %w", err)
				}
				appControllerDeployment = nil
			}
			if appControllerDeployment != nil {
				if appControllerDeployment.Spec.Replicas != nil && int(*appControllerDeployment.Spec.Replicas) <= 0 {
					return fmt.Errorf("application controller deployment replicas is not set or is less than 0, replicas: %d", appControllerDeployment.Spec.Replicas)
				}
				shard := env.ParseNumFromEnv(common.EnvControllerShard, -1, -math.MaxInt32, math.MaxInt32)
				shard, err := sharding.GetOrUpdateShardFromConfigMap(kubeClientset.(*kubernetes.Clientset), settingsMgr, int(*appControllerDeployment.Spec.Replicas), shard)
				if err != nil {
					return fmt.Errorf("error while updating the heartbeat for to the Shard Mapping ConfigMap: %w", err)
				}

				// update the shard number in the clusterSharding, and resync all applications if the shard number is updated
				if ctrl.clusterSharding.UpdateShard(shard) {
					// update shard number in stateCache
					ctrl.stateCache.UpdateShard(shard)

					// resync all applications
					apps, err := ctrl.appLister.List(labels.Everything())
					if err != nil {
						return err
					}
					for _, app := range apps {
						if !ctrl.canProcessApp(app) {
							continue
						}
						key, err := cache.MetaNamespaceKeyFunc(app)
						if err == nil {
							ctrl.appRefreshQueue.AddRateLimited(key)
							ctrl.clusterSharding.AddApp(app)
						}
					}
				}
			}
		}
		return nil
	}

	metricsAddr := fmt.Sprintf("0.0.0.0:%d", metricsPort)

	ctrl.metricsServer, err = metrics.NewMetricsServer(metricsAddr, appLister, ctrl.canProcessApp, readinessHealthCheck, metricsApplicationLabels, metricsApplicationConditions, ctrl.db)
	if err != nil {
		return nil, err
	}
	if metricsCacheExpiration.Seconds() != 0 {
		err = ctrl.metricsServer.SetExpiration(metricsCacheExpiration)
		if err != nil {
			return nil, err
		}
	}
	stateCache := statecache.NewLiveStateCache(db, appInformer, ctrl.settingsMgr, ctrl.metricsServer, ctrl.handleObjectUpdated, clusterSharding, argo.NewResourceTracking())
	appStateManager := NewAppStateManager(db, applicationClientset, repoClientset, namespace, kubectl, ctrl.onKubectlRun, ctrl.settingsMgr, stateCache, ctrl.metricsServer, argoCache, ctrl.statusRefreshTimeout, argo.NewResourceTracking(), persistResourceHealth, repoErrorGracePeriod, serverSideDiff, ignoreNormalizerOpts)
	ctrl.appInformer = appInformer
	ctrl.appLister = appLister
	ctrl.projInformer = projInformer
	ctrl.deploymentInformer = deploymentInformer
	ctrl.appStateManager = appStateManager
	ctrl.stateCache = stateCache

	return &ctrl, nil
}

func (ctrl *ApplicationController) InvalidateProjectsCache(names ...string) {
	if len(names) > 0 {
		for _, name := range names {
			ctrl.projByNameCache.Delete(name)
		}
	} else if ctrl != nil {
		ctrl.projByNameCache.Range(func(key, _ any) bool {
			ctrl.projByNameCache.Delete(key)
			return true
		})
	}
}

func (ctrl *ApplicationController) GetMetricsServer() *metrics.MetricsServer {
	return ctrl.metricsServer
}

func (ctrl *ApplicationController) onKubectlRun(command string) (kube.CleanupFunc, error) {
	ctrl.metricsServer.IncKubectlExec(command)
	if ctrl.kubectlSemaphore != nil {
		if err := ctrl.kubectlSemaphore.Acquire(context.Background(), 1); err != nil {
			return nil, err
		}
		ctrl.metricsServer.IncKubectlExecPending(command)
	}
	return func() {
		if ctrl.kubectlSemaphore != nil {
			ctrl.kubectlSemaphore.Release(1)
			ctrl.metricsServer.DecKubectlExecPending(command)
		}
	}, nil
}

func isSelfReferencedApp(app *appv1.Application, ref corev1.ObjectReference) bool {
	gvk := ref.GroupVersionKind()
	return ref.UID == app.UID &&
		ref.Name == app.Name &&
		ref.Namespace == app.Namespace &&
		gvk.Group == application.Group &&
		gvk.Kind == application.ApplicationKind
}

func (ctrl *ApplicationController) newAppProjCache(name string) *appProjCache {
	return &appProjCache{name: name, ctrl: ctrl}
}

type appProjCache struct {
	name string
	ctrl *ApplicationController

	lock    sync.Mutex
	appProj *appv1.AppProject
}

// GetAppProject gets an AppProject from the cache. If the AppProject is not
// yet cached, retrieves the AppProject from the K8s control plane and stores
// in the cache.
func (projCache *appProjCache) GetAppProject(ctx context.Context) (*appv1.AppProject, error) {
	projCache.lock.Lock()
	defer projCache.lock.Unlock()
	if projCache.appProj != nil {
		return projCache.appProj, nil
	}
	proj, err := argo.GetAppProjectByName(ctx, projCache.name, applisters.NewAppProjectLister(projCache.ctrl.projInformer.GetIndexer()), projCache.ctrl.namespace, projCache.ctrl.settingsMgr, projCache.ctrl.db)
	if err != nil {
		return nil, err
	}
	projCache.appProj = proj
	return projCache.appProj, nil
}

// getAppProj gets the AppProject for the given Application app.
func (ctrl *ApplicationController) getAppProj(app *appv1.Application) (*appv1.AppProject, error) {
	projCache, _ := ctrl.projByNameCache.Load(app.Spec.GetProject())
	if projCache == nil {
		projCache = ctrl.newAppProjCache(app.Spec.GetProject())
		ctrl.projByNameCache.Store(app.Spec.GetProject(), projCache)
	}
	proj, err := projCache.(*appProjCache).GetAppProject(context.TODO())
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, err
		}
		return nil, fmt.Errorf("could not retrieve AppProject '%s' from cache: %w", app.Spec.Project, err)
	}
	if !proj.IsAppNamespacePermitted(app, ctrl.namespace) {
		return nil, argo.ErrProjectNotPermitted(app.GetName(), app.GetNamespace(), proj.GetName())
	}
	return proj, nil
}

func (ctrl *ApplicationController) handleObjectUpdated(managedByApp map[string]bool, ref corev1.ObjectReference) {
	// if namespaced resource is not managed by any app it might be orphaned resource of some other apps
	if len(managedByApp) == 0 && ref.Namespace != "" {
		// retrieve applications which monitor orphaned resources in the same namespace and refresh them unless resource is denied in app project
		if objs, err := ctrl.appInformer.GetIndexer().ByIndex(orphanedIndex, ref.Namespace); err == nil {
			for i := range objs {
				app, ok := objs[i].(*appv1.Application)
				if !ok {
					continue
				}

				managedByApp[app.InstanceName(ctrl.namespace)] = true
			}
		}
	}
	for appName, isManagedResource := range managedByApp {
		// The appName is given as <namespace>_<name>, but the indexer needs it
		// format <namespace>/<name>
		appKey := ctrl.toAppKey(appName)
		obj, exists, err := ctrl.appInformer.GetIndexer().GetByKey(appKey)
		app, ok := obj.(*appv1.Application)
		if exists && err == nil && ok && isSelfReferencedApp(app, ref) {
			// Don't force refresh app if related resource is application itself. This prevents infinite reconciliation loop.
			continue
		}

		if !ctrl.canProcessApp(obj) {
			// Don't force refresh app if app belongs to a different controller shard or is outside the allowed namespaces.
			continue
		}

		logCtx := log.WithFields(applog.GetAppLogFields(app))
		// Enforce application's permission for the source namespace
		_, err = ctrl.getAppProj(app)
		if err != nil {
			logCtx.Errorf("Unable to determine project for app '%s': %v", app.QualifiedName(), err)
			continue
		}

		level := ComparisonWithNothing
		if isManagedResource {
			level = CompareWithRecent
		}

		namespace := ref.Namespace
		if ref.Namespace == "" {
			namespace = "(cluster-scoped)"
		}
		logCtx.WithFields(log.Fields{
			"comparison-level": level,
			"namespace":        namespace,
			"name":             ref.Name,
			"api-version":      ref.APIVersion,
			"kind":             ref.Kind,
			"server":           app.Spec.Destination.Server,
			"cluster-name":     app.Spec.Destination.Name,
		}).Debug("Requesting app refresh caused by object update")

		ctrl.requestAppRefresh(app.QualifiedName(), &level, nil)
	}
}

// setAppManagedResources will build a list of ResourceDiff based on the provided comparisonResult
// and persist app resources related data in the cache. Will return the persisted ApplicationTree.
func (ctrl *ApplicationController) setAppManagedResources(destCluster *appv1.Cluster, a *appv1.Application, comparisonResult *comparisonResult) (*appv1.ApplicationTree, error) {
	ts := stats.NewTimingStats()
	defer func() {
		logCtx := log.WithFields(applog.GetAppLogFields(a))
		for k, v := range ts.Timings() {
			logCtx = logCtx.WithField(k, v.Milliseconds())
		}
		logCtx = logCtx.WithField("time_ms", time.Since(ts.StartTime).Milliseconds())
		logCtx.Debug("Finished setting app managed resources")
	}()
	managedResources, err := ctrl.hideSecretData(destCluster, a, comparisonResult)
	ts.AddCheckpoint("hide_secret_data_ms")
	if err != nil {
		return nil, fmt.Errorf("error getting managed resources: %w", err)
	}
	tree, err := ctrl.getResourceTree(destCluster, a, managedResources)
	ts.AddCheckpoint("get_resource_tree_ms")
	if err != nil {
		return nil, fmt.Errorf("error getting resource tree: %w", err)
	}
	err = ctrl.cache.SetAppResourcesTree(a.InstanceName(ctrl.namespace), tree)
	ts.AddCheckpoint("set_app_resources_tree_ms")
	if err != nil {
		return nil, fmt.Errorf("error setting app resource tree: %w", err)
	}
	err = ctrl.cache.SetAppManagedResources(a.InstanceName(ctrl.namespace), managedResources)
	ts.AddCheckpoint("set_app_managed_resources_ms")
	if err != nil {
		return nil, fmt.Errorf("error setting app managed resources: %w", err)
	}
	return tree, nil
}

// returns true of given resources exist in the namespace by default and not managed by the user
func isKnownOrphanedResourceExclusion(key kube.ResourceKey, proj *appv1.AppProject) bool {
	if key.Namespace == "default" && key.Group == "" && key.Kind == kube.ServiceKind && key.Name == "kubernetes" {
		return true
	}
	if key.Group == "" && key.Kind == kube.ServiceAccountKind && key.Name == "default" {
		return true
	}
	if key.Group == "" && key.Kind == "ConfigMap" && key.Name == "kube-root-ca.crt" {
		return true
	}
	list := proj.Spec.OrphanedResources.Ignore
	for _, item := range list {
		if item.Kind == "" || glob.Match(item.Kind, key.Kind) {
			if glob.Match(item.Group, key.Group) {
				if item.Name == "" || glob.Match(item.Name, key.Name) {
					return true
				}
			}
		}
	}
	return false
}

func (ctrl *ApplicationController) getResourceTree(destCluster *appv1.Cluster, a *appv1.Application, managedResources []*appv1.ResourceDiff) (*appv1.ApplicationTree, error) {
	ts := stats.NewTimingStats()
	defer func() {
		logCtx := log.WithFields(applog.GetAppLogFields(a))
		for k, v := range ts.Timings() {
			logCtx = logCtx.WithField(k, v.Milliseconds())
		}
		logCtx = logCtx.WithField("time_ms", time.Since(ts.StartTime).Milliseconds())
		logCtx.Debug("Finished getting resource tree")
	}()
	nodes := make([]appv1.ResourceNode, 0)
	proj, err := ctrl.getAppProj(a)
	ts.AddCheckpoint("get_app_proj_ms")
	if err != nil {
		return nil, fmt.Errorf("failed to get project: %w", err)
	}

	orphanedNodesMap := make(map[kube.ResourceKey]appv1.ResourceNode)
	warnOrphaned := true
	if proj.Spec.OrphanedResources != nil {
		orphanedNodesMap, err = ctrl.stateCache.GetNamespaceTopLevelResources(destCluster, a.Spec.Destination.Namespace)
		if err != nil {
			return nil, fmt.Errorf("failed to get namespace top-level resources: %w", err)
		}
		warnOrphaned = proj.Spec.OrphanedResources.IsWarn()
	}
	ts.AddCheckpoint("get_orphaned_resources_ms")
	managedResourcesKeys := make([]kube.ResourceKey, 0)
	for i := range managedResources {
		managedResource := managedResources[i]
		delete(orphanedNodesMap, kube.NewResourceKey(managedResource.Group, managedResource.Kind, managedResource.Namespace, managedResource.Name))
		live := &unstructured.Unstructured{}
		err := json.Unmarshal([]byte(managedResource.LiveState), &live)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal live state of managed resources: %w", err)
		}

		if live == nil {
			target := &unstructured.Unstructured{}
			err = json.Unmarshal([]byte(managedResource.TargetState), &target)
			if err != nil {
				return nil, fmt.Errorf("failed to unmarshal target state of managed resources: %w", err)
			}
			nodes = append(nodes, appv1.ResourceNode{
				ResourceRef: appv1.ResourceRef{
					Version:   target.GroupVersionKind().Version,
					Name:      managedResource.Name,
					Kind:      managedResource.Kind,
					Group:     managedResource.Group,
					Namespace: managedResource.Namespace,
				},
				Health: &appv1.HealthStatus{
					Status: health.HealthStatusMissing,
				},
			})
		} else {
			managedResourcesKeys = append(managedResourcesKeys, kube.GetResourceKey(live))
		}
	}
	err = ctrl.stateCache.IterateHierarchyV2(destCluster, managedResourcesKeys, func(child appv1.ResourceNode, _ string) bool {
		permitted, _ := proj.IsResourcePermitted(schema.GroupKind{Group: child.Group, Kind: child.Kind}, child.Namespace, destCluster, func(project string) ([]*appv1.Cluster, error) {
			clusters, err := ctrl.db.GetProjectClusters(context.TODO(), project)
			if err != nil {
				return nil, fmt.Errorf("failed to get project clusters: %w", err)
			}
			return clusters, nil
		})
		if !permitted {
			return false
		}
		nodes = append(nodes, child)
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("failed to iterate resource hierarchy v2: %w", err)
	}
	ts.AddCheckpoint("process_managed_resources_ms")
	orphanedNodes := make([]appv1.ResourceNode, 0)
	orphanedNodesKeys := make([]kube.ResourceKey, 0)
	for k := range orphanedNodesMap {
		if k.Namespace != "" && proj.IsGroupKindPermitted(k.GroupKind(), true) && !isKnownOrphanedResourceExclusion(k, proj) {
			orphanedNodesKeys = append(orphanedNodesKeys, k)
		}
	}
	err = ctrl.stateCache.IterateHierarchyV2(destCluster, orphanedNodesKeys, func(child appv1.ResourceNode, appName string) bool {
		belongToAnotherApp := false
		if appName != "" {
			appKey := ctrl.toAppKey(appName)
			if _, exists, err := ctrl.appInformer.GetIndexer().GetByKey(appKey); exists && err == nil {
				belongToAnotherApp = true
			}
		}

		if belongToAnotherApp {
			return false
		}

		permitted, _ := proj.IsResourcePermitted(schema.GroupKind{Group: child.Group, Kind: child.Kind}, child.Namespace, destCluster, func(project string) ([]*appv1.Cluster, error) {
			return ctrl.db.GetProjectClusters(context.TODO(), project)
		})

		if !permitted {
			return false
		}
		orphanedNodes = append(orphanedNodes, child)
		return true
	})
	if err != nil {
		return nil, err
	}

	var conditions []appv1.ApplicationCondition
	if len(orphanedNodes) > 0 && warnOrphaned {
		conditions = []appv1.ApplicationCondition{{
			Type:    appv1.ApplicationConditionOrphanedResourceWarning,
			Message: fmt.Sprintf("Application has %d orphaned resources", len(orphanedNodes)),
		}}
	}
	ctrl.metricsServer.SetOrphanedResourcesMetric(a, len(orphanedNodes))
	a.Status.SetConditions(conditions, map[appv1.ApplicationConditionType]bool{appv1.ApplicationConditionOrphanedResourceWarning: true})
	sort.Slice(orphanedNodes, func(i, j int) bool {
		return orphanedNodes[i].ResourceRef.String() < orphanedNodes[j].ResourceRef.String()
	})
	ts.AddCheckpoint("process_orphaned_resources_ms")

	hosts, err := ctrl.getAppHosts(destCluster, a, nodes)
	if err != nil {
		return nil, fmt.Errorf("failed to get app hosts: %w", err)
	}
	ts.AddCheckpoint("get_app_hosts_ms")
	return &appv1.ApplicationTree{Nodes: nodes, OrphanedNodes: orphanedNodes, Hosts: hosts}, nil
}

func (ctrl *ApplicationController) getAppHosts(destCluster *appv1.Cluster, a *appv1.Application, appNodes []appv1.ResourceNode) ([]appv1.HostInfo, error) {
	ts := stats.NewTimingStats()
	defer func() {
		logCtx := log.WithFields(applog.GetAppLogFields(a))
		for k, v := range ts.Timings() {
			logCtx = logCtx.WithField(k, v.Milliseconds())
		}
		logCtx = logCtx.WithField("time_ms", time.Since(ts.StartTime).Milliseconds())
		logCtx.Debug("Finished getting app hosts")
	}()
	supportedResourceNames := map[corev1.ResourceName]bool{
		corev1.ResourceCPU:     true,
		corev1.ResourceStorage: true,
		corev1.ResourceMemory:  true,
	}
	appPods := map[kube.ResourceKey]bool{}
	for _, node := range appNodes {
		if node.Group == "" && node.Kind == kube.PodKind {
			appPods[kube.NewResourceKey(node.Group, node.Kind, node.Namespace, node.Name)] = true
		}
	}

	allNodesInfo := map[string]statecache.NodeInfo{}
	allPodsByNode := map[string][]statecache.PodInfo{}
	appPodsByNode := map[string][]statecache.PodInfo{}
	err := ctrl.stateCache.IterateResources(destCluster, func(res *clustercache.Resource, info *statecache.ResourceInfo) {
		key := res.ResourceKey()

		switch {
		case info.NodeInfo != nil && key.Group == "" && key.Kind == "Node":
			allNodesInfo[key.Name] = *info.NodeInfo
		case info.PodInfo != nil && key.Group == "" && key.Kind == kube.PodKind:
			if appPods[key] {
				appPodsByNode[info.PodInfo.NodeName] = append(appPodsByNode[info.PodInfo.NodeName], *info.PodInfo)
			} else {
				allPodsByNode[info.PodInfo.NodeName] = append(allPodsByNode[info.PodInfo.NodeName], *info.PodInfo)
			}
		}
	})
	ts.AddCheckpoint("iterate_resources_ms")
	if err != nil {
		return nil, err
	}

	var hosts []appv1.HostInfo
	for nodeName, appPods := range appPodsByNode {
		node, ok := allNodesInfo[nodeName]
		if !ok {
			continue
		}

		neighbors := allPodsByNode[nodeName]

		resources := map[corev1.ResourceName]appv1.HostResourceInfo{}
		for name, resource := range node.Capacity {
			info := resources[name]
			info.ResourceName = name
			info.Capacity += resource.MilliValue()
			resources[name] = info
		}

		for _, pod := range appPods {
			for name, resource := range pod.ResourceRequests {
				if !supportedResourceNames[name] {
					continue
				}

				info := resources[name]
				info.RequestedByApp += resource.MilliValue()
				resources[name] = info
			}
		}

		for _, pod := range neighbors {
			for name, resource := range pod.ResourceRequests {
				if !supportedResourceNames[name] || pod.Phase == corev1.PodSucceeded || pod.Phase == corev1.PodFailed {
					continue
				}
				info := resources[name]
				info.RequestedByNeighbors += resource.MilliValue()
				resources[name] = info
			}
		}

		var resourcesInfo []appv1.HostResourceInfo
		for _, info := range resources {
			if supportedResourceNames[info.ResourceName] && info.Capacity > 0 {
				resourcesInfo = append(resourcesInfo, info)
			}
		}
		sort.Slice(resourcesInfo, func(i, j int) bool {
			return resourcesInfo[i].ResourceName < resourcesInfo[j].ResourceName
		})

		allowedNodeLabels := ctrl.settingsMgr.GetAllowedNodeLabels()
		nodeLabels := make(map[string]string)
		for _, label := range allowedNodeLabels {
			if val, ok := node.Labels[label]; ok {
				nodeLabels[label] = val
			}
		}

		hosts = append(hosts, appv1.HostInfo{Name: nodeName, SystemInfo: node.SystemInfo, ResourcesInfo: resourcesInfo, Labels: nodeLabels})
	}
	ts.AddCheckpoint("process_app_pods_by_node_ms")
	return hosts, nil
}

func (ctrl *ApplicationController) hideSecretData(destCluster *appv1.Cluster, app *appv1.Application, comparisonResult *comparisonResult) ([]*appv1.ResourceDiff, error) {
	items := make([]*appv1.ResourceDiff, len(comparisonResult.managedResources))
	for i := range comparisonResult.managedResources {
		res := comparisonResult.managedResources[i]
		item := appv1.ResourceDiff{
			Namespace:       res.Namespace,
			Name:            res.Name,
			Group:           res.Group,
			Kind:            res.Kind,
			Hook:            res.Hook,
			ResourceVersion: res.ResourceVersion,
		}

		target := res.Target
		live := res.Live
		resDiff := res.Diff
		if res.Kind == kube.SecretKind && res.Group == "" {
			var err error
			target, live, err = diff.HideSecretData(res.Target, res.Live, ctrl.settingsMgr.GetSensitiveAnnotations())
			if err != nil {
				return nil, fmt.Errorf("error hiding secret data: %w", err)
			}
			compareOptions, err := ctrl.settingsMgr.GetResourceCompareOptions()
			if err != nil {
				return nil, fmt.Errorf("error getting resource compare options: %w", err)
			}
			resourceOverrides, err := ctrl.settingsMgr.GetResourceOverrides()
			if err != nil {
				return nil, fmt.Errorf("error getting resource overrides: %w", err)
			}
			appLabelKey, err := ctrl.settingsMgr.GetAppInstanceLabelKey()
			if err != nil {
				return nil, fmt.Errorf("error getting app instance label key: %w", err)
			}
			trackingMethod, err := ctrl.settingsMgr.GetTrackingMethod()
			if err != nil {
				return nil, fmt.Errorf("error getting tracking method: %w", err)
			}

			clusterCache, err := ctrl.stateCache.GetClusterCache(destCluster)
			if err != nil {
				return nil, fmt.Errorf("error getting cluster cache: %w", err)
			}
			diffConfig, err := argodiff.NewDiffConfigBuilder().
				WithDiffSettings(app.Spec.IgnoreDifferences, resourceOverrides, compareOptions.IgnoreAggregatedRoles, ctrl.ignoreNormalizerOpts).
				WithTracking(appLabelKey, trackingMethod).
				WithNoCache().
				WithLogger(logutils.NewLogrusLogger(logutils.NewWithCurrentConfig())).
				WithGVKParser(clusterCache.GetGVKParser()).
				Build()
			if err != nil {
				return nil, fmt.Errorf("appcontroller error building diff config: %w", err)
			}

			diffResult, err := argodiff.StateDiff(live, target, diffConfig)
			if err != nil {
				return nil, fmt.Errorf("error applying diff: %w", err)
			}
			resDiff = diffResult
		}

		if live != nil {
			data, err := json.Marshal(live)
			if err != nil {
				return nil, fmt.Errorf("error marshaling live json: %w", err)
			}
			item.LiveState = string(data)
		} else {
			item.LiveState = "null"
		}

		if target != nil {
			data, err := json.Marshal(target)
			if err != nil {
				return nil, fmt.Errorf("error marshaling target json: %w", err)
			}
			item.TargetState = string(data)
		} else {
			item.TargetState = "null"
		}
		item.PredictedLiveState = string(resDiff.PredictedLive)
		item.NormalizedLiveState = string(resDiff.NormalizedLive)
		item.Modified = resDiff.Modified

		items[i] = &item
	}
	return items, nil
}

// Run starts the Application CRD controller.
func (ctrl *ApplicationController) Run(ctx context.Context, statusProcessors int, operationProcessors int) {
	defer runtime.HandleCrash()
	defer ctrl.appRefreshQueue.ShutDown()
	defer ctrl.appComparisonTypeRefreshQueue.ShutDown()
	defer ctrl.appOperationQueue.ShutDown()
	defer ctrl.projectRefreshQueue.ShutDown()
	defer ctrl.appHydrateQueue.ShutDown()
	defer ctrl.hydrationQueue.ShutDown()

	ctrl.RegisterClusterSecretUpdater(ctx)
	ctrl.metricsServer.RegisterClustersInfoSource(ctx, ctrl.stateCache, ctrl.db, ctrl.metricsClusterLabels)

	if ctrl.dynamicClusterDistributionEnabled {
		// only start deployment informer if dynamic distribution is enabled
		go ctrl.deploymentInformer.Informer().Run(ctx.Done())
	}

	clusters, err := ctrl.db.ListClusters(ctx)
	if err != nil {
		log.Warnf("Cannot init sharding. Error while querying clusters list from database: %v", err)
	} else {
		appItems, err := ctrl.getAppList(metav1.ListOptions{})

		if err != nil {
			log.Warnf("Cannot init sharding. Error while querying application list from database: %v", err)
		} else {
			ctrl.clusterSharding.Init(clusters, appItems)
		}
	}

	go ctrl.appInformer.Run(ctx.Done())
	go ctrl.projInformer.Run(ctx.Done())

	errors.CheckError(ctrl.stateCache.Init())

	if !cache.WaitForCacheSync(ctx.Done(), ctrl.appInformer.HasSynced, ctrl.projInformer.HasSynced) {
		log.Error("Timed out waiting for caches to sync")
		return
	}

	go func() { errors.CheckError(ctrl.stateCache.Run(ctx)) }()
	go func() { errors.CheckError(ctrl.metricsServer.ListenAndServe()) }()

	for i := 0; i < statusProcessors; i++ {
		go wait.Until(func() {
			for ctrl.processAppRefreshQueueItem() {
			}
		}, time.Second, ctx.Done())
	}

	for i := 0; i < operationProcessors; i++ {
		go wait.Until(func() {
			for ctrl.processAppOperationQueueItem() {
			}
		}, time.Second, ctx.Done())
	}

	go wait.Until(func() {
		for ctrl.processAppComparisonTypeQueueItem() {
		}
	}, time.Second, ctx.Done())

	go wait.Until(func() {
		for ctrl.processProjectQueueItem() {
		}
	}, time.Second, ctx.Done())

	if ctrl.hydrator != nil {
		go wait.Until(func() {
			for ctrl.processAppHydrateQueueItem() {
			}
		}, time.Second, ctx.Done())

		go wait.Until(func() {
			for ctrl.processHydrationQueueItem() {
			}
		}, time.Second, ctx.Done())
	}

	<-ctx.Done()
}

// requestAppRefresh adds a request for given app to the refresh queue. appName
// needs to be the qualified name of the application, i.e. <namespace>/<name>.
func (ctrl *ApplicationController) requestAppRefresh(appName string, compareWith *CompareWith, after *time.Duration) {
	key := ctrl.toAppKey(appName)

	if compareWith != nil && after != nil {
		ctrl.appComparisonTypeRefreshQueue.AddAfter(fmt.Sprintf("%s/%d", key, *compareWith), *after)
	} else {
		if compareWith != nil {
			ctrl.refreshRequestedAppsMutex.Lock()
			ctrl.refreshRequestedApps[key] = compareWith.Max(ctrl.refreshRequestedApps[key])
			ctrl.refreshRequestedAppsMutex.Unlock()
		}
		if after != nil {
			ctrl.appRefreshQueue.AddAfter(key, *after)
		} else {
			ctrl.appRefreshQueue.AddRateLimited(key)
		}
	}
}

func (ctrl *ApplicationController) isRefreshRequested(appName string) (bool, CompareWith) {
	ctrl.refreshRequestedAppsMutex.Lock()
	defer ctrl.refreshRequestedAppsMutex.Unlock()
	level, ok := ctrl.refreshRequestedApps[appName]
	if ok {
		delete(ctrl.refreshRequestedApps, appName)
	}
	return ok, level
}

func (ctrl *ApplicationController) processAppOperationQueueItem() (processNext bool) {
	appKey, shutdown := ctrl.appOperationQueue.Get()
	if shutdown {
		processNext = false
		return
	}
	processNext = true
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("Recovered from panic: %+v\n%s", r, debug.Stack())
		}
		ctrl.appOperationQueue.Done(appKey)
	}()

	obj, exists, err := ctrl.appInformer.GetIndexer().GetByKey(appKey)
	if err != nil {
		log.Errorf("Failed to get application '%s' from informer index: %+v", appKey, err)
		return
	}
	if !exists {
		// This happens after app was deleted, but the work queue still had an entry for it.
		return
	}
	origApp, ok := obj.(*appv1.Application)
	if !ok {
		log.Warnf("Key '%s' in index is not an application", appKey)
		return
	}
	app := origApp.DeepCopy()
	logCtx := log.WithFields(applog.GetAppLogFields(app))
	ts := stats.NewTimingStats()
	defer func() {
		for k, v := range ts.Timings() {
			logCtx = logCtx.WithField(k, v.Milliseconds())
		}
		logCtx = logCtx.WithField("time_ms", time.Since(ts.StartTime).Milliseconds())
		logCtx.Debug("Finished processing app operation queue item")
	}()

	if app.Operation != nil {
		// If we get here, we are about to process an operation, but we cannot rely on informer since it might have stale data.
		// So always retrieve the latest version to ensure it is not stale to avoid unnecessary syncing.
		// We cannot rely on informer since applications might be updated by both application controller and api server.
		freshApp, err := ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.ObjectMeta.Namespace).Get(context.Background(), app.Name, metav1.GetOptions{})
		if err != nil {
			logCtx.Errorf("Failed to retrieve latest application state: %v", err)
			return
		}
		app = freshApp
	}
	ts.AddCheckpoint("get_fresh_app_ms")

	if app.Operation != nil {
		ctrl.processRequestedAppOperation(app)
		ts.AddCheckpoint("process_requested_app_operation_ms")
	} else if app.DeletionTimestamp != nil {
		if err = ctrl.finalizeApplicationDeletion(app, func(project string) ([]*appv1.Cluster, error) {
			return ctrl.db.GetProjectClusters(context.Background(), project)
		}); err != nil {
			ctrl.setAppCondition(app, appv1.ApplicationCondition{
				Type:    appv1.ApplicationConditionDeletionError,
				Message: err.Error(),
			})
			message := fmt.Sprintf("Unable to delete application resources: %v", err.Error())
			ctrl.logAppEvent(context.TODO(), app, argo.EventInfo{Reason: argo.EventReasonStatusRefreshed, Type: corev1.EventTypeWarning}, message)
		}
		ts.AddCheckpoint("finalize_application_deletion_ms")
	}
	return
}

func (ctrl *ApplicationController) processAppComparisonTypeQueueItem() (processNext bool) {
	key, shutdown := ctrl.appComparisonTypeRefreshQueue.Get()
	processNext = true

	defer func() {
		if r := recover(); r != nil {
			log.Errorf("Recovered from panic: %+v\n%s", r, debug.Stack())
		}
		ctrl.appComparisonTypeRefreshQueue.Done(key)
	}()
	if shutdown {
		processNext = false
		return
	}

	if parts := strings.Split(key, "/"); len(parts) != 3 {
		log.Warnf("Unexpected key format in appComparisonTypeRefreshTypeQueue. Key should consists of namespace/name/comparisonType but got: %s", key)
	} else {
		compareWith, err := strconv.Atoi(parts[2])
		if err != nil {
			log.Warnf("Unable to parse comparison type: %v", err)
			return
		}
		ctrl.requestAppRefresh(ctrl.toAppQualifiedName(parts[1], parts[0]), CompareWith(compareWith).Pointer(), nil)
	}
	return
}

func (ctrl *ApplicationController) processProjectQueueItem() (processNext bool) {
	key, shutdown := ctrl.projectRefreshQueue.Get()
	processNext = true

	defer func() {
		if r := recover(); r != nil {
			log.Errorf("Recovered from panic: %+v\n%s", r, debug.Stack())
		}
		ctrl.projectRefreshQueue.Done(key)
	}()
	if shutdown {
		processNext = false
		return
	}
	obj, exists, err := ctrl.projInformer.GetIndexer().GetByKey(key)
	if err != nil {
		log.Errorf("Failed to get project '%s' from informer index: %+v", key, err)
		return
	}
	if !exists {
		// This happens after appproj was deleted, but the work queue still had an entry for it.
		return
	}
	origProj, ok := obj.(*appv1.AppProject)
	if !ok {
		log.Warnf("Key '%s' in index is not an appproject", key)
		return
	}

	if origProj.DeletionTimestamp != nil && origProj.HasFinalizer() {
		if err := ctrl.finalizeProjectDeletion(origProj.DeepCopy()); err != nil {
			log.Warnf("Failed to finalize project deletion: %v", err)
		}
	}
	return
}

func (ctrl *ApplicationController) finalizeProjectDeletion(proj *appv1.AppProject) error {
	apps, err := ctrl.appLister.Applications(ctrl.namespace).List(labels.Everything())
	if err != nil {
		return fmt.Errorf("error listing applications: %w", err)
	}
	appsCount := 0
	for i := range apps {
		if apps[i].Spec.GetProject() == proj.Name {
			appsCount++
		}
	}
	if appsCount == 0 {
		return ctrl.removeProjectFinalizer(proj)
	}
	log.Infof("Cannot remove project '%s' finalizer as is referenced by %d applications", proj.Name, appsCount)
	return nil
}

func (ctrl *ApplicationController) removeProjectFinalizer(proj *appv1.AppProject) error {
	proj.RemoveFinalizer()
	var patch []byte
	patch, _ = json.Marshal(map[string]any{
		"metadata": map[string]any{
			"finalizers": proj.Finalizers,
		},
	})
	_, err := ctrl.applicationClientset.ArgoprojV1alpha1().AppProjects(ctrl.namespace).Patch(context.Background(), proj.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// shouldBeDeleted returns whether a given resource obj should be deleted on cascade delete of application app
func (ctrl *ApplicationController) shouldBeDeleted(app *appv1.Application, obj *unstructured.Unstructured) bool {
	return !kube.IsCRD(obj) && !isSelfReferencedApp(app, kube.GetObjectRef(obj)) &&
		!resourceutil.HasAnnotationOption(obj, synccommon.AnnotationSyncOptions, synccommon.SyncOptionDisableDeletion) &&
		!resourceutil.HasAnnotationOption(obj, helm.ResourcePolicyAnnotation, helm.ResourcePolicyKeep)
}

func (ctrl *ApplicationController) getPermittedAppLiveObjects(destCluster *appv1.Cluster, app *appv1.Application, proj *appv1.AppProject, projectClusters func(project string) ([]*appv1.Cluster, error)) (map[kube.ResourceKey]*unstructured.Unstructured, error) {
	objsMap, err := ctrl.stateCache.GetManagedLiveObjs(destCluster, app, []*unstructured.Unstructured{})
	if err != nil {
		return nil, err
	}
	// Don't delete live resources which are not permitted in the app project
	for k, v := range objsMap {
		permitted, err := proj.IsLiveResourcePermitted(v, destCluster, projectClusters)
		if err != nil {
			return nil, err
		}

		if !permitted {
			delete(objsMap, k)
		}
	}
	return objsMap, nil
}

func (ctrl *ApplicationController) finalizeApplicationDeletion(app *appv1.Application, projectClusters func(project string) ([]*appv1.Cluster, error)) error {
	logCtx := log.WithFields(applog.GetAppLogFields(app))
	// Get refreshed application info, since informer app copy might be stale
	app, err := ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(context.Background(), app.Name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			logCtx.Errorf("Unable to get refreshed application info prior deleting resources: %v", err)
		}
		return nil
	}
	proj, err := ctrl.getAppProj(app)
	if err != nil {
		return err
	}
	destCluster, err := argo.GetDestinationCluster(context.Background(), app.Spec.Destination, ctrl.db)
	if err != nil {
		logCtx.Warnf("Unable to get destination cluster: %v", err)
		app.UnSetCascadedDeletion()
		app.UnSetPostDeleteFinalizer()
		if err := ctrl.updateFinalizers(app); err != nil {
			return err
		}
		logCtx.Infof("Resource entries removed from undefined cluster")
		return nil
	}
	clusterRESTConfig, err := destCluster.RESTConfig()
	if err != nil {
		return err
	}
	config := metrics.AddMetricsTransportWrapper(ctrl.metricsServer, app, clusterRESTConfig)

	if app.CascadedDeletion() {
		deletionApproved := app.IsDeletionConfirmed(app.DeletionTimestamp.Time)

		logCtx.Infof("Deleting resources")
		// ApplicationDestination points to a valid cluster, so we may clean up the live objects
		objs := make([]*unstructured.Unstructured, 0)
		objsMap, err := ctrl.getPermittedAppLiveObjects(destCluster, app, proj, projectClusters)
		if err != nil {
			return err
		}

		for k := range objsMap {
			// Wait for objects pending deletion to complete before proceeding with next sync wave
			if objsMap[k].GetDeletionTimestamp() != nil {
				logCtx.Infof("%d objects remaining for deletion", len(objsMap))
				return nil
			}

			if ctrl.shouldBeDeleted(app, objsMap[k]) {
				objs = append(objs, objsMap[k])
				if res, ok := app.Status.FindResource(k); ok && res.RequiresDeletionConfirmation && !deletionApproved {
					logCtx.Infof("Resource %v requires manual confirmation to delete", k)
					return nil
				}
			}
		}

		filteredObjs := FilterObjectsForDeletion(objs)

		propagationPolicy := metav1.DeletePropagationForeground
		if app.GetPropagationPolicy() == appv1.BackgroundPropagationPolicyFinalizer {
			propagationPolicy = metav1.DeletePropagationBackground
		}
		logCtx.Infof("Deleting application's resources with %s propagation policy", propagationPolicy)

		err = kube.RunAllAsync(len(filteredObjs), func(i int) error {
			obj := filteredObjs[i]
			return ctrl.kubectl.DeleteResource(context.Background(), config, obj.GroupVersionKind(), obj.GetName(), obj.GetNamespace(), metav1.DeleteOptions{PropagationPolicy: &propagationPolicy})
		})
		if err != nil {
			return err
		}

		objsMap, err = ctrl.getPermittedAppLiveObjects(destCluster, app, proj, projectClusters)
		if err != nil {
			return err
		}

		for k, obj := range objsMap {
			if !ctrl.shouldBeDeleted(app, obj) {
				delete(objsMap, k)
			}
		}
		if len(objsMap) > 0 {
			logCtx.Infof("%d objects remaining for deletion", len(objsMap))
			return nil
		}
		logCtx.Infof("Successfully deleted %d resources", len(objs))
		app.UnSetCascadedDeletion()
		return ctrl.updateFinalizers(app)
	}

	if app.HasPostDeleteFinalizer() {
		objsMap, err := ctrl.getPermittedAppLiveObjects(destCluster, app, proj, projectClusters)
		if err != nil {
			return err
		}

		done, err := ctrl.executePostDeleteHooks(app, proj, objsMap, config, logCtx)
		if err != nil {
			return err
		}
		if !done {
			return nil
		}
		app.UnSetPostDeleteFinalizer()
		return ctrl.updateFinalizers(app)
	}

	if app.HasPostDeleteFinalizer("cleanup") {
		objsMap, err := ctrl.getPermittedAppLiveObjects(destCluster, app, proj, projectClusters)
		if err != nil {
			return err
		}

		done, err := ctrl.cleanupPostDeleteHooks(objsMap, config, logCtx)
		if err != nil {
			return err
		}
		if !done {
			return nil
		}
		app.UnSetPostDeleteFinalizer("cleanup")
		return ctrl.updateFinalizers(app)
	}

	if !app.CascadedDeletion() && !app.HasPostDeleteFinalizer() {
		if err := ctrl.cache.SetAppManagedResources(app.Name, nil); err != nil {
			return err
		}

		if err := ctrl.cache.SetAppResourcesTree(app.Name, nil); err != nil {
			return err
		}
		ctrl.projectRefreshQueue.Add(fmt.Sprintf("%s/%s", ctrl.namespace, app.Spec.GetProject()))
	}

	return nil
}

func (ctrl *ApplicationController) updateFinalizers(app *appv1.Application) error {
	_, err := ctrl.getAppProj(app)
	if err != nil {
		return fmt.Errorf("error getting project: %w", err)
	}

	var patch []byte
	patch, _ = json.Marshal(map[string]any{
		"metadata": map[string]any{
			"finalizers": app.Finalizers,
		},
	})

	_, err = ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Patch(context.Background(), app.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func (ctrl *ApplicationController) setAppCondition(app *appv1.Application, condition appv1.ApplicationCondition) {
	logCtx := log.WithFields(applog.GetAppLogFields(app))
	// do nothing if app already has same condition
	for _, c := range app.Status.Conditions {
		if c.Message == condition.Message && c.Type == condition.Type {
			return
		}
	}

	app.Status.SetConditions([]appv1.ApplicationCondition{condition}, map[appv1.ApplicationConditionType]bool{condition.Type: true})

	var patch []byte
	patch, err := json.Marshal(map[string]any{
		"status": map[string]any{
			"conditions": app.Status.Conditions,
		},
	})
	if err == nil {
		_, err = ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Patch(context.Background(), app.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	}
	if err != nil {
		logCtx.Errorf("Unable to set application condition: %v", err)
	}
}

func (ctrl *ApplicationController) processRequestedAppOperation(app *appv1.Application) {
	logCtx := log.WithFields(applog.GetAppLogFields(app))
	var state *appv1.OperationState
	// Recover from any unexpected panics and automatically set the status to be failed
	defer func() {
		if r := recover(); r != nil {
			logCtx.Errorf("Recovered from panic: %+v\n%s", r, debug.Stack())
			state.Phase = synccommon.OperationError
			if rerr, ok := r.(error); ok {
				state.Message = rerr.Error()
			} else {
				state.Message = fmt.Sprintf("%v", r)
			}
			ctrl.setOperationState(app, state)
		}
	}()
	ts := stats.NewTimingStats()
	defer func() {
		for k, v := range ts.Timings() {
			logCtx = logCtx.WithField(k, v.Milliseconds())
		}
		logCtx = logCtx.WithField("time_ms", time.Since(ts.StartTime).Milliseconds())
		logCtx.Debug("Finished processing requested app operation")
	}()
	terminating := false
	if isOperationInProgress(app) {
		state = app.Status.OperationState.DeepCopy()
		terminating = state.Phase == synccommon.OperationTerminating
		switch {
		case state.FinishedAt != nil && !terminating:
			// Failed operation with retry strategy might be in-progress and has completion time
			retryAt, err := app.Status.OperationState.Operation.Retry.NextRetryAt(state.FinishedAt.Time, state.RetryCount)
			if err != nil {
				state.Phase = synccommon.OperationError
				state.Message = err.Error()
				ctrl.setOperationState(app, state)
				return
			}
			retryAfter := time.Until(retryAt)
			if retryAfter > 0 {
				logCtx.Infof("Skipping retrying in-progress operation. Attempting again at: %s", retryAt.Format(time.RFC3339))
				ctrl.requestAppRefresh(app.QualifiedName(), CompareWithLatest.Pointer(), &retryAfter)
				return
			}
			// Get rid of sync results and null out previous operation completion time
			// This will start the retry attempt
			state.FinishedAt = nil
			state.SyncResult = nil
			ctrl.setOperationState(app, state)
		case ctrl.syncTimeout != time.Duration(0) && time.Now().After(state.StartedAt.Add(ctrl.syncTimeout)) && !terminating:
			state.Phase = synccommon.OperationTerminating
			state.Message = "operation is terminating due to timeout"
			ctrl.setOperationState(app, state)
			logCtx.Infof("Terminating in-progress operation due to timeout. Started at: %v, timeout: %v", state.StartedAt, ctrl.syncTimeout)
		default:
			logCtx.Infof("Resuming in-progress operation. phase: %s, message: %s", state.Phase, state.Message)
		}
	} else {
		state = NewOperationState(*app.Operation)
		ctrl.setOperationState(app, state)
		if ctrl.syncTimeout != time.Duration(0) {
			// Schedule a check during which the timeout would be checked.
			ctrl.appOperationQueue.AddAfter(ctrl.toAppKey(app.QualifiedName()), ctrl.syncTimeout)
		}
		logCtx.Infof("Initialized new operation: %v", *app.Operation)
	}
	ts.AddCheckpoint("initial_operation_stage_ms")

	project, err := ctrl.getAppProj(app)
	if err == nil {
		// Start or resume the sync
		ctrl.appStateManager.SyncAppState(app, project, state)
	} else {
		state.Phase = synccommon.OperationError
		state.Message = fmt.Sprintf("Failed to load application project: %v", err)
	}
	ts.AddCheckpoint("sync_app_state_ms")

	switch state.Phase {
	case synccommon.OperationRunning:
		// It's possible for an app to be terminated while we were operating on it. We do not want
		// to clobber the Terminated state with Running. Get the latest app state to check for this.
		freshApp, err := ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(context.Background(), app.Name, metav1.GetOptions{})
		if err == nil {
			if freshApp.Status.OperationState != nil && freshApp.Status.OperationState.Phase == synccommon.OperationTerminating {
				state.Phase = synccommon.OperationTerminating
				state.Message = "operation is terminating"
				// after this, we will get requeued to the workqueue, but next time the
				// SyncAppState will operate in a Terminating phase, allowing the worker to perform
				// cleanup (e.g. delete jobs, workflows, etc...)
			}
		}
	case synccommon.OperationFailed, synccommon.OperationError:
		if !terminating && (state.RetryCount < state.Operation.Retry.Limit || state.Operation.Retry.Limit < 0) {
			now := metav1.Now()
			state.FinishedAt = &now
			if retryAt, err := state.Operation.Retry.NextRetryAt(now.Time, state.RetryCount); err != nil {
				state.Phase = synccommon.OperationError
				state.Message = fmt.Sprintf("%s (failed to retry: %v)", state.Message, err)
			} else {
				state.Phase = synccommon.OperationRunning
				state.RetryCount++
				state.Message = fmt.Sprintf("%s due to application controller sync timeout. Retrying attempt #%d at %s.", state.Message, state.RetryCount, retryAt.Format(time.Kitchen))
			}
		} else if state.RetryCount > 0 {
			state.Message = fmt.Sprintf("%s (retried %d times).", state.Message, state.RetryCount)
		}
	}

	ctrl.setOperationState(app, state)
	ts.AddCheckpoint("final_set_operation_state")
	if state.Phase.Completed() && (app.Operation.Sync != nil && !app.Operation.Sync.DryRun) {
		// if we just completed an operation, force a refresh so that UI will report up-to-date
		// sync/health information
		if _, err := cache.MetaNamespaceKeyFunc(app); err == nil {
			// force app refresh with using CompareWithLatest comparison type and trigger app reconciliation loop
			ctrl.requestAppRefresh(app.QualifiedName(), CompareWithLatestForceResolve.Pointer(), nil)
		} else {
			logCtx.Warnf("Fails to requeue application: %v", err)
		}
	}
	ts.AddCheckpoint("request_app_refresh_ms")
}

func (ctrl *ApplicationController) setOperationState(app *appv1.Application, state *appv1.OperationState) {
	logCtx := log.WithFields(applog.GetAppLogFields(app))
	if state.Phase == "" {
		// expose any bugs where we neglect to set phase
		panic("no phase was set")
	}
	if state.Phase.Completed() {
		now := metav1.Now()
		state.FinishedAt = &now
	}
	patch := map[string]any{
		"status": map[string]any{
			"operationState": state,
		},
	}
	if state.Phase.Completed() {
		// If operation is completed, clear the operation field to indicate no operation is
		// in progress.
		patch["operation"] = nil
	}
	if reflect.DeepEqual(app.Status.OperationState, state) {
		logCtx.Infof("No operation updates necessary to '%s'. Skipping patch", app.QualifiedName())
		return
	}
	patchJSON, err := json.Marshal(patch)
	if err != nil {
		logCtx.Errorf("error marshaling json: %v", err)
		return
	}
	if app.Status.OperationState != nil && app.Status.OperationState.FinishedAt != nil && state.FinishedAt == nil {
		patchJSON, err = jsonpatch.MergeMergePatches(patchJSON, []byte(`{"status": {"operationState": {"finishedAt": null}}}`))
		if err != nil {
			logCtx.Errorf("error merging operation state patch: %v", err)
			return
		}
	}

	kube.RetryUntilSucceed(context.Background(), updateOperationStateTimeout, "Update application operation state", logutils.NewLogrusLogger(logutils.NewWithCurrentConfig()), func() error {
		_, err := ctrl.PatchAppWithWriteBack(context.Background(), app.Name, app.Namespace, types.MergePatchType, patchJSON, metav1.PatchOptions{})
		if err != nil {
			// Stop retrying updating deleted application
			if apierrors.IsNotFound(err) {
				return nil
			}
			// kube.RetryUntilSucceed logs failed attempts at "debug" level, but we want to know if this fails. Log a
			// warning.
			logCtx.Warnf("error patching application with operation state: %v", err)
			return fmt.Errorf("error patching application with operation state: %w", err)
		}
		return nil
	})

	logCtx.Infof("updated '%s' operation (phase: %s)", app.QualifiedName(), state.Phase)
	if state.Phase.Completed() {
		eventInfo := argo.EventInfo{Reason: argo.EventReasonOperationCompleted}
		var messages []string
		if state.Operation.Sync != nil && len(state.Operation.Sync.Resources) > 0 {
			messages = []string{"Partial sync operation"}
		} else {
			messages = []string{"Sync operation"}
		}
		if state.SyncResult != nil {
			messages = append(messages, "to", state.SyncResult.Revision)
		}
		if state.Phase.Successful() {
			eventInfo.Type = corev1.EventTypeNormal
			messages = append(messages, "succeeded")
		} else {
			eventInfo.Type = corev1.EventTypeWarning
			messages = append(messages, "failed:", state.Message)
		}
		ctrl.logAppEvent(context.TODO(), app, eventInfo, strings.Join(messages, " "))

		destCluster, err := argo.GetDestinationCluster(context.Background(), app.Spec.Destination, ctrl.db)
		if err != nil {
			logCtx.Warnf("Unable to get destination cluster, setting dest_server label to empty string in sync metric: %v", err)
		}
		destServer := ""
		if destCluster != nil {
			destServer = destCluster.Server
		}
		ctrl.metricsServer.IncSync(app, destServer, state)
		ctrl.metricsServer.IncAppSyncDuration(app, destServer, state)
	}
}

// writeBackToInformer writes a just recently updated App back into the informer cache.
// This prevents the situation where the controller operates on a stale app and repeats work
func (ctrl *ApplicationController) writeBackToInformer(app *appv1.Application) {
	logCtx := log.WithFields(applog.GetAppLogFields(app)).WithField("informer-writeBack", true)
	err := ctrl.appInformer.GetStore().Update(app)
	if err != nil {
		logCtx.Errorf("failed to update informer store: %v", err)
		return
	}
}

// PatchAppWithWriteBack patches an application and writes it back to the informer cache
func (ctrl *ApplicationController) PatchAppWithWriteBack(ctx context.Context, name, ns string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (result *appv1.Application, err error) {
	patchedApp, err := ctrl.applicationClientset.ArgoprojV1alpha1().Applications(ns).Patch(ctx, name, pt, data, opts, subresources...)
	if err != nil {
		return patchedApp, err
	}
	ctrl.writeBackToInformer(patchedApp)
	return patchedApp, err
}

func (ctrl *ApplicationController) processAppRefreshQueueItem() (processNext bool) {
	patchDuration := time.Duration(0) // time spent in doing patch/update calls
	setOpDuration := time.Duration(0) // time spent in doing Operation patch calls in autosync
	appKey, shutdown := ctrl.appRefreshQueue.Get()
	if shutdown {
		processNext = false
		return
	}
	processNext = true
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("Recovered from panic: %+v\n%s", r, debug.Stack())
		}
		// We want to have app operation update happen after the sync, so there's no race condition
		// and app updates not proceeding. See https://github.com/argoproj/argo-cd/issues/18500.
		ctrl.appOperationQueue.AddRateLimited(appKey)
		ctrl.appRefreshQueue.Done(appKey)
	}()
	obj, exists, err := ctrl.appInformer.GetIndexer().GetByKey(appKey)
	if err != nil {
		log.Errorf("Failed to get application '%s' from informer index: %+v", appKey, err)
		return
	}
	if !exists {
		// This happens after app was deleted, but the work queue still had an entry for it.
		return
	}
	origApp, ok := obj.(*appv1.Application)
	if !ok {
		log.Warnf("Key '%s' in index is not an application", appKey)
		return
	}
	origApp = origApp.DeepCopy()
	needRefresh, refreshType, comparisonLevel := ctrl.needRefreshAppStatus(origApp, ctrl.statusRefreshTimeout, ctrl.statusHardRefreshTimeout)

	if !needRefresh {
		return
	}
	app := origApp.DeepCopy()
	logCtx := log.WithFields(applog.GetAppLogFields(app)).WithFields(log.Fields{
		"comparison-level": comparisonLevel,
		"dest-server":      origApp.Spec.Destination.Server,
		"dest-name":        origApp.Spec.Destination.Name,
		"dest-namespace":   origApp.Spec.Destination.Namespace,
	})

	startTime := time.Now()
	ts := stats.NewTimingStats()
	var destCluster *appv1.Cluster
	defer func() {
		reconcileDuration := time.Since(startTime)

		// We may or may not get to the point in the code where destCluster is set. Populate the dest_server label on a
		// best-effort basis.
		destServer := ""
		if destCluster != nil {
			destServer = destCluster.Server
		}
		ctrl.metricsServer.IncReconcile(origApp, destServer, reconcileDuration)
		for k, v := range ts.Timings() {
			logCtx = logCtx.WithField(k, v.Milliseconds())
		}
		logCtx.WithFields(log.Fields{
			"time_ms":  reconcileDuration.Milliseconds(),
			"patch_ms": patchDuration.Milliseconds(),
			"setop_ms": setOpDuration.Milliseconds(),
		}).Info("Reconciliation completed")
	}()

	if comparisonLevel == ComparisonWithNothing {
		// If the destination cluster is invalid, fallback to the normal reconciliation flow
		if destCluster, err = argo.GetDestinationCluster(context.Background(), app.Spec.Destination, ctrl.db); err == nil {
			managedResources := make([]*appv1.ResourceDiff, 0)
			if err := ctrl.cache.GetAppManagedResources(app.InstanceName(ctrl.namespace), &managedResources); err == nil {
				var tree *appv1.ApplicationTree
				if tree, err = ctrl.getResourceTree(destCluster, app, managedResources); err == nil {
					app.Status.Summary = tree.GetSummary(app)
					if err := ctrl.cache.SetAppResourcesTree(app.InstanceName(ctrl.namespace), tree); err != nil {
						logCtx.Errorf("Failed to cache resources tree: %v", err)
						return
					}
				}

				patchDuration = ctrl.persistAppStatus(origApp, &app.Status)
				return
			}
			logCtx.Warnf("Failed to get cached managed resources for tree reconciliation, fall back to full reconciliation")
		}
	}
	ts.AddCheckpoint("comparison_with_nothing_ms")

	project, hasErrors := ctrl.refreshAppConditions(app)
	ts.AddCheckpoint("refresh_app_conditions_ms")
	now := metav1.Now()
	if hasErrors {
		app.Status.Sync.Status = appv1.SyncStatusCodeUnknown
		app.Status.Health.Status = health.HealthStatusUnknown
		patchDuration = ctrl.persistAppStatus(origApp, &app.Status)

		if err := ctrl.cache.SetAppResourcesTree(app.InstanceName(ctrl.namespace), &appv1.ApplicationTree{}); err != nil {
			logCtx.Warnf("failed to set app resource tree: %v", err)
		}
		if err := ctrl.cache.SetAppManagedResources(app.InstanceName(ctrl.namespace), nil); err != nil {
			logCtx.Warnf("failed to set app managed resources tree: %v", err)
		}
		ts.AddCheckpoint("process_refresh_app_conditions_errors_ms")
		return
	}

	destCluster, err = argo.GetDestinationCluster(context.Background(), app.Spec.Destination, ctrl.db)
	if err != nil {
		logCtx.Errorf("Failed to get destination cluster: %v", err)
		// exit the reconciliation. ctrl.refreshAppConditions should have caught the error
		return
	}

	var localManifests []string
	if opState := app.Status.OperationState; opState != nil && opState.Operation.Sync != nil {
		localManifests = opState.Operation.Sync.Manifests
	}

	revisions := make([]string, 0)
	sources := make([]appv1.ApplicationSource, 0)

	hasMultipleSources := app.Spec.HasMultipleSources()

	// If we have multiple sources, we use all the sources under `sources` field and ignore source under `source` field.
	// else we use the source under the source field.
	if hasMultipleSources {
		for _, source := range app.Spec.Sources {
			// We do not perform any filtering of duplicate sources.
			// Argo CD will apply and update the resources generated from the sources automatically
			// based on the order in which manifests were generated
			sources = append(sources, source)
			revisions = append(revisions, source.TargetRevision)
		}
		if comparisonLevel == CompareWithRecent {
			revisions = app.Status.Sync.Revisions
		}
	} else {
		revision := app.Spec.GetSource().TargetRevision
		if comparisonLevel == CompareWithRecent {
			revision = app.Status.Sync.Revision
		}
		revisions = append(revisions, revision)
		sources = append(sources, app.Spec.GetSource())
	}

	compareResult, err := ctrl.appStateManager.CompareAppState(app, project, revisions, sources, refreshType == appv1.RefreshTypeHard, comparisonLevel == CompareWithLatestForceResolve, localManifests, hasMultipleSources)

	ts.AddCheckpoint("compare_app_state_ms")

	if stderrors.Is(err, ErrCompareStateRepo) {
		logCtx.Warnf("Ignoring temporary failed attempt to compare app state against repo: %v", err)
		return // short circuit if git error is encountered
	}

	for k, v := range compareResult.timings {
		logCtx = logCtx.WithField(k, v.Milliseconds())
	}

	ctrl.normalizeApplication(origApp, app)
	ts.AddCheckpoint("normalize_application_ms")

	tree, err := ctrl.setAppManagedResources(destCluster, app, compareResult)
	ts.AddCheckpoint("set_app_managed_resources_ms")
	if err != nil {
		logCtx.Errorf("Failed to cache app resources: %v", err)
	} else {
		app.Status.Summary = tree.GetSummary(app)
	}

	canSync, _ := project.Spec.SyncWindows.Matches(app).CanSync(false)
	if canSync {
		syncErrCond, opDuration := ctrl.autoSync(app, compareResult.syncStatus, compareResult.resources, compareResult.revisionsMayHaveChanges)
		setOpDuration = opDuration
		if syncErrCond != nil {
			app.Status.SetConditions(
				[]appv1.ApplicationCondition{*syncErrCond},
				map[appv1.ApplicationConditionType]bool{appv1.ApplicationConditionSyncError: true},
			)
		} else {
			app.Status.SetConditions(
				[]appv1.ApplicationCondition{},
				map[appv1.ApplicationConditionType]bool{appv1.ApplicationConditionSyncError: true},
			)
		}
	} else {
		logCtx.Info("Sync prevented by sync window")
	}
	ts.AddCheckpoint("auto_sync_ms")

	if app.Status.ReconciledAt == nil || comparisonLevel >= CompareWithLatest {
		app.Status.ReconciledAt = &now
	}
	app.Status.Sync = *compareResult.syncStatus
	app.Status.Health.Status = compareResult.healthStatus
	app.Status.Resources = compareResult.resources
	sort.Slice(app.Status.Resources, func(i, j int) bool {
		return resourceStatusKey(app.Status.Resources[i]) < resourceStatusKey(app.Status.Resources[j])
	})
	app.Status.SourceType = compareResult.appSourceType
	app.Status.SourceTypes = compareResult.appSourceTypes
	app.Status.ControllerNamespace = ctrl.namespace
	ts.AddCheckpoint("app_status_update_ms")
	patchDuration = ctrl.persistAppStatus(origApp, &app.Status)
	// This is a partly a duplicate of patch_ms, but more descriptive and allows to have measurement for the next step.
	ts.AddCheckpoint("persist_app_status_ms")
	if (compareResult.hasPostDeleteHooks != app.HasPostDeleteFinalizer() || compareResult.hasPostDeleteHooks != app.HasPostDeleteFinalizer("cleanup")) &&
		app.GetDeletionTimestamp() == nil {
		if compareResult.hasPostDeleteHooks {
			app.SetPostDeleteFinalizer()
			app.SetPostDeleteFinalizer("cleanup")
		} else {
			app.UnSetPostDeleteFinalizer()
			app.UnSetPostDeleteFinalizer("cleanup")
		}

		if err := ctrl.updateFinalizers(app); err != nil {
			logCtx.Errorf("Failed to update finalizers: %v", err)
		}
	}
	ts.AddCheckpoint("process_finalizers_ms")
	return
}

func (ctrl *ApplicationController) processAppHydrateQueueItem() (processNext bool) {
	appKey, shutdown := ctrl.appHydrateQueue.Get()
	if shutdown {
		processNext = false
		return
	}
	processNext = true
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("Recovered from panic: %+v\n%s", r, debug.Stack())
		}
		ctrl.appHydrateQueue.Done(appKey)
	}()
	obj, exists, err := ctrl.appInformer.GetIndexer().GetByKey(appKey)
	if err != nil {
		log.Errorf("Failed to get application '%s' from informer index: %+v", appKey, err)
		return
	}
	if !exists {
		// This happens after app was deleted, but the work queue still had an entry for it.
		return
	}
	origApp, ok := obj.(*appv1.Application)
	if !ok {
		log.Warnf("Key '%s' in index is not an application", appKey)
		return
	}

	ctrl.hydrator.ProcessAppHydrateQueueItem(origApp)

	log.WithFields(applog.GetAppLogFields(origApp)).Debug("Successfully processed app hydrate queue item")
	return
}

func (ctrl *ApplicationController) processHydrationQueueItem() (processNext bool) {
	hydrationKey, shutdown := ctrl.hydrationQueue.Get()
	if shutdown {
		processNext = false
		return
	}
	processNext = true
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("Recovered from panic: %+v\n%s", r, debug.Stack())
		}
		ctrl.hydrationQueue.Done(hydrationKey)
	}()

	logCtx := log.WithFields(log.Fields{
		"sourceRepoURL":        hydrationKey.SourceRepoURL,
		"sourceTargetRevision": hydrationKey.SourceTargetRevision,
		"destinationBranch":    hydrationKey.DestinationBranch,
	})

	logCtx.Debug("Processing hydration queue item")

	ctrl.hydrator.ProcessHydrationQueueItem(hydrationKey)

	logCtx.Debug("Successfully processed hydration queue item")
	return
}

func resourceStatusKey(res appv1.ResourceStatus) string {
	return strings.Join([]string{res.Group, res.Kind, res.Namespace, res.Name}, "/")
}

func currentSourceEqualsSyncedSource(app *appv1.Application) bool {
	if app.Spec.HasMultipleSources() {
		return app.Spec.Sources.Equals(app.Status.Sync.ComparedTo.Sources)
	}
	source := app.Spec.GetSource()
	return source.Equals(&app.Status.Sync.ComparedTo.Source)
}

// needRefreshAppStatus answers if application status needs to be refreshed.
// Returns true if application never been compared, has changed or comparison result has expired.
// Additionally, it returns whether full refresh was requested or not.
// If full refresh is requested then target and live state should be reconciled, else only live state tree should be updated.
func (ctrl *ApplicationController) needRefreshAppStatus(app *appv1.Application, statusRefreshTimeout, statusHardRefreshTimeout time.Duration) (bool, appv1.RefreshType, CompareWith) {
	logCtx := log.WithFields(applog.GetAppLogFields(app))
	var reason string
	compareWith := CompareWithLatest
	refreshType := appv1.RefreshTypeNormal

	softExpired := app.Status.ReconciledAt == nil || app.Status.ReconciledAt.Add(statusRefreshTimeout).Before(time.Now().UTC())
	hardExpired := (app.Status.ReconciledAt == nil || app.Status.ReconciledAt.Add(statusHardRefreshTimeout).Before(time.Now().UTC())) && statusHardRefreshTimeout.Seconds() != 0

	if requestedType, ok := app.IsRefreshRequested(); ok {
		compareWith = CompareWithLatestForceResolve
		// user requested app refresh.
		refreshType = requestedType
		reason = fmt.Sprintf("%s refresh requested", refreshType)
	} else {
		if !currentSourceEqualsSyncedSource(app) {
			reason = "spec.source differs"
			compareWith = CompareWithLatestForceResolve
			if app.Spec.HasMultipleSources() {
				reason = "at least one of the spec.sources differs"
			}
		} else if hardExpired || softExpired {
			// The commented line below mysteriously crashes if app.Status.ReconciledAt is nil
			// reason = fmt.Sprintf("comparison expired. reconciledAt: %v, expiry: %v", app.Status.ReconciledAt, statusRefreshTimeout)
			// TODO: find existing Golang bug or create a new one
			reconciledAtStr := "never"
			if app.Status.ReconciledAt != nil {
				reconciledAtStr = app.Status.ReconciledAt.String()
			}
			reason = fmt.Sprintf("comparison expired, requesting refresh. reconciledAt: %v, expiry: %v", reconciledAtStr, statusRefreshTimeout)
			if hardExpired {
				reason = fmt.Sprintf("comparison expired, requesting hard refresh. reconciledAt: %v, expiry: %v", reconciledAtStr, statusHardRefreshTimeout)
				refreshType = appv1.RefreshTypeHard
			}
		} else if !reflect.DeepEqual(app.Spec.Destination, app.Status.Sync.ComparedTo.Destination) {
			reason = "spec.destination differs"
		} else if app.HasChangedManagedNamespaceMetadata() {
			reason = "spec.syncPolicy.managedNamespaceMetadata differs"
		} else if !app.Spec.IgnoreDifferences.Equals(app.Status.Sync.ComparedTo.IgnoreDifferences) {
			reason = "spec.ignoreDifferences differs"
		} else if requested, level := ctrl.isRefreshRequested(app.QualifiedName()); requested {
			compareWith = level
			reason = "controller refresh requested"
		}
	}

	if reason != "" {
		logCtx.Infof("Refreshing app status (%s), level (%d)", reason, compareWith)
		return true, refreshType, compareWith
	}
	return false, refreshType, compareWith
}

func (ctrl *ApplicationController) refreshAppConditions(app *appv1.Application) (*appv1.AppProject, bool) {
	errorConditions := make([]appv1.ApplicationCondition, 0)
	proj, err := ctrl.getAppProj(app)
	if err != nil {
		errorConditions = append(errorConditions, ctrl.projectErrorToCondition(err, app))
	} else {
		specConditions, err := argo.ValidatePermissions(context.Background(), &app.Spec, proj, ctrl.db)
		if err != nil {
			errorConditions = append(errorConditions, appv1.ApplicationCondition{
				Type:    appv1.ApplicationConditionUnknownError,
				Message: err.Error(),
			})
		} else {
			errorConditions = append(errorConditions, specConditions...)
		}
	}
	app.Status.SetConditions(errorConditions, map[appv1.ApplicationConditionType]bool{
		appv1.ApplicationConditionInvalidSpecError: true,
		appv1.ApplicationConditionUnknownError:     true,
	})
	return proj, len(errorConditions) > 0
}

// normalizeApplication normalizes an application.spec and additionally persists updates if it changed
func (ctrl *ApplicationController) normalizeApplication(orig, app *appv1.Application) {
	app.Spec = *argo.NormalizeApplicationSpec(&app.Spec)
	logCtx := log.WithFields(applog.GetAppLogFields(app))

	patch, modified, err := diff.CreateTwoWayMergePatch(orig, app, appv1.Application{})

	if err != nil {
		logCtx.Errorf("error constructing app spec patch: %v", err)
	} else if modified {
		_, err := ctrl.PatchAppWithWriteBack(context.Background(), app.Name, app.Namespace, types.MergePatchType, patch, metav1.PatchOptions{})
		if err != nil {
			logCtx.Errorf("Error persisting normalized application spec: %v", err)
		} else {
			logCtx.Infof("Normalized app spec: %s", string(patch))
		}
	}
}

func createMergePatch(orig, newV any) ([]byte, bool, error) {
	origBytes, err := json.Marshal(orig)
	if err != nil {
		return nil, false, err
	}
	newBytes, err := json.Marshal(newV)
	if err != nil {
		return nil, false, err
	}
	patch, err := jsonpatch.CreateMergePatch(origBytes, newBytes)
	if err != nil {
		return nil, false, err
	}
	return patch, string(patch) != "{}", nil
}

// persistAppStatus persists updates to application status. If no changes were made, it is a no-op
func (ctrl *ApplicationController) persistAppStatus(orig *appv1.Application, newStatus *appv1.ApplicationStatus) (patchDuration time.Duration) {
	logCtx := log.WithFields(applog.GetAppLogFields(orig))
	if orig.Status.Sync.Status != newStatus.Sync.Status {
		message := fmt.Sprintf("Updated sync status: %s -> %s", orig.Status.Sync.Status, newStatus.Sync.Status)
		ctrl.logAppEvent(context.TODO(), orig, argo.EventInfo{Reason: argo.EventReasonResourceUpdated, Type: corev1.EventTypeNormal}, message)
	}
	if orig.Status.Health.Status != newStatus.Health.Status {
		// Update the last transition time to now. This should be the ONLY place in code where this is set, because it's
		// the only place that is reliably aware of the previous and updated health statuses.
		now := metav1.Now()
		newStatus.Health.LastTransitionTime = &now

		message := fmt.Sprintf("Updated health status: %s -> %s", orig.Status.Health.Status, newStatus.Health.Status)
		ctrl.logAppEvent(context.TODO(), orig, argo.EventInfo{Reason: argo.EventReasonResourceUpdated, Type: corev1.EventTypeNormal}, message)
	} else {
		// make sure the last transition time is the same and populated if the health is the same
		newStatus.Health.LastTransitionTime = orig.Status.Health.LastTransitionTime
	}
	var newAnnotations map[string]string
	if orig.GetAnnotations() != nil {
		newAnnotations = make(map[string]string)
		for k, v := range orig.GetAnnotations() {
			newAnnotations[k] = v
		}
		delete(newAnnotations, appv1.AnnotationKeyRefresh)
		delete(newAnnotations, appv1.AnnotationKeyHydrate)
	}
	patch, modified, err := createMergePatch(
		&appv1.Application{ObjectMeta: metav1.ObjectMeta{Annotations: orig.GetAnnotations()}, Status: orig.Status},
		&appv1.Application{ObjectMeta: metav1.ObjectMeta{Annotations: newAnnotations}, Status: *newStatus})
	if err != nil {
		logCtx.Errorf("Error constructing app status patch: %v", err)
		return
	}
	if !modified {
		logCtx.Infof("No status changes. Skipping patch")
		return
	}
	// calculate time for path call
	start := time.Now()
	defer func() {
		patchDuration = time.Since(start)
	}()
	_, err = ctrl.PatchAppWithWriteBack(context.Background(), orig.Name, orig.Namespace, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		logCtx.Warnf("Error updating application: %v", err)
	} else {
		logCtx.Infof("Update successful")
	}
	return patchDuration
}

// autoSync will initiate a sync operation for an application configured with automated sync
func (ctrl *ApplicationController) autoSync(app *appv1.Application, syncStatus *appv1.SyncStatus, resources []appv1.ResourceStatus, shouldCompareRevisions bool) (*appv1.ApplicationCondition, time.Duration) {
	logCtx := log.WithFields(applog.GetAppLogFields(app))
	ts := stats.NewTimingStats()
	defer func() {
		for k, v := range ts.Timings() {
			logCtx = logCtx.WithField(k, v.Milliseconds())
		}
		logCtx = logCtx.WithField("time_ms", time.Since(ts.StartTime).Milliseconds())
		logCtx.Debug("Finished auto sync")
	}()
	if app.Spec.SyncPolicy == nil || !app.Spec.SyncPolicy.IsAutomatedSyncEnabled() {
		return nil, 0
	}

	if app.Operation != nil {
		logCtx.Infof("Skipping auto-sync: another operation is in progress")
		return nil, 0
	}
	if app.DeletionTimestamp != nil && !app.DeletionTimestamp.IsZero() {
		logCtx.Infof("Skipping auto-sync: deletion in progress")
		return nil, 0
	}

	// Only perform auto-sync if we detect OutOfSync status. This is to prevent us from attempting
	// a sync when application is already in a Synced or Unknown state
	if syncStatus.Status != appv1.SyncStatusCodeOutOfSync {
		logCtx.Infof("Skipping auto-sync: application status is %s", syncStatus.Status)
		return nil, 0
	}

	if !app.Spec.SyncPolicy.Automated.Prune {
		requirePruneOnly := true
		for _, r := range resources {
			if r.Status != appv1.SyncStatusCodeSynced && !r.RequiresPruning {
				requirePruneOnly = false
				break
			}
		}
		if requirePruneOnly {
			logCtx.Infof("Skipping auto-sync: need to prune extra resources only but automated prune is disabled")
			return nil, 0
		}
	}

	desiredRevisions := []string{syncStatus.Revision}
	if app.Spec.HasMultipleSources() {
		desiredRevisions = syncStatus.Revisions
	}

	op := appv1.Operation{
		Sync: &appv1.SyncOperation{
			Revision:    syncStatus.Revision,
			Prune:       app.Spec.SyncPolicy.Automated.Prune,
			SyncOptions: app.Spec.SyncPolicy.SyncOptions,
			Revisions:   syncStatus.Revisions,
		},
		InitiatedBy: appv1.OperationInitiator{Automated: true},
		Retry:       appv1.RetryStrategy{Limit: 5},
	}
	if app.Spec.SyncPolicy.Retry != nil {
		op.Retry = *app.Spec.SyncPolicy.Retry
	}

	// It is possible for manifests to remain OutOfSync even after a sync/kubectl apply (e.g.
	// auto-sync with pruning disabled). We need to ensure that we do not keep Syncing an
	// application in an infinite loop. To detect this, we only attempt the Sync if the revision
	// and parameter overrides are different from our most recent sync operation.
	alreadyAttempted, lastAttemptedRevisions, lastAttemptedPhase := alreadyAttemptedSync(app, desiredRevisions, shouldCompareRevisions)
	ts.AddCheckpoint("already_attempted_sync_ms")
	if alreadyAttempted {
		if !lastAttemptedPhase.Successful() {
			logCtx.Warnf("Skipping auto-sync: failed previous sync attempt to %s and will not retry for %s", lastAttemptedRevisions, desiredRevisions)
			message := fmt.Sprintf("Failed last sync attempt to %s: %s", lastAttemptedRevisions, app.Status.OperationState.Message)
			return &appv1.ApplicationCondition{Type: appv1.ApplicationConditionSyncError, Message: message}, 0
		}
		if !app.Spec.SyncPolicy.Automated.SelfHeal {
			logCtx.Infof("Skipping auto-sync: most recent sync already to %s", desiredRevisions)
			return nil, 0
		}
		// Self heal will trigger a new sync operation when the desired state changes and cause the application to
		// be OutOfSync when it was previously synced Successfully. This means SelfHeal should only ever be attempted
		// when the revisions have not changed, and where the previous sync to these revision was successful

		// Only carry SelfHealAttemptsCount to be increased when the selfHealBackoffCooldown has not elapsed yet
		if !ctrl.selfHealBackoffCooldownElapsed(app) {
			if app.Status.OperationState != nil && app.Status.OperationState.Operation.Sync != nil {
				op.Sync.SelfHealAttemptsCount = app.Status.OperationState.Operation.Sync.SelfHealAttemptsCount
			}
		}

		if remainingTime := ctrl.selfHealRemainingBackoff(app, int(op.Sync.SelfHealAttemptsCount)); remainingTime > 0 {
			logCtx.Infof("Skipping auto-sync: already attempted sync to %s with timeout %v (retrying in %v)", lastAttemptedRevisions, ctrl.selfHealTimeout, remainingTime)
			ctrl.requestAppRefresh(app.QualifiedName(), CompareWithLatest.Pointer(), &remainingTime)
			return nil, 0
		}

		op.Sync.SelfHealAttemptsCount++
		for _, resource := range resources {
			if resource.Status != appv1.SyncStatusCodeSynced {
				op.Sync.Resources = append(op.Sync.Resources, appv1.SyncOperationResource{
					Kind:  resource.Kind,
					Group: resource.Group,
					Name:  resource.Name,
				})
			}
		}
	}
	ts.AddCheckpoint("already_attempted_check_ms")

	if app.Spec.SyncPolicy.Automated.Prune && !app.Spec.SyncPolicy.Automated.AllowEmpty {
		bAllNeedPrune := true
		for _, r := range resources {
			if !r.RequiresPruning {
				bAllNeedPrune = false
			}
		}
		if bAllNeedPrune {
			message := fmt.Sprintf("Skipping sync attempt to %s: auto-sync will wipe out all resources", desiredRevisions)
			logCtx.Warn(message)
			return &appv1.ApplicationCondition{Type: appv1.ApplicationConditionSyncError, Message: message}, 0
		}
	}

	appIf := ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace)
	ts.AddCheckpoint("get_applications_ms")
	start := time.Now()
	updatedApp, err := argo.SetAppOperation(appIf, app.Name, &op)
	ts.AddCheckpoint("set_app_operation_ms")
	setOpTime := time.Since(start)
	if err != nil {
		if stderrors.Is(err, argo.ErrAnotherOperationInProgress) {
			// skipping auto-sync because another operation is in progress and was not noticed due to stale data in informer
			// it is safe to skip auto-sync because it is already running
			logCtx.Warnf("Failed to initiate auto-sync to %s: %v", desiredRevisions, err)
			return nil, 0
		}

		logCtx.Errorf("Failed to initiate auto-sync to %s: %v", desiredRevisions, err)
		return &appv1.ApplicationCondition{Type: appv1.ApplicationConditionSyncError, Message: err.Error()}, setOpTime
	}
	ctrl.writeBackToInformer(updatedApp)
	ts.AddCheckpoint("write_back_to_informer_ms")

	message := fmt.Sprintf("Initiated automated sync to %s", desiredRevisions)
	ctrl.logAppEvent(context.TODO(), app, argo.EventInfo{Reason: argo.EventReasonOperationStarted, Type: corev1.EventTypeNormal}, message)
	logCtx.Info(message)
	return nil, setOpTime
}

// alreadyAttemptedSync returns whether the most recently synced revision(s) exactly match the given desiredRevisions
// and for the same application source. If the revision(s) have changed or the Application source configuration has been updated,
// it will return false, indicating that a new sync should be attempted.
// When newRevisionHasChanges is false, due to commits not having direct changes on the application, it will not compare the revision(s), but only the sources.
// It also returns the last synced revisions if any, and the result of that last sync operation.
func alreadyAttemptedSync(app *appv1.Application, desiredRevisions []string, newRevisionHasChanges bool) (bool, []string, synccommon.OperationPhase) {
	if app.Status.OperationState == nil {
		// The operation state may be removed when new operations are triggered
		return false, []string{}, ""
	}
	if app.Status.OperationState.SyncResult == nil {
		// If the sync has completed without result, it is very likely that an error happened
		// We don't want to resync with auto-sync indefinitely. We should have retried the configured amount of time already
		// In this case, a manual action to restore the app may be required
		log.WithFields(applog.GetAppLogFields(app)).Warn("Already attempted sync: sync does not have any results")
		return app.Status.OperationState.Phase.Completed(), []string{}, app.Status.OperationState.Phase
	}

	if newRevisionHasChanges {
		log.WithFields(applog.GetAppLogFields(app)).Infof("Already attempted sync: comparing synced revisions to %s", desiredRevisions)
		if app.Spec.HasMultipleSources() {
			if !reflect.DeepEqual(app.Status.OperationState.SyncResult.Revisions, desiredRevisions) {
				return false, app.Status.OperationState.SyncResult.Revisions, app.Status.OperationState.Phase
			}
		} else {
			if len(desiredRevisions) != 1 || app.Status.OperationState.SyncResult.Revision != desiredRevisions[0] {
				return false, []string{app.Status.OperationState.SyncResult.Revision}, app.Status.OperationState.Phase
			}
		}
	} else {
		log.WithFields(applog.GetAppLogFields(app)).Debugf("Already attempted sync: revisions %s have no changes", desiredRevisions)
	}

	log.WithFields(applog.GetAppLogFields(app)).Debug("Already attempted sync: comparing sources")
	if app.Spec.HasMultipleSources() {
		return reflect.DeepEqual(app.Spec.Sources, app.Status.OperationState.SyncResult.Sources), app.Status.OperationState.SyncResult.Revisions, app.Status.OperationState.Phase
	}
	return reflect.DeepEqual(app.Spec.GetSource(), app.Status.OperationState.SyncResult.Source), []string{app.Status.OperationState.SyncResult.Revision}, app.Status.OperationState.Phase
}

func (ctrl *ApplicationController) selfHealRemainingBackoff(app *appv1.Application, selfHealAttemptsCount int) time.Duration {
	if app.Status.OperationState == nil {
		return time.Duration(0)
	}

	var timeSinceOperation *time.Duration
	if app.Status.OperationState.FinishedAt != nil {
		timeSinceOperation = ptr.To(time.Since(app.Status.OperationState.FinishedAt.Time))
	}

	var retryAfter time.Duration
	if ctrl.selfHealBackoff == nil {
		if timeSinceOperation == nil {
			retryAfter = ctrl.selfHealTimeout
		} else {
			retryAfter = ctrl.selfHealTimeout - *timeSinceOperation
		}
	} else {
		backOff := *ctrl.selfHealBackoff
		backOff.Steps = selfHealAttemptsCount
		var delay time.Duration
		steps := backOff.Steps
		for i := 0; i < steps; i++ {
			delay = backOff.Step()
		}
		if timeSinceOperation == nil {
			retryAfter = delay
		} else {
			retryAfter = delay - *timeSinceOperation
		}
	}
	return retryAfter
}

// selfHealBackoffCooldownElapsed returns true when the last successful sync has occurred since longer
// than then self heal cooldown. This means that the application has been in sync for long enough to
// reset the self healing backoff to its initial state
func (ctrl *ApplicationController) selfHealBackoffCooldownElapsed(app *appv1.Application) bool {
	if app.Status.OperationState == nil || app.Status.OperationState.FinishedAt == nil {
		// Something is in progress, or about to be. In that case, selfHeal attempt should be zero anyway
		return true
	}

	timeSinceLastOperation := time.Since(app.Status.OperationState.FinishedAt.Time)
	return timeSinceLastOperation >= ctrl.selfHealBackoffCooldown && app.Status.OperationState.Phase.Successful()
}

// isAppNamespaceAllowed returns whether the application is allowed in the
// namespace it's residing in.
func (ctrl *ApplicationController) isAppNamespaceAllowed(app *appv1.Application) bool {
	return app.Namespace == ctrl.namespace || glob.MatchStringInList(ctrl.applicationNamespaces, app.Namespace, glob.REGEXP)
}

func (ctrl *ApplicationController) canProcessApp(obj any) bool {
	app, ok := obj.(*appv1.Application)
	if !ok {
		return false
	}

	// Only process given app if it exists in a watched namespace, or in the
	// control plane's namespace.
	if !ctrl.isAppNamespaceAllowed(app) {
		return false
	}

	if annotations := app.GetAnnotations(); annotations != nil {
		if skipVal, ok := annotations[common.AnnotationKeyAppSkipReconcile]; ok {
			logCtx := log.WithFields(applog.GetAppLogFields(app))
			if skipReconcile, err := strconv.ParseBool(skipVal); err == nil {
				if skipReconcile {
					logCtx.Debugf("Skipping Application reconcile based on annotation %s", common.AnnotationKeyAppSkipReconcile)
					return false
				}
			} else {
				logCtx.Debugf("Unable to determine if Application should skip reconcile based on annotation %s: %v", common.AnnotationKeyAppSkipReconcile, err)
			}
		}
	}

	destCluster, err := argo.GetDestinationCluster(context.Background(), app.Spec.Destination, ctrl.db)
	if err != nil {
		return ctrl.clusterSharding.IsManagedCluster(nil)
	}
	return ctrl.clusterSharding.IsManagedCluster(destCluster)
}

func (ctrl *ApplicationController) newApplicationInformerAndLister() (cache.SharedIndexInformer, applisters.ApplicationLister) {
	watchNamespace := ctrl.namespace
	// If we have at least one additional namespace configured, we need to
	// watch on them all.
	if len(ctrl.applicationNamespaces) > 0 {
		watchNamespace = ""
	}
	refreshTimeout := ctrl.statusRefreshTimeout
	if ctrl.statusHardRefreshTimeout.Seconds() != 0 && (ctrl.statusHardRefreshTimeout < ctrl.statusRefreshTimeout) {
		refreshTimeout = ctrl.statusHardRefreshTimeout
	}
	informer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (apiruntime.Object, error) {
				// We are only interested in apps that exist in namespaces the
				// user wants to be enabled.
				appList, err := ctrl.applicationClientset.ArgoprojV1alpha1().Applications(watchNamespace).List(context.TODO(), options)
				if err != nil {
					return nil, err
				}
				newItems := []appv1.Application{}
				for _, app := range appList.Items {
					if ctrl.isAppNamespaceAllowed(&app) {
						newItems = append(newItems, app)
					}
				}
				appList.Items = newItems
				return appList, nil
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return ctrl.applicationClientset.ArgoprojV1alpha1().Applications(watchNamespace).Watch(context.TODO(), options)
			},
		},
		&appv1.Application{},
		refreshTimeout,
		cache.Indexers{
			cache.NamespaceIndex: func(obj any) ([]string, error) {
				app, ok := obj.(*appv1.Application)
				if ok {
					// We only generally work with applications that are in one
					// the allowed namespaces.
					if ctrl.isAppNamespaceAllowed(app) {
						// If the application is not allowed to use the project,
						// log an error.
						if _, err := ctrl.getAppProj(app); err != nil {
							ctrl.setAppCondition(app, ctrl.projectErrorToCondition(err, app))
						} else if _, err = argo.GetDestinationCluster(context.Background(), app.Spec.Destination, ctrl.db); err != nil {
							ctrl.setAppCondition(app, appv1.ApplicationCondition{Type: appv1.ApplicationConditionInvalidSpecError, Message: err.Error()})
						}
					}
				}

				return cache.MetaNamespaceIndexFunc(obj)
			},
			orphanedIndex: func(obj any) (i []string, e error) {
				app, ok := obj.(*appv1.Application)
				if !ok {
					return nil, nil
				}

				if !ctrl.isAppNamespaceAllowed(app) {
					return nil, nil
				}

				proj, err := ctrl.getAppProj(app)
				if err != nil {
					return nil, nil
				}
				if proj.Spec.OrphanedResources != nil {
					return []string{app.Spec.Destination.Namespace}, nil
				}
				return nil, nil
			},
		},
	)
	lister := applisters.NewApplicationLister(informer.GetIndexer())
	_, err := informer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj any) {
				if !ctrl.canProcessApp(obj) {
					return
				}
				key, err := cache.MetaNamespaceKeyFunc(obj)
				if err == nil {
					ctrl.appRefreshQueue.AddRateLimited(key)
				}
				newApp, newOK := obj.(*appv1.Application)
				if err == nil && newOK {
					ctrl.clusterSharding.AddApp(newApp)
				}
			},
			UpdateFunc: func(old, new any) {
				if !ctrl.canProcessApp(new) {
					return
				}

				key, err := cache.MetaNamespaceKeyFunc(new)
				if err != nil {
					return
				}

				var compareWith *CompareWith
				var delay *time.Duration

				oldApp, oldOK := old.(*appv1.Application)
				newApp, newOK := new.(*appv1.Application)
				if oldOK && newOK {
					if automatedSyncEnabled(oldApp, newApp) {
						log.WithFields(applog.GetAppLogFields(newApp)).Info("Enabled automated sync")
						compareWith = CompareWithLatest.Pointer()
					}
					if ctrl.statusRefreshJitter != 0 && oldApp.ResourceVersion == newApp.ResourceVersion {
						// Handler is refreshing the apps, add a random jitter to spread the load and avoid spikes
						jitter := time.Duration(float64(ctrl.statusRefreshJitter) * rand.Float64())
						delay = &jitter
					}
				}

				ctrl.requestAppRefresh(newApp.QualifiedName(), compareWith, delay)
				if !newOK || (delay != nil && *delay != time.Duration(0)) {
					ctrl.appOperationQueue.AddRateLimited(key)
				}
				if ctrl.hydrator != nil {
					ctrl.appHydrateQueue.AddRateLimited(newApp.QualifiedName())
				}
				ctrl.clusterSharding.UpdateApp(newApp)
			},
			DeleteFunc: func(obj any) {
				if !ctrl.canProcessApp(obj) {
					return
				}
				// IndexerInformer uses a delta queue, therefore for deletes we have to use this
				// key function.
				key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
				if err == nil {
					// for deletes, we immediately add to the refresh queue
					ctrl.appRefreshQueue.Add(key)
				}
				delApp, delOK := obj.(*appv1.Application)
				if err == nil && delOK {
					ctrl.clusterSharding.DeleteApp(delApp)
				}
			},
		},
	)
	if err != nil {
		return nil, nil
	}
	return informer, lister
}

func (ctrl *ApplicationController) projectErrorToCondition(err error, app *appv1.Application) appv1.ApplicationCondition {
	var condition appv1.ApplicationCondition
	if apierrors.IsNotFound(err) {
		condition = appv1.ApplicationCondition{
			Type:    appv1.ApplicationConditionInvalidSpecError,
			Message: fmt.Sprintf("Application referencing project %s which does not exist", app.Spec.Project),
		}
	} else {
		condition = appv1.ApplicationCondition{Type: appv1.ApplicationConditionUnknownError, Message: err.Error()}
	}
	return condition
}

func (ctrl *ApplicationController) RegisterClusterSecretUpdater(ctx context.Context) {
	updater := NewClusterInfoUpdater(ctrl.stateCache, ctrl.db, ctrl.appLister.Applications(""), ctrl.cache, ctrl.clusterSharding.IsManagedCluster, ctrl.getAppProj, ctrl.namespace)
	go updater.Run(ctx)
}

func isOperationInProgress(app *appv1.Application) bool {
	return app.Status.OperationState != nil && !app.Status.OperationState.Phase.Completed()
}

// automatedSyncEnabled tests if an app went from auto-sync disabled to enabled.
// if it was toggled to be enabled, the informer handler will force a refresh
func automatedSyncEnabled(oldApp *appv1.Application, newApp *appv1.Application) bool {
	oldEnabled := false
	oldSelfHealEnabled := false
	if oldApp.Spec.SyncPolicy != nil && oldApp.Spec.SyncPolicy.IsAutomatedSyncEnabled() {
		oldEnabled = true
		oldSelfHealEnabled = oldApp.Spec.SyncPolicy.Automated.SelfHeal
	}

	newEnabled := false
	newSelfHealEnabled := false
	if newApp.Spec.SyncPolicy != nil && newApp.Spec.SyncPolicy.IsAutomatedSyncEnabled() {
		newEnabled = true
		newSelfHealEnabled = newApp.Spec.SyncPolicy.Automated.SelfHeal
	}
	if !oldEnabled && newEnabled {
		return true
	}
	if !oldSelfHealEnabled && newSelfHealEnabled {
		return true
	}
	// nothing changed
	return false
}

// toAppKey returns the application key from a given appName, that is, it will
// replace underscores with forward-slashes to become a <namespace>/<name>
// format. If the appName is an unqualified name (such as, "app"), it will use
// the controller's namespace in the key.
func (ctrl *ApplicationController) toAppKey(appName string) string {
	if !strings.Contains(appName, "_") && !strings.Contains(appName, "/") {
		return ctrl.namespace + "/" + appName
	} else if strings.Contains(appName, "/") {
		return appName
	}
	return strings.ReplaceAll(appName, "_", "/")
}

func (ctrl *ApplicationController) toAppQualifiedName(appName, appNamespace string) string {
	return fmt.Sprintf("%s/%s", appNamespace, appName)
}

func (ctrl *ApplicationController) getAppList(options metav1.ListOptions) (*appv1.ApplicationList, error) {
	watchNamespace := ctrl.namespace
	// If we have at least one additional namespace configured, we need to
	// watch on them all.
	if len(ctrl.applicationNamespaces) > 0 {
		watchNamespace = ""
	}

	appList, err := ctrl.applicationClientset.ArgoprojV1alpha1().Applications(watchNamespace).List(context.TODO(), options)
	if err != nil {
		return nil, err
	}
	newItems := []appv1.Application{}
	for _, app := range appList.Items {
		if ctrl.isAppNamespaceAllowed(&app) {
			newItems = append(newItems, app)
		}
	}
	appList.Items = newItems
	return appList, nil
}

func (ctrl *ApplicationController) logAppEvent(ctx context.Context, a *appv1.Application, eventInfo argo.EventInfo, message string) {
	eventLabels := argo.GetAppEventLabels(ctx, a, applisters.NewAppProjectLister(ctrl.projInformer.GetIndexer()), ctrl.namespace, ctrl.settingsMgr, ctrl.db)
	ctrl.auditLogger.LogAppEvent(a, eventInfo, message, "", eventLabels)
}

type ClusterFilterFunction func(c *appv1.Cluster, distributionFunction sharding.DistributionFunction) bool
