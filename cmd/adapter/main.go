package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/util/logs"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/draios/kubernetes-sysdig-metrics-apiserver/internal/cmprovider"
	"github.com/draios/kubernetes-sysdig-metrics-apiserver/internal/sdc"

	// Temporar hack until I can vendor it.
	_cma_server "github.com/draios/kubernetes-sysdig-metrics-apiserver/internal/custom-metrics-apiserver/pkg/cmd/server"
	_cma_dynamicmapper "github.com/draios/kubernetes-sysdig-metrics-apiserver/internal/custom-metrics-apiserver/pkg/dynamicmapper"
)

// This is the name associated to our CustomMetricsAdapterServer.
const customMetricAdapterName = "sysdig-custom-metrics-adapter"

func main() {
	// Workaround for this issue: https://github.com/kubernetes/kubernetes/issues/17162.
	flag.CommandLine.Parse([]string{})

	// Initialize the log configuration the way that Kubernetes likes.
	logs.InitLogs()
	defer logs.FlushLogs()

	cmd := command(os.Stdout, os.Stderr, wait.NeverStop)
	cmd.Flags().AddGoFlagSet(flag.CommandLine)
	if err := cmd.Execute(); err != nil {
		panic(err)
	}
}

func command(out, errOut io.Writer, stopCh <-chan struct{}) *cobra.Command {
	baseOpts := _cma_server.NewCustomMetricsAdapterServerOptions(out, errOut)
	o := adapterOpts{
		CustomMetricsAdapterServerOptions: baseOpts,
		DiscoveryInterval:                 10 * time.Minute,
	}

	cmd := &cobra.Command{
		Short: "Launch the custom metrics API adapter server",
		Long:  "Launch the custom metrics API adapter server",
		RunE: func(c *cobra.Command, args []string) error {
			if err := o.Complete(); err != nil {
				return err
			}
			if err := o.Validate(args); err != nil {
				return err
			}
			return o.runCustomMetricsAdapterServer(stopCh)
		},
	}

	flags := cmd.Flags()
	o.SecureServing.AddFlags(flags)
	o.Authentication.AddFlags(flags)
	o.Authorization.AddFlags(flags)
	o.Features.AddFlags(flags)

	flags.StringVar(&o.RemoteKubeConfigFile, "lister-kubeconfig", o.RemoteKubeConfigFile,
		"kubeconfig file pointing at the 'core' kubernetes server with enough rights to list any described objets")
	flags.DurationVar(&o.DiscoveryInterval, "discovery-interval", o.DiscoveryInterval,
		"interval at which to refresh API discovery information")

	flags.StringVar(&o.Metric, "metric", o.Metric,
		"metric name as listed in Sysdig Monitor, e.g.: net.http.request.count")
	flags.StringVar(&o.Namespace, "namespace", o.Namespace,
		"namespace where the object is found, e.g.: default")
	flags.StringVar(&o.Service, "service", o.Service,
		"service object name, e.g.: kuard")

	return cmd
}

type adapterOpts struct {
	*_cma_server.CustomMetricsAdapterServerOptions

	// RemoteKubeConfigFile is the config used to list pods from the master API server
	RemoteKubeConfigFile string

	// DiscoveryInterval is the interval at which discovery information is refreshed
	DiscoveryInterval time.Duration

	// Metric is the name of the Sysdig Monitor metric that this server is using.
	Metric string

	// Namespace of the service object that this server is using.
	Namespace string

	// Service is the name of the service object that this server is using.
	Service string
}

// runCustomMetricsAdapterServer runs our CustomMetricsAdapterServer.
func (o adapterOpts) runCustomMetricsAdapterServer(stopCh <-chan struct{}) error {
	// Sysdig API client configuration.
	var token = os.Getenv("SDC_TOKEN")
	if token == "" {
		return errors.New("Sysdig Monitor API token not provided - pass it via environment string SDC_TOKEN")
	}
	var options []sdc.ClientOpt
	if ep := os.Getenv("SDC_ENDPOINT"); ep != "" {
		options = append(options, sdc.SetBaseURL(ep))
	}
	sysdigClient, err := sdc.New(nil, token, options...)
	if err != nil {
		return err
	}

	// Kubernetes configuration.
	config, err := o.Config()
	if err != nil {
		fmt.Println(err)
		return err
	}
	config.GenericConfig.EnableMetrics = true

	var clientConfig *rest.Config
	if len(o.RemoteKubeConfigFile) > 0 {
		loadingRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: o.RemoteKubeConfigFile}
		loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})
		clientConfig, err = loader.ClientConfig()
	} else {
		clientConfig, err = rest.InClusterConfig()
	}
	if err != nil {
		return fmt.Errorf("unable to construct lister client config to initialize provider: %v", err)
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(clientConfig)
	if err != nil {
		return fmt.Errorf("unable to construct discovery client for dynamic client: %v", err)
	}

	dynamicMapper, err := _cma_dynamicmapper.NewRESTMapper(discoveryClient, apimeta.InterfacesForUnstructured, o.DiscoveryInterval)
	if err != nil {
		return fmt.Errorf("unable to construct dynamic discovery mapper: %v", err)
	}

	clientPool := dynamic.NewClientPool(clientConfig, dynamicMapper, dynamic.LegacyAPIPathResolverFunc)
	if err != nil {
		return fmt.Errorf("unable to construct lister client to initialize provider: %v", err)
	}

	server, err := config.Complete().New(
		// Name of the CustomMetricsAdapterServer (for logging purposes).
		customMetricAdapterName,
		// CustomMetricsProvider.
		cmprovider.NewSysdigProvider(dynamicMapper, clientPool, sysdigClient, o.Metric, o.Namespace, o.Service, stopCh),
		// ExternalMetricsProvider (which we're not implementing)
		nil,
	)
	if err != nil {
		return err
	}
	return server.GenericAPIServer.PrepareRun().Run(stopCh)
}
