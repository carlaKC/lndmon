package collectors

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// log configuration defaults.
	defaultLogFilename = "lndmon.log"
	defaultLogFileSize = 10
	defaultMaxLogFile  = 10
	defaultLndmonDir   = btcutil.AppDataDir("lndmon", false)
)

// PrometheusExporter is a metric exporter that exports relevant lnd metrics
// such as routing policies to track how Lightning fees change over time.
type PrometheusExporter struct {
	cfg *PrometheusConfig

	lnd *lndclient.LndServices

	monitoringCfg *MonitoringConfig

	// collectors is the exporter's active set of collectors.
	collectors []prometheus.Collector

	// errChan is an error channel that we receive errors from our
	// collectors on.
	errChan <-chan error
}

// PrometheusConfig is the set of configuration data that specifies the
// listening address of the Prometheus server and configuration for lndmon logs.
type PrometheusConfig struct {
	// ListenAddr is the listening address that we should use to allow the
	// main Prometheus server to scrape our metrics.
	ListenAddr string `long:"listenaddr" description:"the interface we should listen on for prometheus"`

	// LogDir is the directory to log lndmon output.
	LogDir string `long:"logdir" description:"Directory to log output"`

	// MaxLogFiles is the maximum number of log files to keep (0 for no
	// rotation).
	MaxLogFiles int `long:"maxlogfiles" description:"Maximum log files to keep (0 for no rotation)"`

	// MaxLogFileSize is the maximum log file size in MB.
	MaxLogFileSize int `long:"maxlogfilesize" description:"Maximum log file size in MB"`
}

// MonitoringConfig contains information that specifies how to monitor the node.
type MonitoringConfig struct {
	// PrimaryNode is the pubkey of the primary node in primary-gateway
	// setups.
	PrimaryNode *route.Vertex
}

func DefaultConfig() *PrometheusConfig {
	return &PrometheusConfig{
		ListenAddr:     "localhost:9092",
		LogDir:         filepath.Join(defaultLndmonDir, "logs"),
		MaxLogFiles:    3,
		MaxLogFileSize: 10,
	}
}

// NewPrometheusExporter makes a new instance of the PrometheusExporter given
// the address to listen for Prometheus on and an lnd gRPC client.
func NewPrometheusExporter(cfg *PrometheusConfig, lnd *lndclient.LndServices,
	monitoringCfg *MonitoringConfig) *PrometheusExporter {

	// We have six collectors, so we buffer our error channel by 6 so that
	// we do not need to consume all errors from this channel (on the first
	// one, we'll start shutting down, but a few could arrive quickly).
	errChan := make(chan error, 6)

	return &PrometheusExporter{
		cfg:           cfg,
		lnd:           lnd,
		monitoringCfg: monitoringCfg,
		collectors: []prometheus.Collector{
			NewChainCollector(lnd.Client, errChan),
			NewChannelsCollector(
				lnd.Client, errChan, monitoringCfg,
			),
			NewWalletCollector(lnd, errChan),
			NewGraphCollector(lnd.Client, errChan),
			NewPeerCollector(lnd.Client, errChan),
			NewInfoCollector(lnd.Client, errChan),
		},
		errChan: errChan,
	}
}

// Start registers all relevant metrics with the Prometheus library, then
// launches the HTTP server that Prometheus will hit to scrape our metrics.
func (p *PrometheusExporter) Start() error {
	err := initLogRotator(
		filepath.Join(p.cfg.LogDir, defaultLogFilename),
		defaultLogFileSize,
		defaultMaxLogFile,
	)
	if err != nil {
		return err
	}

	Logger.Info("Starting Prometheus exporter...")
	if p.lnd == nil {
		return fmt.Errorf("cannot start PrometheusExporter without " +
			"backing Lightning node to pull metrics from")
	}

	// Next, we'll attempt to register all our metrics. If we fail to
	// register ANY metric, then we'll fail all together.
	if err := p.registerMetrics(); err != nil {
		return err
	}

	// Finally, we'll launch the HTTP server that Prometheus will use to
	// scape our metrics.
	go func() {
		errorLogger := log.New(
			os.Stdout, "promhttp", log.Ldate|log.Ltime|log.Lshortfile,
		)

		promHandler := promhttp.InstrumentMetricHandler(
			prometheus.DefaultRegisterer,
			promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{
				ErrorLog:      errorLogger,
				ErrorHandling: promhttp.ContinueOnError,
			}),
		)
		http.Handle("/metrics", promHandler)
		Logger.Info(http.ListenAndServe(p.cfg.ListenAddr, nil))
	}()

	Logger.Info("Prometheus active!")

	return nil
}

// Errors returns an error channel that any failures experienced by its
// collectors experience.
func (p *PrometheusExporter) Errors() <-chan error {
	return p.errChan
}

// registerMetrics iterates through all the registered collectors and attempts
// to register each one. If any of the collectors fail to register, then an
// error will be returned.
func (p *PrometheusExporter) registerMetrics() error {
	for _, collector := range p.collectors {
		err := prometheus.Register(collector)
		if err != nil {
			return err
		}
	}

	return nil
}
