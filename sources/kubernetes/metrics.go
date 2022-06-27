package kubernetes

import (
	"github.com/prometheus/client_golang/prometheus"
)

var (
	// ProcessedContainerUpdates stores the number of processed containers
	ProcessedContainerUpdates = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "kube_eventer",
		Name:      "container_updates_processed_total",
		Help:      "The total number of processed container updates.",
	}, []string{"update_type"})
)

func init() {
	// 初始化OOMKilled监控指标
	prometheus.MustRegister(ProcessedContainerUpdates)
	ProcessedContainerUpdates.WithLabelValues("not_oomkilled").Add(0)
	ProcessedContainerUpdates.WithLabelValues("oomkilled_termination_too_old").Add(0)
	ProcessedContainerUpdates.WithLabelValues("oomkilled_restart_count_unchanged").Add(0)
	ProcessedContainerUpdates.WithLabelValues("oomkilled_event_sent").Add(0)
}
