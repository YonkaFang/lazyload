package controllers

import (
	"context"
	stderrors "errors"
	envoy_config_core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	data_accesslog "github.com/envoyproxy/go-control-plane/envoy/data/accesslog/v3"
	prometheusApi "github.com/prometheus/client_golang/api"
	prometheusV1 "github.com/prometheus/client_golang/api/prometheus/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"slime.io/slime/framework/apis/config/v1alpha1"
	"slime.io/slime/framework/bootstrap"
	"slime.io/slime/framework/model/metric"
	"slime.io/slime/framework/model/trigger"
	"slime.io/slime/framework/util"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	AccessLogConvertorName     = "lazyload-accesslog-convertor"
	MetricSourceTypePrometheus = "prometheus"
	MetricSourceTypeAccesslog  = "accesslog"
)

// call back function for watcher producer
func (r *ServicefenceReconciler) handleWatcherEvent(event trigger.WatcherEvent) metric.QueryMap {

	// check event
	gvks := []schema.GroupVersionKind{
		{Group: "networking.istio.io", Version: "v1beta1", Kind: "Sidecar"},
	}
	invalidEvent := false
	for _, gvk := range gvks {
		if event.GVK == gvk && r.getInterestMeta()[event.NN.String()] {
			invalidEvent = true
		}
	}
	if !invalidEvent {
		return nil
	}

	// generate query map for producer
	qm := make(map[string][]metric.Handler)
	var hs []metric.Handler

	// check metric source type
	switch r.env.Config.Global.Misc["metric_source_type"] {
	case MetricSourceTypePrometheus:
		for pName, pHandler := range r.env.Config.Metric.Prometheus.Handlers {
			hs = append(hs, generateHandler(event.NN.Name, event.NN.Namespace, pName, pHandler))
		}
	case MetricSourceTypeAccesslog:
		hs = []metric.Handler{
			{
				Name:  AccessLogConvertorName,
				Query: "",
			},
		}
	}

	qm[event.NN.String()] = hs
	return qm
}

// call back function for ticker producer
func (r *ServicefenceReconciler) handleTickerEvent(event trigger.TickerEvent) metric.QueryMap {

	// no need to check time duration

	// generate query map for producer
	// check metric source type
	qm := make(map[string][]metric.Handler)

	switch r.env.Config.Global.Misc["metric_source_type"] {
	case MetricSourceTypePrometheus:
		for meta := range r.getInterestMeta() {
			namespace, name := strings.Split(meta, "/")[0], strings.Split(meta, "/")[1]
			var hs []metric.Handler
			for pName, pHandler := range r.env.Config.Metric.Prometheus.Handlers {
				hs = append(hs, generateHandler(name, namespace, pName, pHandler))
			}
			qm[meta] = hs
		}
	case MetricSourceTypeAccesslog:
		for meta := range r.getInterestMeta() {
			qm[meta] = []metric.Handler{
				{
					Name:  AccessLogConvertorName,
					Query: "",
				},
			}
		}
	}

	return qm
}

func generateHandler(name, namespace, pName string, pHandler *v1alpha1.Prometheus_Source_Handler) metric.Handler {
	query := strings.ReplaceAll(pHandler.Query, "$namespace", namespace)
	query = strings.ReplaceAll(query, "$source_app", name)
	return metric.Handler{Name: pName, Query: query}
}

func newProducerConfig(env bootstrap.Environment) (*metric.ProducerConfig, error) {

	// init metric source
	var enablePrometheusSource bool
	var prometheusSourceConfig metric.PrometheusSourceConfig
	var accessLogSourceConfig metric.AccessLogSourceConfig
	var err error

	switch env.Config.Global.Misc["metric_source_type"] {
	case MetricSourceTypePrometheus:
		enablePrometheusSource = true
		prometheusSourceConfig, err = newPrometheusSourceConfig(env)
		if err != nil {
			return nil, err
		}
	case MetricSourceTypeAccesslog:
		enablePrometheusSource = false
		// init log source port
		port := env.Config.Global.Misc["log_source_port"]

		ipToSvcCache, cacheLock, err := newIpToSvcCache(env.K8SClient)
		if err != nil {
			return nil, err
		}
		accessLogSourceConfig = metric.AccessLogSourceConfig{
			ServePort: port,
			AccessLogConvertorConfigs: []metric.AccessLogConvertorConfig{
				{
					Name: AccessLogConvertorName,
					Handler: func(logEntry []*data_accesslog.HTTPAccessLogEntry) (map[string]map[string]string, error) {
						return accessLogHandler(logEntry, ipToSvcCache, cacheLock)
					},
				},
			},
		}
	default:
		return nil, stderrors.New("wrong metric_source_type")
	}

	// init whole producer config
	pc := &metric.ProducerConfig{
		EnablePrometheusSource: enablePrometheusSource,
		PrometheusSourceConfig: prometheusSourceConfig,
		AccessLogSourceConfig:  accessLogSourceConfig,
		EnableWatcherProducer:  true,
		WatcherProducerConfig: metric.WatcherProducerConfig{
			Name:       "lazyload-watcher",
			MetricChan: make(chan metric.Metric),
			WatcherTriggerConfig: trigger.WatcherTriggerConfig{
				Kinds: []schema.GroupVersionKind{
					{
						Group:   "networking.istio.io",
						Version: "v1beta1",
						Kind:    "Sidecar",
					},
				},
				EventChan:     make(chan trigger.WatcherEvent),
				DynamicClient: env.DynamicClient,
			},
		},
		EnableTickerProducer: true,
		TickerProducerConfig: metric.TickerProducerConfig{
			Name:       "lazyload-ticker",
			MetricChan: make(chan metric.Metric),
			TickerTriggerConfig: trigger.TickerTriggerConfig{
				Durations: []time.Duration{
					30 * time.Second,
				},
				EventChan: make(chan trigger.TickerEvent),
			},
		},
		StopChan: env.Stop,
	}

	return pc, nil

}

func newPrometheusSourceConfig(env bootstrap.Environment) (metric.PrometheusSourceConfig, error) {
	ps := env.Config.Metric.Prometheus
	if ps == nil {
		return metric.PrometheusSourceConfig{}, stderrors.New("failure create prometheus client, empty prometheus config")
	}
	promClient, err := prometheusApi.NewClient(prometheusApi.Config{
		Address:      ps.Address,
		RoundTripper: nil,
	})
	if err != nil {
		return metric.PrometheusSourceConfig{}, err
	}

	return metric.PrometheusSourceConfig{
		Api: prometheusV1.NewAPI(promClient),
	}, nil
}

func accessLogHandler(logEntry []*data_accesslog.HTTPAccessLogEntry, ipToSvcCache map[string]string, cacheLock *sync.RWMutex) (map[string]map[string]string, error) {
	log := log.WithField("reporter", "accesslog convertor").WithField("function", "accessLogHandler")
	result := make(map[string]map[string]string)

	tmpResult := make(map[string]map[string]int)
	for _, entry := range logEntry {
		//tmpValue := make(map[string]int)

		// fetch sourceEp
		sourceIp, err := fetchSourceIp(entry)
		if err != nil {
			return nil, err
		}
		if sourceIp == "" {
			continue
		}

		// fetch sourceSvcMeta
		sourceSvc, err := spliceSourceSvc(sourceIp, ipToSvcCache, cacheLock)
		if err != nil {
			return nil, err
		}
		if sourceSvc == "" {
			continue
		}

		// fetch destinationSvcMeta
		destinationSvc := spliceDestinationSvc(entry)
		if destinationSvc == "" {
			continue
		}

		// push result
		if dstSvcMappings, ok := tmpResult[sourceSvc]; !ok {
			tmpValue := make(map[string]int)
			tmpValue[destinationSvc] = 1
			tmpResult[sourceSvc] = tmpValue
		} else {
			dstSvcMappings[destinationSvc] += 1
		}

		log.Debugf("tmpResult[%s][%s]: %d", sourceSvc, destinationSvc, tmpResult[sourceSvc][destinationSvc])
	}

	for sourceSvc, dstSvcMappings := range tmpResult {
		result[sourceSvc] = make(map[string]string)
		for dstSvc, count := range dstSvcMappings {
			result[sourceSvc][dstSvc] = strconv.Itoa(count)
		}
	}

	return result, nil
}

func fetchSourceIp(entry *data_accesslog.HTTPAccessLogEntry) (string, error) {
	log := log.WithField("reporter", "accesslog convertor").WithField("function", "fetchSourceIp")
	if entry.CommonProperties.DownstreamRemoteAddress == nil {
		log.Debugf("DownstreamRemoteAddress is nil, skip")
		return "", nil
	}
	downstreamSock, ok := entry.CommonProperties.DownstreamRemoteAddress.Address.(*envoy_config_core.Address_SocketAddress)
	if !ok {
		return "", stderrors.New("wrong type of DownstreamRemoteAddress")
	}
	if downstreamSock == nil || downstreamSock.SocketAddress == nil {
		return "", stderrors.New("downstream socket address is nil")
	}
	log.Debugf("SourceEp is: %s", downstreamSock.SocketAddress.Address)
	return downstreamSock.SocketAddress.Address, nil
}

func spliceSourceSvc(sourceIp string, ipToSvcCache map[string]string, cacheLock *sync.RWMutex) (string, error) {
	cacheLock.RLock()
	defer cacheLock.RUnlock()

	for ip, svc := range ipToSvcCache {
		if sourceIp == ip {
			return svc, nil
		}
	}

	return "", nil
}

func spliceDestinationSvc(entry *data_accesslog.HTTPAccessLogEntry) string {
	log := log.WithField("reporter", "accesslog convertor").WithField("function", "spliceDestinationSvc")
	upstreamCluster := entry.CommonProperties.UpstreamCluster
	parts := strings.Split(upstreamCluster, "|")
	if len(parts) != 4 {
		log.Debugf("UpstreamCluster parts number is not 4, skip")
		return ""
	}
	if parts[0] != "outbound" {
		log.Debugf("UpstreamCluster parts[0] is not outbound, skip")
		return ""
	}
	parts = strings.Split(parts[3], ".")
	log.Debugf("DestinationSvc is: %s", "{destination_service=\""+parts[0]+"."+parts[1]+".svc.cluster.local"+"\"}")
	return "{destination_service=\"" + parts[0] + "." + parts[1] + ".svc.cluster.local" + "\"}"
}

func newIpToSvcCache(clientSet *kubernetes.Clientset) (map[string]string, *sync.RWMutex, error) {
	log := log.WithField("reporter", "AccessLogConvertor").WithField("function", "generateSvcToIpsCache")
	ipToSvcCache := make(map[string]string)
	svcToIpsCache := make(map[string][]string)
	var cacheLock sync.RWMutex

	// init svcToIps
	eps, err := clientSet.CoreV1().Endpoints("").List(metav1.ListOptions{})
	if err != nil {
		return nil, nil, stderrors.New("failed to get endpoints list")
	}

	for _, ep := range eps.Items {
		svc := ep.GetNamespace() + "/" + ep.GetName()
		var addresses []string
		for _, subset := range ep.Subsets {
			for _, address := range subset.Addresses {
				addresses = append(addresses, address.IP)
				ipToSvcCache[address.IP] = svc
			}
		}
		svcToIpsCache[svc] = addresses
	}

	// init endpoint watcher
	epsClient := clientSet.CoreV1().Endpoints("")
	lw := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return epsClient.List(options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return epsClient.Watch(options)
		},
	}
	watcher := util.ListWatcher(context.Background(), lw)

	go func() {
		for {
			e, ok := <-watcher.ResultChan()
			if !ok {
				log.Warningf("a result chan of endpoint watcher is closed, break process loop")
				return
			}

			ep, ok := e.Object.(*v1.Endpoints)
			if !ok {
				log.Errorf("invalid type of object in watcher event")
				continue
			}

			svc := ep.GetNamespace() + "/" + ep.GetName()
			// delete event
			if e.Type == watch.Deleted {
				cacheLock.Lock()
				for _, ip := range svcToIpsCache[svc] {
					delete(ipToSvcCache, ip)
				}
				delete(svcToIpsCache, svc)
				cacheLock.Unlock()
				continue
			}

			// add, update event
			ep, err := clientSet.CoreV1().Endpoints(ep.GetNamespace()).Get(ep.GetName(), metav1.GetOptions{})
			if err != nil {
				continue
			}
			// delete previous key, value
			cacheLock.Lock()
			for _, ip := range svcToIpsCache[svc] {
				delete(ipToSvcCache, ip)
			}
			// add new key, value
			var addresses []string
			for _, subset := range ep.Subsets {
				for _, address := range subset.Addresses {
					addresses = append(addresses, address.IP)
					ipToSvcCache[address.IP] = svc
				}
			}
			svcToIpsCache[svc] = addresses
			cacheLock.Unlock()

		}
	}()

	return ipToSvcCache, &cacheLock, nil
}
