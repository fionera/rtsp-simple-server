package metrics

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/aler9/rtsp-simple-server/internal/logger"
	"github.com/aler9/rtsp-simple-server/internal/stats"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Parent is implemented by program.
type Parent interface {
	Log(logger.Level, string, ...interface{})
}

// Metrics is a metrics exporter.
type Metrics struct {
	stats *stats.Stats

	listener net.Listener
	mux      *http.ServeMux
	server   *http.Server
}

var (
	rtspClientsDesc = prometheus.NewDesc("rtsp_clients", "A Gauge displaying the currently connected client", []string{"state"}, nil)
	rtspSourcesDesc = prometheus.NewDesc("rtsp_sources", "A Gauge displaying the currently connected sources", []string{"type", "state"}, nil)

	ReceivedDataCounter = promauto.NewCounter(prometheus.CounterOpts{Name: "received_data", Help: "The Sum of all transmitted data"})
)

func (m *Metrics) Describe(descs chan<- *prometheus.Desc) {
	descs <- rtspClientsDesc
	descs <- rtspSourcesDesc
}

func (m *Metrics) Collect(metrics chan<- prometheus.Metric) {
	metrics <- prometheus.MustNewConstMetric(rtspClientsDesc, prometheus.GaugeValue, float64(atomic.LoadInt64(m.stats.CountPublishers)), "publishing")
	metrics <- prometheus.MustNewConstMetric(rtspClientsDesc, prometheus.GaugeValue, float64(atomic.LoadInt64(m.stats.CountReaders)), "reading")
	metrics <- prometheus.MustNewConstMetric(rtspSourcesDesc, prometheus.GaugeValue, float64(atomic.LoadInt64(m.stats.CountSourcesRTSP)), "rtsp", "idle")
	metrics <- prometheus.MustNewConstMetric(rtspSourcesDesc, prometheus.GaugeValue, float64(atomic.LoadInt64(m.stats.CountSourcesRTSPRunning)), "rtsp", "running")
	metrics <- prometheus.MustNewConstMetric(rtspSourcesDesc, prometheus.GaugeValue, float64(atomic.LoadInt64(m.stats.CountSourcesRTMP)), "rtmp", "idle")
	metrics <- prometheus.MustNewConstMetric(rtspSourcesDesc, prometheus.GaugeValue, float64(atomic.LoadInt64(m.stats.CountSourcesRTMPRunning)), "rtmp", "running")
}

// New allocates a metrics.
func New(
	address string,
	stats *stats.Stats,
	parent Parent,
) (*Metrics, error) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	m := &Metrics{
		stats: stats,
		server: &http.Server{
			Addr:    address,
			Handler: mux,
		},
	}

	if err := prometheus.Register(m); err != nil {
		return nil, err
	}

	parent.Log(logger.Info, "[metrics] opened on "+address)

	go m.run()
	return m, nil
}

// Close closes a Metrics.
func (m *Metrics) Close() {
	m.server.Shutdown(context.Background())
}

func (m *Metrics) run() {
	err := m.server.ListenAndServe()
	if err != http.ErrServerClosed {
		panic(err)
	}
}
