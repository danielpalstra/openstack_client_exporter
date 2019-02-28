package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/prometheus/common/version"

	"github.com/gophercloud/gophercloud"

	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/tokens"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	flavorName          = "t2.small"
	imageName           = "ubuntu-16.04-x86_64"
	networkName         = "private"
	externalNetworkName = "internet"

	defaultRequestTimeout       = 59 * time.Second
	garbageCollectorSleep       = 1 * time.Minute
	garbageCollectorResourceAge = 15 * time.Minute

	program     = "openstack_client"
	resourceTag = "openstack-client-exporter"
)

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	requestTimeout := defaultRequestTimeout

	timeoutParam := r.URL.Query().Get("timeout")

	if timeoutParam != "" {
		value, err := time.ParseDuration(timeoutParam)

		if err == nil {
			requestTimeout = value
			log.Printf("Setting request timeout to user provided value of %s", requestTimeout)
		}
	}

	registry := prometheus.NewRegistry()

	registry.MustRegister(version.NewCollector("openstack_client_exporter"))

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	wg := sync.WaitGroup{}

	// Spawn a server and ssh into it
	wg.Add(1)
	go func() {
		spawnMain(ctx, registry)
		wg.Done()
	}()

	// Upload and download a file from the object store
	wg.Add(1)
	go func() {
		objectStoreMain(ctx, registry)
		wg.Done()
	}()

	wg.Wait()

	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(w, r)
}

func getProvider() (*gophercloud.ProviderClient, error) {
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

func main() {
	// Launch our garbage collector in its own goroutine

	go runGarbageCollector()

	// Handle prometheus metric requests

	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", metricsHandler)
	log.Fatal(http.ListenAndServe("127.0.0.1:9539", mux))
}
