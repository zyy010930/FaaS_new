package metrics

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"

	types "github.com/openfaas/faas-provider/types"
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

		var functions []types.FunctionStatus

		err := json.Unmarshal(upstreamBody, &functions)
		if err != nil {
			log.Printf("Metrics upstream error: %s, value: %s", err, string(upstreamBody))

			http.Error(w, "Unable to parse list of functions from provider", http.StatusInternalServerError)
			return
		}

		// Ensure values are empty first.
		for i := range functions {
			functions[i].InvocationCount = 0
			var usage = types.FunctionUsage{CPU: 0, TotalMemoryBytes: 0}
			functions[i].Usage = &usage
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
			ns1 := functions[0].Namespace
			q1 := fmt.Sprintf(`sum(pod_cpu_usage_seconds_total{function_name=~".*.%s"}) by (function_name)`, ns1)
			results1, err1 := prometheusQuery.Fetch(url.QueryEscape(q1))
			if err1 != nil {
				// log the error but continue, the mixIn will correctly handle the empty results.
				log.Printf("Error querying Prometheus: %s\n", err.Error())
			}
			mixCPU(&functions, results1)

			ns2 := functions[0].Namespace
			q2 := fmt.Sprintf(`sum(pod_memory_working_set_bytes{function_name=~".*.%s"}) by (function_name)`, ns2)
			results2, err2 := prometheusQuery.Fetch(url.QueryEscape(q2))
			if err2 != nil {
				// log the error but continue, the mixIn will correctly handle the empty results.
				log.Printf("Error querying Prometheus: %s\n", err.Error())
			}
			mixMemory(&functions, results2)
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

func mixIn(functions *[]types.FunctionStatus, metrics *VectorQueryResponse) {

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

func mixCPU(functions *[]types.FunctionStatus, metrics *VectorQueryResponse) {

	if functions == nil {
		return
	}

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
					(*functions)[i].Usage.CPU += f
				}
			}
		}
	}
}

func mixMemory(functions *[]types.FunctionStatus, metrics *VectorQueryResponse) {

	if functions == nil {
		return
	}

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
					(*functions)[i].Usage.TotalMemoryBytes += f
				}
			}
		}
	}
}
