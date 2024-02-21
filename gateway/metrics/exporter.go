// Copyright (c) Alex Ellis 2017
// Copyright (c) 2018 OpenFaaS Author(s)
// Licensed under the MIT license. See LICENSE file in the project root for full license information.

package metrics

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"time"

	"log"

	"github.com/openfaas/faas-provider/auth"
	types "github.com/openfaas/faas-provider/types"
	"github.com/prometheus/client_golang/prometheus"
)

// Exporter is a prometheus exporter
type Exporter struct {
	metricOptions     MetricOptions
	services          []types.FunctionStatus
	credentials       *auth.BasicAuthCredentials
	FunctionNamespace string

	// 加这个，用来查询prometheus
	prometheusQuery PrometheusQueryFetcher
}

// NewExporter creates a new exporter for the OpenFaaS gateway metrics
func NewExporter(options MetricOptions, credentials *auth.BasicAuthCredentials, namespace string, prometheusQuery PrometheusQueryFetcher) *Exporter {
	return &Exporter{
		metricOptions:     options,
		services:          []types.FunctionStatus{},
		credentials:       credentials,
		FunctionNamespace: namespace,
		// 加这个
		prometheusQuery: prometheusQuery,
	}
}

// Describe is to describe the metrics for Prometheus
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {

	e.metricOptions.GatewayFunctionInvocation.Describe(ch)
	e.metricOptions.GatewayFunctionsHistogram.Describe(ch)
	e.metricOptions.ServiceReplicasGauge.Describe(ch)
	e.metricOptions.GatewayFunctionInvocationStarted.Describe(ch)

	// e.metricOptions.GatewayFunctionRequestHistogram.Describe(ch)
	e.metricOptions.GatewayFunctionRequestSummary.Describe(ch)
	e.metricOptions.PodCpuUsageSecondsTotal.Describe(ch)
	e.metricOptions.PodMemoryWorkingSetBytes.Describe(ch)
}

// Collect collects data to be consumed by prometheus
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.metricOptions.GatewayFunctionInvocation.Collect(ch)
	e.metricOptions.GatewayFunctionsHistogram.Collect(ch)

	e.metricOptions.GatewayFunctionInvocationStarted.Collect(ch)

	e.metricOptions.ServiceReplicasGauge.Reset()

	for _, service := range e.services {
		var serviceName string
		if len(service.Namespace) > 0 {
			serviceName = fmt.Sprintf("%s.%s", service.Name, service.Namespace)
		} else {
			serviceName = service.Name
		}

		// Set current replica count
		e.metricOptions.ServiceReplicasGauge.
			WithLabelValues(serviceName).
			Set(float64(service.Replicas))

		// 加这里，不然销毁的实例没数据
		e.metricOptions.PodCpuUsageSecondsTotal.WithLabelValues(serviceName).Set(0)
		e.metricOptions.PodMemoryWorkingSetBytes.WithLabelValues(serviceName).Set(0)
	}
	// 加这个来计算
	e.calc()

	// 添加如下
	// e.metricOptions.GatewayFunctionRequestHistogram.Collect(ch)
	e.metricOptions.GatewayFunctionRequestSummary.Collect(ch)
	e.metricOptions.PodCpuUsageSecondsTotal.Collect(ch)
	e.metricOptions.PodMemoryWorkingSetBytes.Collect(ch)

	e.metricOptions.ServiceReplicasGauge.Collect(ch)
}

// StartServiceWatcher starts a ticker and collects service replica counts to expose to prometheus
func (e *Exporter) StartServiceWatcher(endpointURL url.URL, metricsOptions MetricOptions, label string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	quit := make(chan struct{})

	go func() {
		for {
			select {
			case <-ticker.C:

				namespaces, err := e.getNamespaces(endpointURL)
				if err != nil {
					log.Println(err)
				}

				services := []types.FunctionStatus{}

				// Providers like faasd for instance have no namespaces.
				if len(namespaces) == 0 {
					services, err = e.getFunctions(endpointURL, e.FunctionNamespace)
					if err != nil {
						log.Println(err)
						continue
					}
					e.services = services
				} else {
					for _, namespace := range namespaces {
						nsServices, err := e.getFunctions(endpointURL, namespace)
						if err != nil {
							log.Println(err)
							continue
						}
						services = append(services, nsServices...)
					}
				}

				e.services = services

				break
			case <-quit:
				return
			}
		}
	}()
}

func (e *Exporter) getHTTPClient(timeout time.Duration) http.Client {

	return http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   timeout,
				KeepAlive: 0,
			}).DialContext,
			MaxIdleConns:          1,
			DisableKeepAlives:     true,
			IdleConnTimeout:       120 * time.Millisecond,
			ExpectContinueTimeout: 1500 * time.Millisecond,
		},
	}
}

func (e *Exporter) getFunctions(endpointURL url.URL, namespace string) ([]types.FunctionStatus, error) {
	timeout := 5 * time.Second
	proxyClient := e.getHTTPClient(timeout)

	endpointURL.Path = path.Join(endpointURL.Path, "/system/functions")
	if len(namespace) > 0 {
		q := endpointURL.Query()
		q.Set("namespace", namespace)
		endpointURL.RawQuery = q.Encode()
	}

	get, _ := http.NewRequest(http.MethodGet, endpointURL.String(), nil)
	if e.credentials != nil {
		get.SetBasicAuth(e.credentials.User, e.credentials.Password)
	}

	services := []types.FunctionStatus{}
	res, err := proxyClient.Do(get)
	if err != nil {
		return services, err
	}

	bytesOut, readErr := io.ReadAll(res.Body)
	if readErr != nil {
		return services, readErr
	}

	if err := json.Unmarshal(bytesOut, &services); err != nil {
		return services, fmt.Errorf("error unmarshalling response: %s, error: %s",
			string(bytesOut), err)
	}

	return services, nil
}

func (e *Exporter) getNamespaces(endpointURL url.URL) ([]string, error) {
	namespaces := []string{}
	endpointURL.Path = path.Join(endpointURL.Path, "system/namespaces")

	get, _ := http.NewRequest(http.MethodGet, endpointURL.String(), nil)
	if e.credentials != nil {
		get.SetBasicAuth(e.credentials.User, e.credentials.Password)
	}

	timeout := 5 * time.Second
	proxyClient := e.getHTTPClient(timeout)

	res, err := proxyClient.Do(get)
	if err != nil {
		return namespaces, err
	}

	if res.StatusCode == http.StatusNotFound {
		return namespaces, nil
	}

	bytesOut, readErr := io.ReadAll(res.Body)
	if readErr != nil {
		return namespaces, readErr
	}

	if err := json.Unmarshal(bytesOut, &namespaces); err != nil {
		return namespaces, fmt.Errorf("error unmarshalling response: %s, error: %s", string(bytesOut), err)
	}
	return namespaces, nil
}

// ! 这个是新加的函数，直接放最底下。即将查出来的指标转成自己定义的
func (e *Exporter) calc() {
	q1 := `sum by(container, namespace) (container_cpu_usage_seconds_total{image!="",namespace="openfaas-fn", container!="POD"})`
	q2 := `sum by(container, namespace) (container_memory_working_set_bytes{image!="",namespace="openfaas-fn", container!="POD"})`

	q1Results, err := e.prometheusQuery.Fetch(url.QueryEscape(q1))
	if err != nil {
		log.Printf("Error querying q1: %s\n", err.Error())
		return
	}

	// cpu
	for _, v := range q1Results.Data.Result {
		metricValue := v.Value[1]
		f, _ := strconv.ParseFloat(metricValue.(string), 64)
		log.Printf("calc cpu f: %f", f)
		e.metricOptions.PodCpuUsageSecondsTotal.WithLabelValues(fmt.Sprintf("%s.%s", v.Metric.Container, v.Metric.Namespace)).Set(f)
	}

	q2Results, err := e.prometheusQuery.Fetch(url.QueryEscape(q2))
	if err != nil {
		log.Printf("Error querying q2: %s\n", err.Error())
		return
	}

	// memory
	for _, v := range q2Results.Data.Result {
		metricValue := v.Value[1]
		f, _ := strconv.ParseFloat(metricValue.(string), 64)
		log.Printf("calc memory f: %f", f)
		e.metricOptions.PodMemoryWorkingSetBytes.WithLabelValues(fmt.Sprintf("%s.%s", v.Metric.Container, v.Metric.Namespace)).Set(f)
	}
}
