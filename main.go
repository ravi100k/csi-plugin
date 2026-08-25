/*
Copyright 2019 Hammerspace

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package main

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hammer-space/csi-plugin/pkg/common"
	"github.com/hammer-space/csi-plugin/pkg/driver"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	metric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// DefaultLogLevel is the level used when LOG_LEVEL is unset or invalid.
const DefaultLogLevel = common.DefaultLogLevel

func init() {
	// Setup logging. Emit one JSON object per line: log collectors (Loki, ELK,
	// CloudWatch) parse container output line by line, so indented multi-line
	// JSON is read as several unparseable fragments per entry.
	common.ConfigureJSONLogging()
	log.SetReportCaller(false)
	// Initialize OpenTelemetry (traces + metrics), configured via env vars.
	if err := initTelemetry(); err != nil {
		log.Fatalf("failed to init telemetry: %v", err)
	}
}

// parseLogLevel resolves the LOG_LEVEL environment variable to a log level,
// accepting the usual names (panic, fatal, error, warn, info, debug, trace).
// An unset or unparseable value falls back to info.
//
// Note that debug logs every Anvil REST call, so it is verbose under load and
// is intended for troubleshooting rather than steady-state operation.
func parseLogLevel(level string) log.Level {
	return common.ParseLogLevel(level)
}

// initTelemetry wires OTel providers according to standard env vars:
//
//	OTEL_TRACES_EXPORTER   = none | console | otlp    (default: none)
//	OTEL_METRICS_EXPORTER  = none | prometheus | otlp (default: none)
//	OTEL_EXPORTER_OTLP_ENDPOINT = host:4317           (used when *_EXPORTER=otlp)
//	OTEL_METRICS_PROMETHEUS_LISTEN = :9090            (Prometheus scrape port)
//
// With defaults, both providers are no-ops so instrumentation stays cheap.
func initTelemetry() error {
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("hammerspace-csi"),
		semconv.ServiceVersion(common.Version),
	))
	if err != nil {
		return err
	}

	// ---- Traces ----
	switch os.Getenv("OTEL_TRACES_EXPORTER") {
	case "console":
		exp, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return err
		}
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res)))
		log.Info("OTel traces: stdouttrace exporter enabled")
	case "otlp":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		exp, err := otlptracegrpc.New(ctx)
		if err != nil {
			return err
		}
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res)))
		log.Infof("OTel traces: otlp exporter -> %s", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	default:
		log.Info("OTel traces: disabled (OTEL_TRACES_EXPORTER=none)")
	}
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// ---- Metrics ----
	switch os.Getenv("OTEL_METRICS_EXPORTER") {
	case "prometheus":
		promExp, err := prometheus.New()
		if err != nil {
			return err
		}
		otel.SetMeterProvider(metric.NewMeterProvider(metric.WithReader(promExp), metric.WithResource(res)))
		listen := os.Getenv("OTEL_METRICS_PROMETHEUS_LISTEN")
		if listen == "" {
			listen = ":9090"
		}
		go func() {
			mux := http.NewServeMux()
			mux.Handle("/metrics", promhttp.Handler())
			mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
			log.Infof("OTel metrics: prometheus /metrics listening on %s", listen)
			if err := http.ListenAndServe(listen, mux); err != nil {
				log.Errorf("prometheus /metrics listener failed: %v", err)
			}
		}()
	case "otlp":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		exp, err := otlpmetricgrpc.New(ctx)
		if err != nil {
			return err
		}
		otel.SetMeterProvider(metric.NewMeterProvider(
			metric.WithReader(metric.NewPeriodicReader(exp, metric.WithInterval(30*time.Second))),
			metric.WithResource(res),
		))
		log.Infof("OTel metrics: otlp exporter -> %s", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	default:
		log.Info("OTel metrics: disabled (OTEL_METRICS_EXPORTER=none)")
	}
	return nil
}

func validateEnvironmentVars() {
	endpoint := os.Getenv("CSI_ENDPOINT")
	if len(endpoint) == 0 {
		log.Error("CSI_ENDPOINT must be defined and must be a path")
		os.Exit(1)
	}
	if strings.Contains(endpoint, ":") {
		log.Error("CSI_ENDPOINT must be a unix path")
		os.Exit(1)
	}

	hsEndpoint := os.Getenv("HS_ENDPOINT")
	if len(hsEndpoint) == 0 {
		log.Error("HS_ENDPOINT must be defined")
		os.Exit(1)
	}

	endpointUrl, err := url.Parse(hsEndpoint)
	if err != nil || endpointUrl.Scheme != "https" || endpointUrl.Host == "" {
		log.Error("HS_ENDPOINT must be a valid HTTPS URL")
		os.Exit(1)
	}

	username := os.Getenv("HS_USERNAME")
	if len(username) == 0 {
		log.Error("HS_USERNAME must be defined")
		os.Exit(1)
	}

	password := os.Getenv("HS_PASSWORD")
	if len(password) == 0 {
		log.Error("HS_PASSWORD must be defined")
		os.Exit(1)
	}

	if os.Getenv("HS_TLS_VERIFY") != "" {
		_, err = strconv.ParseBool(os.Getenv("HS_TLS_VERIFY"))
		if err != nil {
			log.Error("HS_TLS_VERIFY must be a bool")
			os.Exit(1)
		}
	}
	if caBundle := os.Getenv("HS_CA_BUNDLE"); caBundle != "" {
		if _, statErr := os.Stat(caBundle); statErr != nil {
			log.Errorf("HS_CA_BUNDLE must reference a readable CA bundle: %v", statErr)
			os.Exit(1)
		}
	}

	csiMajorVersion := os.Getenv("CSI_MAJOR_VERSION")
	if csiMajorVersion != "0" && csiMajorVersion != "1" {
		log.Error("CSI_MAJOR_VERSION must be set to \"0\" or \"1\"")
		os.Exit(1)
	}

	common.DataPortalMountPrefix = os.Getenv("HS_DATA_PORTAL_MOUNT_PREFIX")
}

type Server interface {
	Start(net.Listener) error
	Stop()
}

func runControllerLeaderElection(csiDriver *driver.CSIDriver) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("controller leader election requires in-cluster Kubernetes configuration: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("create Kubernetes client for leader election: %v", err)
	}
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		namespace = "kube-system"
	}
	identity := os.Getenv("POD_NAME")
	if identity == "" {
		identity, err = os.Hostname()
		if err != nil {
			log.Fatalf("determine leader-election identity: %v", err)
		}
	}
	lockName := os.Getenv("CSI_LEADER_ELECTION_LEASE_NAME")
	if lockName == "" {
		lockName = "hammerspace-csi-controller"
	}

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{Name: lockName, Namespace: namespace},
		Client:    clientset.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: identity,
		},
	}
	leaderelection.RunOrDie(context.Background(), leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		ReleaseOnCancel: true,
		Name:            "hammerspace-csi-controller",
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leaderCtx context.Context) {
				recoveryCtx, cancel := context.WithTimeout(leaderCtx, 2*time.Minute)
				if err := csiDriver.RecoverFrozenTargets(recoveryCtx); err != nil {
					log.Errorf("frozen-target recovery completed with errors: %v", err)
				}
				cancel()
				csiDriver.SetControllerLeader(true)
				log.Infof("acquired controller leadership as %s", identity)
			},
			OnStoppedLeading: func() {
				csiDriver.SetControllerLeader(false)
				// Continuing after losing the lease could permit two controllers to
				// mutate backend state. Terminate so Kubernetes restarts the pod.
				log.Fatal("lost controller leadership")
			},
			OnNewLeader: func(newIdentity string) {
				if newIdentity != identity {
					log.Infof("controller leader is now %s", newIdentity)
				}
			},
		},
	})
}

func main() {

	validateEnvironmentVars()

	var server Server

	CSI_version := os.Getenv("CSI_MAJOR_VERSION")
	endpoint := os.Getenv("CSI_ENDPOINT")

	csiDriver := driver.NewCSIDriver(
		os.Getenv("HS_ENDPOINT"),
		os.Getenv("HS_USERNAME"),
		os.Getenv("HS_PASSWORD"),
		os.Getenv("HS_TLS_VERIFY"),
	)
	if os.Getenv("CSI_NODE_NAME") == "" {
		leaderElection, parseErr := strconv.ParseBool(os.Getenv("CSI_LEADER_ELECTION"))
		if parseErr == nil && leaderElection {
			// Close the startup window before the election goroutine is scheduled:
			// controller RPCs remain gated until this replica owns the Lease.
			csiDriver.EnableControllerLeaderElection()
			go runControllerLeaderElection(csiDriver)
		}
	}

	if CSI_version == "0" {
		server = driver.NewCSIDriver_v0Support(csiDriver)
		common.CsiVersion = "0"
	} else {
		server = csiDriver
	}

	// Listen
	os.Remove(endpoint)
	l, err := net.Listen("unix", endpoint)
	if err != nil {
		log.Errorf("Error: Unable to listen on %s socket: %v\n",
			endpoint,
			err)
		os.Exit(1)
	}
	defer os.Remove(endpoint)

	// Start server
	if err := server.Start(l); err != nil {
		log.Errorf("Error: Unable to start CSI server: %v\n",
			err)
		os.Exit(1)
	}
	log.Info("hammerspace driver started")

	// Wait for signal
	sigc := make(chan os.Signal, 1)
	sigs := []os.Signal{
		syscall.SIGTERM,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGQUIT,
	}
	signal.Notify(sigc, sigs...)

	<-sigc
	server.Stop()
	log.Info("hammerspace driver stopped")
}
