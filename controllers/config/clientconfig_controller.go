package config

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/go-logr/logr"
	configapi "github.com/k8ssandra/k8ssandra-operator/apis/config/v1beta1"
	k8ssandraapi "github.com/k8ssandra/k8ssandra-operator/apis/k8ssandra/v1alpha1"
	"github.com/k8ssandra/k8ssandra-operator/pkg/clientcache"
	"github.com/k8ssandra/k8ssandra-operator/pkg/utils"
)

const (
	ClientConfigHashAnnotation = k8ssandraapi.ResourceHashAnnotation
	KubeSecretHashAnnotation   = "k8ssandra.io/secret-hash"
)

type ClientConfigReconciler struct {
	Scheme       *runtime.Scheme
	ClientCache  *clientcache.ClientCache
	shutdownFunc context.CancelFunc

	// filterMutex  sync.RWMutex
	secretFilter map[types.NamespacedName]types.NamespacedName
}

func (r *ClientConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	nonCacheClient := r.ClientCache.GetLocalNonCacheClient()

	clientConfig := configapi.ClientConfig{}
	if err := nonCacheClient.Get(ctx, req.NamespacedName, &clientConfig); err != nil {
		if errors.IsNotFound(err) {
			// ClientConfig was deleted, shutdown to refresh correct list
			logger.Info(fmt.Sprintf("ClientConfig %v was deleted, shutting down the operator", req))
			r.shutdownFunc()
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// ClientConfig without proper annotations, must be a new item, shutdown to refresh correct list
	if !metav1.HasAnnotation(clientConfig.ObjectMeta, ClientConfigHashAnnotation) ||
		!metav1.HasAnnotation(clientConfig.ObjectMeta, KubeSecretHashAnnotation) {
		logger.Info(fmt.Sprintf("ClientConfig %v is missing hash annotations, shutting down the operator", req))
		r.shutdownFunc()
		return ctrl.Result{}, nil
	}

	cCfgHash, secretHash, err := calculateHashes(ctx, nonCacheClient, clientConfig)
	if err != nil {
		if errors.IsNotFound(err) {
			// ClientConfig was deleted, shutdown to refresh correct list
			logger.Info(fmt.Sprintf("Secret %v was deleted, shutting down the operator", req))
			r.shutdownFunc()
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Verify hashes are still original
	if clientConfig.Annotations[ClientConfigHashAnnotation] != cCfgHash ||
		clientConfig.Annotations[KubeSecretHashAnnotation] != secretHash {
		// Hashes do not match, something was modified, shutdown to refresh
		logger.Info(fmt.Sprintf("ClientConfig %v or secret has been modified, shutting down the operator", req))
		r.shutdownFunc()
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager will only set this controller to listen in control plane cluster
func (r *ClientConfigReconciler) SetupWithManager(mgr ctrl.Manager, cancelFunc context.CancelFunc) error {
	r.shutdownFunc = cancelFunc
	if r.secretFilter == nil {
		r.secretFilter = make(map[types.NamespacedName]types.NamespacedName)
	}

	// We should only reconcile objects that match the rules
	toMatchingClientConfig := func(ctx context.Context, secret client.Object) []reconcile.Request {
		requests := []reconcile.Request{}
		secretKey := types.NamespacedName{Name: secret.GetName(), Namespace: secret.GetNamespace()}
		if clientConfigName, found := r.secretFilter[secretKey]; found {
			requests = append(requests, reconcile.Request{NamespacedName: clientConfigName})
		}
		return requests
	}

	cb := ctrl.NewControllerManagedBy(mgr).
		For(&configapi.ClientConfig{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(toMatchingClientConfig))

	return cb.Complete(r)
}

// InitClientConfigs will fetch clientConfigs from the current cluster (control plane cluster) and create all the required Cluster objects for
// other controllers to use. Not called from SetupWithManager since other controllers need the []cluster.Cluster array
func (r *ClientConfigReconciler) InitClientConfigs(ctx context.Context, mgr ctrl.Manager, watchNamespace string) ([]cluster.Cluster, error) {
	logger := log.FromContext(ctx)

	uncachedClient := r.ClientCache.GetLocalNonCacheClient()
	clientConfigs := make([]configapi.ClientConfig, 0)
	namespaces := []string{}
	if watchNamespace != "" {
		namespaces = strings.Split(watchNamespace, ",")
		for _, ns := range namespaces {
			cConfigs := configapi.ClientConfigList{}
			if err := uncachedClient.List(ctx, &cConfigs, client.InNamespace(ns)); err != nil {
				return nil, err
			}
			clientConfigs = append(clientConfigs, cConfigs.Items...)
		}
	} else {
		cConfigs := configapi.ClientConfigList{}
		if err := uncachedClient.List(ctx, &cConfigs); err != nil {
			return nil, err
		}
		clientConfigs = append(clientConfigs, cConfigs.Items...)
	}

	additionalClusters := make([]cluster.Cluster, 0, len(clientConfigs))

	// TODO Secret could point to multiple clientConfigs. Shouldn't matter in our current use-case
	r.secretFilter = make(map[types.NamespacedName]types.NamespacedName, len(clientConfigs))

	for _, cCfg := range clientConfigs {
		logger.V(1).Info(fmt.Sprintf("Initializing client config %s namespaces %s", cCfg.Name, namespaces))
		c, err := r.initAdditionalClusterConfig(ctx, cCfg, mgr, namespaces, logger)
		if err != nil {
			return nil, err
		}
		additionalClusters = append(additionalClusters, c)
	}

	logger.V(1).Info(fmt.Sprintf("Finished initializing %d client configs", len(clientConfigs)))

	return additionalClusters, nil
}

func calculateHashes(ctx context.Context, anyClient client.Client, clientCfg configapi.ClientConfig) (string, string, error) {
	secret := &corev1.Secret{}
	secretName := types.NamespacedName{Name: clientCfg.Spec.KubeConfigSecret.Name, Namespace: clientCfg.Namespace}

	if err := anyClient.Get(ctx, secretName, secret); err != nil {
		return "", "", err
	}

	cfgHash := utils.DeepHashString(clientCfg.Spec)
	secretHash := utils.DeepHashString(secret.Data)

	return cfgHash, secretHash, nil
}

// initAdditionalCLusterConfig fetches the clientConfigs for additional clusters
func (r *ClientConfigReconciler) initAdditionalClusterConfig(ctx context.Context, cCfg configapi.ClientConfig, mgr ctrl.Manager, namespaces []string, logger logr.Logger) (cluster.Cluster, error) {
	uncachedClient := r.ClientCache.GetLocalNonCacheClient()

	// Calculate hashes
	cCfgName := types.NamespacedName{Name: cCfg.Name, Namespace: cCfg.Namespace}
	secretName := types.NamespacedName{Name: cCfg.Spec.KubeConfigSecret.Name, Namespace: cCfg.Namespace}

	cCfgHash, secretHash, err := calculateHashes(ctx, uncachedClient, cCfg)
	if err != nil {
		return nil, err
	}

	metav1.SetMetaDataAnnotation(&cCfg.ObjectMeta, ClientConfigHashAnnotation, cCfgHash)
	metav1.SetMetaDataAnnotation(&cCfg.ObjectMeta, KubeSecretHashAnnotation, secretHash)

	if err := uncachedClient.Update(ctx, &cCfg); err != nil {
		return nil, err
	}

	// Add the Secret to the cache
	r.secretFilter[secretName] = cCfgName

	// Create clients and add them to the client cache
	cfg, err := r.ClientCache.GetRestConfig(&cCfg)
	if err != nil {
		return nil, err
	}

	// Add cluster to the manager
	var c cluster.Cluster
	c, err = cluster.New(cfg, func(o *cluster.Options) {
		o.Scheme = r.Scheme
		if len(namespaces) > 0 {
			nsConfig := make(map[string]cache.Config)
			for _, i := range namespaces {
				nsConfig[i] = cache.Config{}
			}
			o.Cache.DefaultNamespaces = nsConfig
		}
	})

	if err != nil {
		return nil, err
	}

	r.ClientCache.AddClient(cCfg.GetContextName(), c.GetClient())

	err = mgr.Add(c)
	if err != nil {
		return nil, err
	}

	return c, nil
}
