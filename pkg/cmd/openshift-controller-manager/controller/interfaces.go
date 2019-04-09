package controller

import (
	"sync"
	"time"

	"k8s.io/klog"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	cacheddiscovery "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	controllerapp "k8s.io/kubernetes/cmd/kube-controller-manager/app"
	"k8s.io/kubernetes/pkg/controller"

	openshiftcontrolplanev1 "github.com/openshift/api/openshiftcontrolplane/v1"
	appsclient "github.com/openshift/client-go/apps/clientset/versioned"
	appsinformer "github.com/openshift/client-go/apps/informers/externalversions"
	buildclient "github.com/openshift/client-go/build/clientset/versioned"
	buildinformer "github.com/openshift/client-go/build/informers/externalversions"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	configinformer "github.com/openshift/client-go/config/informers/externalversions"
	imageclient "github.com/openshift/client-go/image/clientset/versioned"
	imageinformer "github.com/openshift/client-go/image/informers/externalversions"
	networkclient "github.com/openshift/client-go/network/clientset/versioned"
	networkinformer "github.com/openshift/client-go/network/informers/externalversions"
	quotaclient "github.com/openshift/client-go/quota/clientset/versioned"
	quotainformer "github.com/openshift/client-go/quota/informers/externalversions"
	routeclient "github.com/openshift/client-go/route/clientset/versioned"
	routeinformer "github.com/openshift/client-go/route/informers/externalversions"
	securityclient "github.com/openshift/client-go/security/clientset/versioned"
	templateclient "github.com/openshift/client-go/template/clientset/versioned"
	templateinformer "github.com/openshift/client-go/template/informers/externalversions"
	"github.com/openshift/origin/pkg/client/genericinformers"
	"github.com/openshift/origin/pkg/cmd/server/bootstrappolicy"
)

func NewControllerContext(
	config openshiftcontrolplanev1.OpenShiftControllerManagerConfig,
	inClientConfig *rest.Config,
	stopCh <-chan struct{},
) (*ControllerContext, error) {

	const defaultInformerResyncPeriod = 10 * time.Minute
	kubeClient, err := kubernetes.NewForConfig(inClientConfig)
	if err != nil {
		return nil, err
	}

	// copy to avoid messing with original
	clientConfig := rest.CopyConfig(inClientConfig)
	// divide up the QPS since it re-used separately for every client
	// TODO, eventually make this configurable individually in some way.
	if clientConfig.QPS > 0 {
		clientConfig.QPS = clientConfig.QPS/10 + 1
	}
	if clientConfig.Burst > 0 {
		clientConfig.Burst = clientConfig.Burst/10 + 1
	}

	discoveryClient := cacheddiscovery.NewMemCacheClient(kubeClient.Discovery())
	dynamicRestMapper := restmapper.NewDeferredDiscoveryRESTMapper(discoveryClient)
	dynamicRestMapper.Reset()
	go wait.Until(dynamicRestMapper.Reset, 30*time.Second, stopCh)

	appsClient, err := appsclient.NewForConfig(clientConfig)
	if err != nil {
		return nil, err
	}
	buildClient, err := buildclient.NewForConfig(clientConfig)
	if err != nil {
		return nil, err
	}
	configClient, err := configclient.NewForConfig(nonProtobufConfig(clientConfig))
	if err != nil {
		return nil, err
	}
	imageClient, err := imageclient.NewForConfig(clientConfig)
	if err != nil {
		return nil, err
	}
	networkClient, err := networkclient.NewForConfig(nonProtobufConfig(clientConfig))
	if err != nil {
		return nil, err
	}
	quotaClient, err := quotaclient.NewForConfig(nonProtobufConfig(clientConfig))
	if err != nil {
		return nil, err
	}
	routerClient, err := routeclient.NewForConfig(clientConfig)
	if err != nil {
		return nil, err
	}
	templateClient, err := templateclient.NewForConfig(clientConfig)
	if err != nil {
		return nil, err
	}

	openshiftControllerContext := &ControllerContext{
		OpenshiftControllerConfig: config,

		ClientBuilder: OpenshiftControllerClientBuilder{
			ControllerClientBuilder: controller.SAControllerClientBuilder{
				ClientConfig:         rest.AnonymousClientConfig(clientConfig),
				CoreClient:           kubeClient.CoreV1(),
				AuthenticationClient: kubeClient.AuthenticationV1(),
				Namespace:            bootstrappolicy.DefaultOpenShiftInfraNamespace,
			},
		},
		KubernetesInformers:                informers.NewSharedInformerFactory(kubeClient, defaultInformerResyncPeriod),
		OpenshiftConfigKubernetesInformers: informers.NewSharedInformerFactoryWithOptions(kubeClient, defaultInformerResyncPeriod, informers.WithNamespace("openshift-config")),
		AppsInformers:                      appsinformer.NewSharedInformerFactory(appsClient, defaultInformerResyncPeriod),
		BuildInformers:                     buildinformer.NewSharedInformerFactory(buildClient, defaultInformerResyncPeriod),
		ConfigInformers:                    configinformer.NewSharedInformerFactory(configClient, defaultInformerResyncPeriod),
		ImageInformers:                     imageinformer.NewSharedInformerFactory(imageClient, defaultInformerResyncPeriod),
		NetworkInformers:                   networkinformer.NewSharedInformerFactory(networkClient, defaultInformerResyncPeriod),
		QuotaInformers:                     quotainformer.NewSharedInformerFactory(quotaClient, defaultInformerResyncPeriod),
		RouteInformers:                     routeinformer.NewSharedInformerFactory(routerClient, defaultInformerResyncPeriod),
		TemplateInformers:                  templateinformer.NewSharedInformerFactory(templateClient, defaultInformerResyncPeriod),
		Stop:                               stopCh,
		InformersStarted:                   make(chan struct{}),
		RestMapper:                         dynamicRestMapper,
	}
	openshiftControllerContext.GenericResourceInformer = openshiftControllerContext.ToGenericInformer()

	return openshiftControllerContext, nil
}

func (c *ControllerContext) ToGenericInformer() genericinformers.GenericResourceInformer {
	return genericinformers.NewGenericInformers(
		c.StartInformers,
		c.KubernetesInformers,
		genericinformers.GenericResourceInformerFunc(func(resource schema.GroupVersionResource) (informers.GenericInformer, error) {
			return c.AppsInformers.ForResource(resource)
		}),
		genericinformers.GenericResourceInformerFunc(func(resource schema.GroupVersionResource) (informers.GenericInformer, error) {
			return c.BuildInformers.ForResource(resource)
		}),
		genericinformers.GenericResourceInformerFunc(func(resource schema.GroupVersionResource) (informers.GenericInformer, error) {
			return c.ConfigInformers.ForResource(resource)
		}),
		genericinformers.GenericResourceInformerFunc(func(resource schema.GroupVersionResource) (informers.GenericInformer, error) {
			return c.ImageInformers.ForResource(resource)
		}),
		genericinformers.GenericResourceInformerFunc(func(resource schema.GroupVersionResource) (informers.GenericInformer, error) {
			return c.NetworkInformers.ForResource(resource)
		}),
		genericinformers.GenericInternalResourceInformerFunc(func(resource schema.GroupVersionResource) (informers.GenericInformer, error) {
			return c.QuotaInformers.ForResource(resource)
		}),
		genericinformers.GenericResourceInformerFunc(func(resource schema.GroupVersionResource) (informers.GenericInformer, error) {
			return c.RouteInformers.ForResource(resource)
		}),
		genericinformers.GenericInternalResourceInformerFunc(func(resource schema.GroupVersionResource) (informers.GenericInformer, error) {
			return c.TemplateInformers.ForResource(resource)
		}),
	)
}

type ControllerContext struct {
	OpenshiftControllerConfig openshiftcontrolplanev1.OpenShiftControllerManagerConfig

	// ClientBuilder will provide a client for this controller to use
	ClientBuilder ControllerClientBuilder

	KubernetesInformers                informers.SharedInformerFactory
	OpenshiftConfigKubernetesInformers informers.SharedInformerFactory

	TemplateInformers templateinformer.SharedInformerFactory
	QuotaInformers    quotainformer.SharedInformerFactory
	RouteInformers    routeinformer.SharedInformerFactory

	AppsInformers    appsinformer.SharedInformerFactory
	BuildInformers   buildinformer.SharedInformerFactory
	ConfigInformers  configinformer.SharedInformerFactory
	ImageInformers   imageinformer.SharedInformerFactory
	NetworkInformers networkinformer.SharedInformerFactory

	GenericResourceInformer genericinformers.GenericResourceInformer
	RestMapper              meta.RESTMapper

	// Stop is the stop channel
	Stop <-chan struct{}

	informersStartedLock   sync.Mutex
	informersStartedClosed bool
	// InformersStarted is closed after all of the controllers have been initialized and are running.  After this point it is safe,
	// for an individual controller to start the shared informers. Before it is closed, they should not.
	InformersStarted chan struct{}
}

func (c *ControllerContext) StartInformers(stopCh <-chan struct{}) {
	c.KubernetesInformers.Start(stopCh)
	c.OpenshiftConfigKubernetesInformers.Start(stopCh)

	c.AppsInformers.Start(stopCh)
	c.BuildInformers.Start(stopCh)
	c.ConfigInformers.Start(stopCh)
	c.ImageInformers.Start(stopCh)
	c.NetworkInformers.Start(stopCh)

	c.TemplateInformers.Start(stopCh)
	c.QuotaInformers.Start(stopCh)
	c.RouteInformers.Start(stopCh)

	c.informersStartedLock.Lock()
	defer c.informersStartedLock.Unlock()
	if !c.informersStartedClosed {
		close(c.InformersStarted)
		c.informersStartedClosed = true
	}
}

func (c *ControllerContext) IsControllerEnabled(name string) bool {
	return controllerapp.IsControllerEnabled(name, sets.String{}, c.OpenshiftControllerConfig.Controllers...)
}

type ControllerClientBuilder interface {
	controller.ControllerClientBuilder

	OpenshiftAppsClient(name string) (appsclient.Interface, error)
	OpenshiftAppsClientOrDie(name string) appsclient.Interface

	OpenshiftBuildClient(name string) (buildclient.Interface, error)
	OpenshiftBuildClientOrDie(name string) buildclient.Interface

	OpenshiftConfigClient(name string) (configclient.Interface, error)
	OpenshiftConfigClientOrDie(name string) configclient.Interface

	OpenshiftSecurityClient(name string) (securityclient.Interface, error)
	OpenshiftSecurityClientOrDie(name string) securityclient.Interface

	// OpenShift clients based on generated internal clientsets
	OpenshiftTemplateClient(name string) (templateclient.Interface, error)
	OpenshiftTemplateClientOrDie(name string) templateclient.Interface

	OpenshiftImageClient(name string) (imageclient.Interface, error)
	OpenshiftImageClientOrDie(name string) imageclient.Interface

	OpenshiftQuotaClient(name string) (quotaclient.Interface, error)
	OpenshiftQuotaClientOrDie(name string) quotaclient.Interface

	OpenshiftNetworkClient(name string) (networkclient.Interface, error)
	OpenshiftNetworkClientOrDie(name string) networkclient.Interface
}

// InitFunc is used to launch a particular controller.  It may run additional "should I activate checks".
// Any error returned will cause the controller process to `Fatal`
// The bool indicates whether the controller was enabled.
type InitFunc func(ctx *ControllerContext) (bool, error)

type OpenshiftControllerClientBuilder struct {
	controller.ControllerClientBuilder
}

// OpenshiftInternalTemplateClient provides a REST client for the template API.
// If the client cannot be created because of configuration error, this function
// will return an error.
func (b OpenshiftControllerClientBuilder) OpenshiftTemplateClient(name string) (templateclient.Interface, error) {
	clientConfig, err := b.Config(name)
	if err != nil {
		return nil, err
	}
	return templateclient.NewForConfig(clientConfig)
}

// OpenshiftInternalTemplateClientOrDie provides a REST client for the template API.
// If the client cannot be created because of configuration error, this function
// will panic.
func (b OpenshiftControllerClientBuilder) OpenshiftTemplateClientOrDie(name string) templateclient.Interface {
	client, err := b.OpenshiftTemplateClient(name)
	if err != nil {
		klog.Fatal(err)
	}
	return client
}

// OpenshiftImageClient provides a REST client for the image API.
// If the client cannot be created because of configuration error, this function
// will error.
func (b OpenshiftControllerClientBuilder) OpenshiftImageClient(name string) (imageclient.Interface, error) {
	clientConfig, err := b.Config(name)
	if err != nil {
		return nil, err
	}
	return imageclient.NewForConfig(clientConfig)
}

// OpenshiftImageClientOrDie provides a REST client for the image API.
// If the client cannot be created because of configuration error, this function
// will panic.
func (b OpenshiftControllerClientBuilder) OpenshiftImageClientOrDie(name string) imageclient.Interface {
	client, err := b.OpenshiftImageClient(name)
	if err != nil {
		klog.Fatal(err)
	}
	return client
}

// OpenshiftAppsClient provides a REST client for the apps API.
// If the client cannot be created because of configuration error, this function
// will error.
func (b OpenshiftControllerClientBuilder) OpenshiftAppsClient(name string) (appsclient.Interface, error) {
	clientConfig, err := b.Config(name)
	if err != nil {
		return nil, err
	}
	return appsclient.NewForConfig(clientConfig)
}

// OpenshiftAppsClientOrDie provides a REST client for the apps API.
// If the client cannot be created because of configuration error, this function
// will panic.
func (b OpenshiftControllerClientBuilder) OpenshiftAppsClientOrDie(name string) appsclient.Interface {
	client, err := b.OpenshiftAppsClient(name)
	if err != nil {
		klog.Fatal(err)
	}
	return client
}

// OpenshiftBuildClient provides a REST client for the build  API.
// If the client cannot be created because of configuration error, this function
// will error.
func (b OpenshiftControllerClientBuilder) OpenshiftBuildClient(name string) (buildclient.Interface, error) {
	clientConfig, err := b.Config(name)
	if err != nil {
		return nil, err
	}
	return buildclient.NewForConfig(clientConfig)
}

// OpenshiftBuildClientOrDie provides a REST client for the build API.
// If the client cannot be created because of configuration error, this function
// will panic.
func (b OpenshiftControllerClientBuilder) OpenshiftBuildClientOrDie(name string) buildclient.Interface {
	client, err := b.OpenshiftBuildClient(name)
	if err != nil {
		klog.Fatal(err)
	}
	return client
}

// OpenshiftConfigClient provides a REST client for the build  API.
// If the client cannot be created because of configuration error, this function
// will error.
func (b OpenshiftControllerClientBuilder) OpenshiftConfigClient(name string) (configclient.Interface, error) {
	clientConfig, err := b.Config(name)
	if err != nil {
		return nil, err
	}
	return configclient.NewForConfig(nonProtobufConfig(clientConfig))
}

// OpenshiftConfigClientOrDie provides a REST client for the build API.
// If the client cannot be created because of configuration error, this function
// will panic.
func (b OpenshiftControllerClientBuilder) OpenshiftConfigClientOrDie(name string) configclient.Interface {
	client, err := b.OpenshiftConfigClient(name)
	if err != nil {
		klog.Fatal(err)
	}
	return client
}

func (b OpenshiftControllerClientBuilder) OpenshiftQuotaClient(name string) (quotaclient.Interface, error) {
	clientConfig, err := b.Config(name)
	if err != nil {
		return nil, err
	}
	return quotaclient.NewForConfig(nonProtobufConfig(clientConfig))
}

// OpenshiftInternalBuildClientOrDie provides a REST client for the build API.
// If the client cannot be created because of configuration error, this function
// will panic.
func (b OpenshiftControllerClientBuilder) OpenshiftQuotaClientOrDie(name string) quotaclient.Interface {
	client, err := b.OpenshiftQuotaClient(name)
	if err != nil {
		klog.Fatal(err)
	}
	return client
}

// OpenshiftNetworkClient provides a REST client for the network API.
// If the client cannot be created because of configuration error, this function
// will error.
func (b OpenshiftControllerClientBuilder) OpenshiftNetworkClient(name string) (networkclient.Interface, error) {
	clientConfig, err := b.Config(name)
	if err != nil {
		return nil, err
	}
	return networkclient.NewForConfig(nonProtobufConfig(clientConfig))
}

// OpenshiftNetworkClientOrDie provides a REST client for the network API.
// If the client cannot be created because of configuration error, this function
// will panic.
func (b OpenshiftControllerClientBuilder) OpenshiftNetworkClientOrDie(name string) networkclient.Interface {
	client, err := b.OpenshiftNetworkClient(name)
	if err != nil {
		klog.Fatal(err)
	}
	return client
}

func (b OpenshiftControllerClientBuilder) OpenshiftSecurityClient(name string) (securityclient.Interface, error) {
	clientConfig, err := b.Config(name)
	if err != nil {
		return nil, err
	}
	return securityclient.NewForConfig(clientConfig)
}

func (b OpenshiftControllerClientBuilder) OpenshiftSecurityClientOrDie(name string) securityclient.Interface {
	client, err := b.OpenshiftSecurityClient(name)
	if err != nil {
		klog.Fatal(err)
	}
	return client
}

// nonProtobufConfig returns a copy of inConfig that doesn't force the use of protobufs,
// for working with CRD-based APIs.
func nonProtobufConfig(inConfig *rest.Config) *rest.Config {
	npConfig := rest.CopyConfig(inConfig)
	npConfig.ContentConfig.AcceptContentTypes = "application/json"
	npConfig.ContentConfig.ContentType = "application/json"
	return npConfig
}
