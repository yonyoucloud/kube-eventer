// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kubernetes

import (
	"fmt"
	"net/url"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AliyunContainerService/kube-eventer/common/kubernetes"
	"github.com/AliyunContainerService/kube-eventer/core"
	kubeapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubewatch "k8s.io/apimachinery/pkg/watch"
	kubernetesCore "k8s.io/client-go/kubernetes"
	kubev1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog"
)

const (
	// Number of object pointers. Big enough so it won't be hit anytime soon with reasonable GetNewEvents frequency.
	LocalEventsBufferSize = 100000

	// TerminationReasonOOMKilled is the reason of a ContainerStateTerminated that reflects an OOM kill
	TerminationReasonOOMKilled = "OOMKilled"
)

var (
	// Last time of event since unix epoch in seconds
	lastEventTimestamp = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "kube_eventer",
			Subsystem: "scraper",
			Name:      "last_time_seconds",
			Help:      "Last time of event since unix epoch in seconds.",
		})
	totalEventsNum = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: "kube_eventer",
			Subsystem: "scraper",
			Name:      "events_total_number",
			Help:      "The total number of events.",
		})
	scrapEventsDuration = prometheus.NewSummary(
		prometheus.SummaryOpts{
			Namespace: "kube_eventer",
			Subsystem: "scraper",
			Name:      "duration_milliseconds",
			Help:      "Time spent scraping events in milliseconds.",
		})

	kubernetesEvent = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "kube_eventer",
			Subsystem: "scraper",
			Name:      "kubernetes_event",
			Help:      "The event of the kubernetes.",
		}, []string{"count", "kind", "namespace", "name", "uid", "resource_version", "component", "host", "first_occurrence_timestamp", "last_occurrence_timestamp", "reason", "message", "type"})
)

func init() {
	prometheus.MustRegister(lastEventTimestamp)
	prometheus.MustRegister(totalEventsNum)
	prometheus.MustRegister(scrapEventsDuration)
	prometheus.MustRegister(kubernetesEvent)
}

// Implements core.EventSource interface.
type KubernetesEventSource struct {
	// Large local buffer, periodically read.
	localEventsBuffer chan *kubeapi.Event

	stopChannel chan struct{}

	eventClient kubev1core.EventInterface

	kubeClient kubernetesCore.Interface

	startTime time.Time
}

func (this *KubernetesEventSource) GetNewEvents() *core.EventBatch {
	startTime := time.Now()
	defer func() {
		lastEventTimestamp.Set(float64(time.Now().Unix()))
		scrapEventsDuration.Observe(float64(time.Since(startTime)) / float64(time.Millisecond))
	}()
	result := core.EventBatch{
		Timestamp: time.Now(),
		Events:    []*kubeapi.Event{},
	}
	// Get all data from the buffer.
event_loop:
	for {
		select {
		case event := <-this.localEventsBuffer:
			// Prometheus中暂时只记录非Normal类型的事件
			if event.Type != "Normal" {
				// 记录Kubernetes事件到Prometheus
				kubernetesEvent.WithLabelValues(
					fmt.Sprintf("%d", event.Count),
					event.InvolvedObject.Kind,
					event.InvolvedObject.Namespace,
					event.InvolvedObject.Name,
					string(event.UID),
					event.ResourceVersion,
					event.Source.Component,
					event.Source.Host,
					event.FirstTimestamp.Format("2006-01-02T15:04:05Z"),
					event.LastTimestamp.Format("2006-01-02T15:04:05Z"),
					event.Reason,
					event.Message,
					event.Type,
				).Set(1)
			}
			result.Events = append(result.Events, event)
		default:
			break event_loop
		}
	}

	totalEventsNum.Add(float64(len(result.Events)))

	return &result
}

func (this *KubernetesEventSource) watch() {
	// Outer loop, for reconnections.
	for {
		events, err := this.eventClient.List(metav1.ListOptions{Limit: 1})
		if err != nil {
			klog.Errorf("Failed to load events: %v", err)
			time.Sleep(time.Second)
			continue
		}
		// Do not write old events.
		klog.V(9).Infof("kubernetes source watch event. list event first. raw events: %v", events)

		resourceVersion := events.ResourceVersion

		watcher, err := this.eventClient.Watch(
			metav1.ListOptions{
				Watch:           true,
				ResourceVersion: resourceVersion})
		if err != nil {
			klog.Errorf("Failed to start watch for new events: %v", err)
			time.Sleep(time.Second)
			continue
		}

		watchChannel := watcher.ResultChan()
		// Inner loop, for update processing.
	inner_loop:
		for {
			select {
			case watchUpdate, ok := <-watchChannel:

				klog.V(10).Infof("kubernetes source watch channel update. watch channel update. watchChanObject: %v", watchUpdate)

				if !ok {
					klog.Errorf("Event watch channel closed")
					break inner_loop
				}

				if watchUpdate.Type == kubewatch.Error {
					if status, ok := watchUpdate.Object.(*metav1.Status); ok {
						klog.Errorf("Error during watch: %#v", status)
						break inner_loop
					}
					klog.Errorf("Received unexpected error: %#v", watchUpdate.Object)
					break inner_loop
				}

				if event, ok := watchUpdate.Object.(*kubeapi.Event); ok {

					klog.V(9).Infof("kubernetes source watch event. watch channel update. event: %v", event)

					switch watchUpdate.Type {
					case kubewatch.Added, kubewatch.Modified:
						select {
						case this.localEventsBuffer <- event:
							// 评估事件，将类似OOMKilled事件发送出来
							this.evaluateEvent(event)
							// Ok, buffer not full.
						default:
							// Buffer full, need to drop the event.
							klog.Errorf("Event buffer full, dropping event")
						}
					case kubewatch.Deleted:
						// Deleted events are silently ignored.
					default:
						klog.Warningf("Unknown watchUpdate.Type: %#v", watchUpdate.Type)
					}
				} else {
					klog.Errorf("Wrong object received: %v", watchUpdate)
				}

			case <-this.stopChannel:
				watcher.Stop()
				klog.Infof("Event watching stopped")
				return
			}
		}
	}
}

const startedEvent = "Started"
const podKind = "Pod"

// 判断是否是容器启动事件
func isContainerStartedEvent(event *kubeapi.Event) bool {
	return (event.Reason == startedEvent &&
		event.InvolvedObject.Kind == podKind)
}

// 评估事件
func (this *KubernetesEventSource) evaluateEvent(event *kubeapi.Event) {
	klog.V(2).Infof("Got event %s/%s (count: %d), reason: %s, involved object: %s", event.ObjectMeta.Namespace, event.ObjectMeta.Name, event.Count, event.Reason, event.InvolvedObject.Kind)

	// 排除容器启动事件
	if !isContainerStartedEvent(event) {
		// 任何事件都应该触发一次“评估检测”
		// return
	}

	// 排查非Pod类型的事件
	if event.InvolvedObject.Kind != podKind {
		return
	}

	// 获取发生事件的Pod信息
	pod, err := this.kubeClient.CoreV1().Pods(event.InvolvedObject.Namespace).Get(event.InvolvedObject.Name, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed to retrieve pod %s/%s, due to: %v", event.InvolvedObject.Namespace, event.InvolvedObject.Name, err)
		return
	}

	// 评估Pod状态
	this.evaluatePodStatus(pod)
}

// 评估Pod状态
func (this *KubernetesEventSource) evaluatePodStatus(pod *kubeapi.Pod) {
	// Look for OOMKilled containers
	for i, s := range pod.Status.ContainerStatuses {
		// 排除正常状态和非OOMKilled状态的情况，注意: 后续可以针对不在原生事件中的其他重要状态
		if s.LastTerminationState.Terminated == nil || s.LastTerminationState.Terminated.Reason != TerminationReasonOOMKilled {
			ProcessedContainerUpdates.WithLabelValues("not_oomkilled").Inc()
			continue
		}

		// 排除发生在kube-eventer启动之前的情况
		if s.LastTerminationState.Terminated.FinishedAt.Time.Before(this.startTime) {
			klog.V(1).Infof("The container '%s' in '%s/%s' was terminated before this controller started", s.Name, pod.Namespace, pod.Name)
			ProcessedContainerUpdates.WithLabelValues("oomkilled_termination_too_old").Inc()
			continue
		}

		// 设置OOMKilled事件信息
		eventType := kubeapi.EventTypeWarning
		reason := "PreviousContainerWasOOMKilled"
		message := fmt.Sprintf("The previous instance of the container '%s' (%s) was OOMKilled, LimitMemory: %.1fMB", s.Name, s.ContainerID, float64(pod.Spec.Containers[i].Resources.Limits.Memory().Value()/1024/1024))

		// 构造一个事件
		event := &kubeapi.Event{
			ObjectMeta: metav1.ObjectMeta{
				Name:        pod.Name,
				Namespace:   pod.Namespace,
				Annotations: pod.Annotations,
			},
			InvolvedObject: kubeapi.ObjectReference{
				APIVersion: pod.APIVersion,
				Kind:       podKind,
				// FieldPath: ,
				Name:            pod.Name,
				Namespace:       pod.Namespace,
				ResourceVersion: pod.ResourceVersion,
				UID:             pod.UID,
			},
			Reason:         reason,
			Message:        message,
			FirstTimestamp: s.LastTerminationState.Terminated.StartedAt,
			LastTimestamp:  s.LastTerminationState.Terminated.FinishedAt,
			Count:          1,
			Type:           eventType,
			Source: kubeapi.EventSource{
				Component: "enhancement",
				Host:      pod.Status.HostIP,
			},
		}
		// 将事件发送到本地事件缓冲区
		this.localEventsBuffer <- event

		ProcessedContainerUpdates.WithLabelValues("oomkilled_event_sent").Inc()
	}
}

func NewKubernetesSource(uri *url.URL) (*KubernetesEventSource, error) {
	kubeClient, err := kubernetes.GetKubernetesClient(uri)
	if err != nil {
		klog.Errorf("Failed to create kubernetes client,because of %v", err)
		return nil, err
	}
	eventClient := kubeClient.CoreV1().Events(kubeapi.NamespaceAll)
	result := KubernetesEventSource{
		localEventsBuffer: make(chan *kubeapi.Event, LocalEventsBufferSize),
		stopChannel:       make(chan struct{}),
		eventClient:       eventClient,
		kubeClient:        kubeClient,
		startTime:         time.Now(),
	}
	go result.watch()
	return &result, nil
}
