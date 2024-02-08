package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
)

func main() {

	jobMode := os.Getenv("JOB_MODE")

	// In task mode, collect the resource metrics once
	if jobMode == "task" {
		collectInstanceMetrics()
		return
	}

	// If the 'INTERVAL' env var is set then sleep for that many seconds
	sleepDuration := 10
	if t := os.Getenv("INTERVAL"); t != "" {
		sleepDuration, _ = strconv.Atoi(t)
	}

	// In daemon mode, collect resource metrics in an endless loop
	for {
		collectInstanceMetrics()
		time.Sleep(time.Duration(sleepDuration) * time.Second)
	}
}

type ResourceStats struct {
	Current    int64 `json:"current"`
	Configured int64 `json:"configured"`
	Usage      int64 `json:"usage"`
}

type InstanceResourceStats struct {
	Metric        string        `json:"metric"`
	Name          string        `json:"name"`
	Parent        string        `json:"parent"`
	ComponentType string        `json:"component_type"`
	ComponentName string        `json:"component_name"`
	Cpu           ResourceStats `json:"cpu"`
	Memory        ResourceStats `json:"memory"`
	Message       string        `json:"message"`
}

// helper function that retrieves all pods and all pod metrics
// this function creates a structured log line for each pod for which the kube metrics api provides a metric
func collectInstanceMetrics() {

	startTime := time.Now()
	fmt.Println("Start to capture pod metrics ...")

	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}

	// obtain the kube namespace related to this Code Engine project
	nsBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		panic(err.Error())
	}
	namespace := string(nsBytes)

	// fetches all pods
	pods := getAllPods(namespace, config)

	// fetch all pod metrics
	podMetrics := getAllPodMetrics(namespace, config)

	for _, podMetric := range podMetrics {

		componentType := "unknown"
		if _, ok := podMetric.ObjectMeta.Labels["buildrun.shipwright.io/name"]; ok {
			componentType = "build"
		}
		if _, ok := podMetric.ObjectMeta.Labels["serving.knative.dev/service"]; ok {
			componentType = "app"
		}
		if _, ok := podMetric.ObjectMeta.Labels["codeengine.cloud.ibm.com/job-run"]; ok {
			componentType = "job"
		}

		var componentName string
		var parent string
		switch componentType {
		case "job":
			if val, ok := podMetric.ObjectMeta.Labels["codeengine.cloud.ibm.com/job-definition-name"]; ok {
				componentName = val
			} else {
				componentName = "standalone"
			}
			parent = podMetric.ObjectMeta.Labels["codeengine.cloud.ibm.com/job-run"]
		case "app":
			componentName = podMetric.ObjectMeta.Labels["serving.knative.dev/service"]
			parent = podMetric.ObjectMeta.Labels["serving.knative.dev/revision"]
		case "build":
			if val, ok := podMetric.ObjectMeta.Labels["build.shipwright.io/name"]; ok {
				componentName = val
			} else {
				componentName = "standalone"
			}

			parent = podMetric.ObjectMeta.Labels["buildrun.shipwright.io/name"]
		default:
			componentName = "unknown"
		}
		cpuCurrent := podMetric.Containers[0].Usage.Cpu().ToDec().AsApproximateFloat64() * 1000
		memoryCurrent := podMetric.Containers[0].Usage.Memory().ToDec().AsApproximateFloat64() / 1000 / 1000

		stats := InstanceResourceStats{
			Metric:        "instance-resources",
			Name:          podMetric.Name,
			Parent:        parent,
			ComponentType: componentType,
			ComponentName: componentName,
			Cpu: ResourceStats{
				Current: int64(cpuCurrent),
			},
			Memory: ResourceStats{
				Current: int64(memoryCurrent),
			},
		}

		pod := getPod(podMetric.Name, pods)
		if pod != nil {
			// extract memory and cpu limit
			cpu, memory := getCpuAndMemoryLimit(componentType, *pod)

			cpuLimit := cpu.ToDec().AsApproximateFloat64() * 1000
			stats.Cpu.Configured = int64(cpuLimit)
			stats.Cpu.Usage = int64((cpuCurrent / cpuLimit) * 100)

			memoryLimit := memory.ToDec().AsApproximateFloat64() / 1000 / 1000
			stats.Memory.Configured = int64(memoryLimit)
			stats.Memory.Usage = int64(memoryCurrent / memoryLimit * 100)
		}

		stats.Message = "Captured metrics of " + stats.ComponentType + " instance '" + stats.Name + "': " + fmt.Sprintf("%d", stats.Cpu.Current) + "m vCPU, " + fmt.Sprintf("%d", stats.Memory.Current) + " MB memory"

		fmt.Println(ToJSONString(stats))
	}

	fmt.Println("Captured pod metrics in " + strconv.FormatInt(time.Since(startTime).Milliseconds(), 10) + "ms")
}

func getPod(name string, pods []v1.Pod) *v1.Pod {
	for _, pod := range pods {
		if pod.Name == name {
			return &pod
		}
	}
	return nil
}

// helper function to retrieve all pods from the Kube API
func getAllPods(namespace string, config *rest.Config) []v1.Pod {

	// obtain the core clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	// fetches all pods
	pods := []v1.Pod{}
	var podsContinueToken string
	podsPagelimit := int64(100)
	for {
		podList, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{Limit: podsPagelimit, Continue: podsContinueToken})
		if err != nil {
			fmt.Println("Failed to list pods" + err.Error())
			break
		}

		pods = append(pods, podList.Items...)

		podsContinueToken = podList.Continue
		if len(podsContinueToken) == 0 {
			break
		}
	}

	return pods
}

// helper function to retrieve all pod metrics from the Kube API
func getAllPodMetrics(namespace string, config *rest.Config) []v1beta1.PodMetrics {
	// obtain the metrics clientset
	metricsclientset, err := metricsv.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	// fetch all pod metrics
	podMetrics := []v1beta1.PodMetrics{}
	var metricsContinueToken string
	metricsPageLimit := int64(100)
	for {
		// fetch all pod metrics
		podMetricsList, err := metricsclientset.MetricsV1beta1().PodMetricses(namespace).List(context.TODO(), metav1.ListOptions{Limit: metricsPageLimit, Continue: metricsContinueToken})
		if err != nil {
			fmt.Println("Failed to list pod metrics" + err.Error())
			break
		}
		podMetrics = append(podMetrics, podMetricsList.Items...)

		metricsContinueToken = podMetricsList.Continue
		if len(metricsContinueToken) == 0 {
			break
		}
	}

	return podMetrics
}

// Helper function to extract CPU and Memory limits from the pod spec
func getCpuAndMemoryLimit(componentType string, pod v1.Pod) (*resource.Quantity, *resource.Quantity) {
	if componentType == "job" {
		if len(pod.Spec.Containers) > 0 {
			cpuLimit := pod.Spec.Containers[0].Resources.Limits.Cpu()
			memoryLimit := pod.Spec.Containers[0].Resources.Limits.Memory()
			return cpuLimit, memoryLimit
		}
	} else if componentType == "app" {
		if len(pod.Spec.Containers) > 0 {
			for _, container := range pod.Spec.Containers {
				if container.Name == "user-container" {
					cpuLimit := container.Resources.Limits.Cpu()
					memoryLimit := container.Resources.Limits.Memory()
					return cpuLimit, memoryLimit
				}
			}
		}
	} else if componentType == "build" {
		if len(pod.Spec.Containers) > 0 {
			cpuLimit := pod.Spec.Containers[0].Resources.Limits.Cpu()
			memoryLimit := pod.Spec.Containers[0].Resources.Limits.Memory()
			return cpuLimit, memoryLimit
		}
	}

	return nil, nil
}

// Helper function that converts any object into a JSON string representation
func ToJSONString(obj interface{}) string {
	if obj == nil {
		return ""
	}

	bytes, err := json.Marshal(&obj)
	if err != nil {
		return "marshal error"
	}

	return string(bytes)
}
