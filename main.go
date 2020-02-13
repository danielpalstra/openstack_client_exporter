package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/common/version"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/acceptance/tools"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/tokens"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	garbageCollectorSleep = 1 * time.Minute

	program     = "openstack_client"
	resourceTag = "openstack-client-exporter"
)

var (
	requestTimeout     time.Duration
	flavorName         string
	imageName          string
	internalNetwork    string
	externalNetwork    string
	userName           string
	disableObjectStore bool
	disableInstance    bool
)

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	registry := prometheus.NewRegistry()

	registry.MustRegister(version.NewCollector("openstack_client_exporter"))
	registry.MustRegister(prometheus.NewGoCollector())

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	wg := sync.WaitGroup{}

	if disableInstance != true {
		// Spawn a server and ssh into it
		wg.Add(1)
		go func() {
			start := time.Now()
			spawnMain(ctx, registry)
			wg.Done()
			log.Printf("spawnMain finished in %v", time.Since(start))
		}()
	}

	if disableObjectStore != true {
		// Upload and download a file from the object store
		wg.Add(1)
		go func() {
			start := time.Now()
			objectStoreMain(ctx, registry)
			wg.Done()
			log.Printf("objectStoreMain finished in %v", time.Since(start))
		}()

		wg.Wait()
	}

	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(w, r)
}

func getProvider(ctx context.Context) (*gophercloud.ProviderClient, error) {
	opts := tokens.AuthOptions{
		Username:   os.Getenv("OS_USERNAME"),
		DomainName: os.Getenv("OS_USER_DOMAIN_NAME"),
		Password:   os.Getenv("OS_PASSWORD"),
		Scope: tokens.Scope{
			ProjectName: os.Getenv("OS_PROJECT_NAME"),
			DomainName:  os.Getenv("OS_PROJECT_DOMAIN_NAME"),
		},
	}

	provider, err := openstack.NewClient(os.Getenv("OS_AUTH_URL"))

	if err != nil {
		return nil, fmt.Errorf("cannot create OpenStack client: %s", err)
	}

	// Progate our current context, this helps cleaning up resources in case of timeout
	provider.Context = ctx

	err = openstack.AuthenticateV3(provider, &opts, gophercloud.EndpointOpts{})

	if err != nil {
		return nil, fmt.Errorf("authentication failure: %s", err)
	}

	return provider, err
}

func step(ctx context.Context, timing prometheus.GaugeVec, name string) error {
	timing.With(prometheus.Labels{"step": name}).SetToCurrentTime()

	select {
	case <-ctx.Done():
		return fmt.Errorf("timeout after %s", name)
	default:
		return nil
	}
}

func createName() string {
	// A timestamp is included in the resource name because it is impossible
	// to get reliable timestamp for all OpenStack resources accross releases

	return resourceTag + "-" + tools.RandomString("", 8) + "-" + strconv.FormatInt(time.Now().Unix(), 10)
}

func main() {
	// Logging configuration
	log.SetFlags(log.Lshortfile)

	// Command line configuration flags

	flag.DurationVar(&requestTimeout, "timeout", 59*time.Second, "maximum timeout for a request")
	flag.StringVar(&flavorName, "flavor", "t2.small", "name of the instance flavor")
	flag.StringVar(&imageName, "image", "ubuntu-16.04-x86_64", "name of the image")
	flag.StringVar(&internalNetwork, "internal-network", "private", "name of the internal network")
	flag.StringVar(&externalNetwork, "external-network", "internet", "name of the external network")
	flag.StringVar(&userName, "user", "ubuntu", "username used for sshing into the instance")
	flag.BoolVar(&disableObjectStore, "disable-objectstore", false, "disable object store")
	flag.BoolVar(&disableInstance, "disable-instance", false, "disable instance")

	flag.Parse()

	// Check environment variables values
	for _, e := range os.Environ() {
		pair := strings.SplitN(e, "=", 2)
		fmt.Println(pair[0])
	}

	// Launch our garbage collector in its own goroutine

	go runGarbageCollector()

	// Handle prometheus metric requests

	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", metricsHandler)
	log.Fatal(http.ListenAndServe("127.0.0.1:9539", mux))
}
