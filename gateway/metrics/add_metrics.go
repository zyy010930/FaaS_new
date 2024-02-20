package metrics

import (
	"encoding/json"
	"fmt"
	"github.com/openfaas/faas-provider/types"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
)

// AddMetricsHandler wraps a http.HandlerFunc with Prometheus metrics
func AddMetricsHandler(handler http.HandlerFunc, prometheusQuery PrometheusQueryFetcher) http.HandlerFunc {

	return func(w http.ResponseWriter, r *http.Request) {

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		upstreamCall := recorder.Result()

		if upstreamCall.Body == nil {
			log.Println("Upstream call had empty body.")
			return
		}

		defer upstreamCall.Body.Close()
		upstreamBody, _ := io.ReadAll(upstreamCall.Body)

		if recorder.Code != http.StatusOK {
			log.Printf("List functions responded with code %d, body: %s",
				recorder.Code,
				string(upstreamBody))
			http.Error(w, string(upstreamBody), recorder.Code)
			return
		}

		var function []types.FunctionStatus

		err := json.Unmarshal(upstreamBody, &function)
		if err != nil {
			log.Printf("Metrics upstream error: %s, value: %s", err, string(upstreamBody))

			http.Error(w, "Unable to parse list of functions from provider", http.StatusInternalServerError)
			return
		}

		var functions []FunctionStatus

		// Ensure values are empty first.
		for i := range function {
			var fun FunctionStatus
			fun.InvocationCount = 0
			fun.InvocationAvgTime = 0
			var usage = FunctionUsage{CPU: 0, TotalMemoryBytes: 0}
			fun.Usage = &usage
			fun.Name = function[i].Name
			fun.Namespace = function[i].Namespace
			fun.Image = function[i].Image
			var limit = FunctionResources{CPU: "", Memory: ""}
			fun.Limits = &limit
			if function[i].Limits != nil {
				fun.Limits.CPU = function[i].Limits.CPU
				fun.Limits.Memory = function[i].Limits.Memory
			}
			fun.EnvProcess = function[i].EnvProcess
			fun.EnvVars = function[i].EnvVars
			fun.AvailableReplicas = function[i].AvailableReplicas
			fun.Replicas = function[i].Replicas
			var request = FunctionResources{CPU: "", Memory: ""}
			fun.Requests = &request
			if function[i].Requests != nil {
				fun.Requests.CPU = function[i].Requests.CPU
				fun.Requests.Memory = function[i].Requests.Memory
			}
			fun.Secrets = function[i].Secrets
			if function[i].Labels != nil {
				fun.Labels = function[i].Labels
			}
			fun.Annotations = function[i].Annotations
			fun.Constraints = function[i].Constraints
			fun.ReadOnlyRootFilesystem = function[i].ReadOnlyRootFilesystem
			fun.CreatedAt = function[i].CreatedAt
			functions = append(functions, fun)
		}

		if len(functions) > 0 {

			ns := functions[0].Namespace
			q := fmt.Sprintf(`sum(gateway_function_invocation_total{function_name=~".*.%s"}) by (function_name)`, ns)
			// Restrict query results to only function names matching namespace suffix.

			results, err := prometheusQuery.Fetch(url.QueryEscape(q))
			if err != nil {
				// log the error but continue, the mixIn will correctly handle the empty results.
				log.Printf("Error querying Prometheus: %s\n", err.Error())
			}
			mixIn(&functions, results)

			//CPU和memory
			//ns1 := functions[0].Namespace
			//q1 := fmt.Sprintf(`sum(pod_cpu_usage_seconds_total{function_name=~".*.%s"}) by (function_name)`, ns1)
			q1 := fmt.Sprintf(`sum by(container, namespace) (irate(container_cpu_usage_seconds_total{image!="",namespace="openfaas-fn", container!="POD"}[5m])*100)`)
			results1, err1 := prometheusQuery.Fetch(url.QueryEscape(q1))
			if err1 != nil {
				// log the error but continue, the mixIn will correctly handle the empty results.
				log.Printf("Error querying Prometheus: %s\n", err.Error())
			}
			mixCPU(&functions, results1)

			//ns2 := functions[0].Namespace
			//q2 := fmt.Sprintf(`sum(pod_memory_working_set_bytes{function_name=~".*.%s"}) by (function_name)`, ns2)
			q2 := fmt.Sprintf(`sum by(container, namespace) (container_memory_working_set_bytes{image!="",namespace="openfaas-fn", container!="POD"})`)
			results2, err2 := prometheusQuery.Fetch(url.QueryEscape(q2))
			if err2 != nil {
				// log the error but continue, the mixIn will correctly handle the empty results.
				log.Printf("Error querying Prometheus: %s\n", err.Error())
			}
			mixMemory(&functions, results2)

			//sum by (function_name) (gateway_function_cold_start_seconds_sum / gateway_function_cold_start_seconds_count)
			q3 := fmt.Sprintf(`sum by (function_name) (gateway_function_request_seconds_sum / gateway_function_request_seconds_count)`)
			results3, err3 := prometheusQuery.Fetch(url.QueryEscape(q3))
			if err3 != nil {
				// log the error but continue, the mixIn will correctly handle the empty results.
				log.Printf("Error querying Prometheus: %s\n", err.Error())
			}
			mixTime(&functions, results3)
		}

		bytesOut, err := json.Marshal(functions)
		if err != nil {
			log.Printf("Error serializing functions: %s", err)
			http.Error(w, "Error writing response after adding metrics", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(bytesOut)
	}
}

func mixIn(functions *[]FunctionStatus, metrics *VectorQueryResponse) {

	if functions == nil {
		return
	}

	for i, function := range *functions {
		for _, v := range metrics.Data.Result {

			if v.Metric.FunctionName == fmt.Sprintf("%s.%s", function.Name, function.Namespace) {
				metricValue := v.Value[1]
				switch value := metricValue.(type) {
				case string:
					f, err := strconv.ParseFloat(value, 64)
					if err != nil {
						log.Printf("add_metrics: unable to convert value %q for metric: %s", value, err)
						continue
					}
					(*functions)[i].InvocationCount += f
				}
			}
		}
	}
}

func mixCPU(functions *[]FunctionStatus, metrics *VectorQueryResponse) {

	if functions == nil {
		return
	}
	log.Printf("metrices len: %d", len(metrics.Data.Result))
	for i, function := range *functions {
		for _, v := range metrics.Data.Result {
			if v.Metric.Container == fmt.Sprintf("%s", function.Name) && v.Metric.Namespace == fmt.Sprintf("%s", function.Namespace) {
				metricValue := v.Value[1]
				switch value := metricValue.(type) {
				case string:
					f, err := strconv.ParseFloat(value, 64)
					if err != nil {
						log.Printf("add_metrics: unable to convert value %q for metric: %s", value, err)
						continue
					}
					log.Printf("add_metrics: CPU %f", f)
					(*((*functions)[i].Usage)).CPU += f
				}
			}
		}
	}
}

func mixMemory(functions *[]FunctionStatus, metrics *VectorQueryResponse) {

	if functions == nil {
		return
	}

	log.Printf("metrices len: %d", len(metrics.Data.Result))
	for i, function := range *functions {
		for _, v := range metrics.Data.Result {
			if v.Metric.Container == fmt.Sprintf("%s", function.Name) && v.Metric.Namespace == fmt.Sprintf("%s", function.Namespace) {
				metricValue := v.Value[1]
				switch value := metricValue.(type) {
				case string:
					f, err := strconv.ParseFloat(value, 64)
					if err != nil {
						log.Printf("add_metrics: unable to convert value %q for metric: %s", value, err)
						continue
					}
					log.Printf("add_metrics: Memory %f", f)
					(*((*functions)[i].Usage)).TotalMemoryBytes += f
				}
			}
		}
	}
}

func mixTime(functions *[]FunctionStatus, metrics *VectorQueryResponse) {

	if functions == nil {
		return
	}

	log.Printf("metrices len: %d", len(metrics.Data.Result))
	for i, function := range *functions {
		num := 0.0
		for _, v := range metrics.Data.Result {
			if v.Metric.FunctionName == fmt.Sprintf("%s.%s", function.Name, function.Namespace) {
				metricValue := v.Value[1]
				switch value := metricValue.(type) {
				case string:
					f, err := strconv.ParseFloat(value, 64)
					if err != nil {
						log.Printf("add_metrics: unable to convert value %q for metric: %s", value, err)
						continue
					}
					log.Printf("add_metrics: avgTime %f", f)
					(*functions)[i].InvocationAvgTime += f
					num += 1.0
				}
			}
		}
		if num != 0 {
			(*functions)[i].InvocationAvgTime /= num
		}
	}
}
