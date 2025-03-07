package schedwatch

import (
	"context"
	"time"

	"github.com/tychoish/fun/pubsub"
	"go.uber.org/zap"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/watch"
)

func isActivePod(pod *corev1.Pod) bool {
	return pod.Status.PodIP != "" && util.PodReady(pod)
}

func StartSchedulerWatcher(
	ctx context.Context,
	parentLogger *zap.Logger,
	kubeClient *kubernetes.Clientset,
	metrics watch.Metrics,
	eventBroker *pubsub.Broker[WatchEvent],
	schedulerName string,
) (*watch.Store[corev1.Pod], error) {
	logger := parentLogger.Named("watch-schedulers")

	return watch.Watch(
		ctx,
		logger.Named("watch"),
		kubeClient.CoreV1().Pods(schedulerNamespace),
		watch.Config{
			ObjectNameLogField: "pod",
			Metrics: watch.MetricsConfig{
				Metrics:  metrics,
				Instance: "Scheduler Pod",
			},
			// We don't need to be super responsive to scheduler changes.
			//
			// FIXME: make these configurable.
			RetryRelistAfter: util.NewTimeRange(time.Second, 4, 5),
			RetryWatchAfter:  util.NewTimeRange(time.Second, 4, 5),
		},
		watch.Accessors[*corev1.PodList, corev1.Pod]{
			Items: func(list *corev1.PodList) []corev1.Pod { return list.Items },
		},
		watch.InitModeSync,
		metav1.ListOptions{LabelSelector: schedulerLabelSelector(schedulerName)},
		watch.HandlerFuncs[*corev1.Pod]{
			AddFunc: func(pod *corev1.Pod, preexisting bool) {
				if isActivePod(pod) {
					event := WatchEvent{kind: eventKindReady, info: newSchedulerInfo(pod)}
					logger.Info("new scheduler, already ready", zap.Object("event", event))
					eventBroker.Publish(ctx, event)
				}
			},
			UpdateFunc: func(oldPod, newPod *corev1.Pod) {
				oldReady := isActivePod(oldPod)
				newReady := isActivePod(newPod)

				if !oldReady && newReady {
					event := WatchEvent{kind: eventKindReady, info: newSchedulerInfo(newPod)}
					logger.Info("existing scheduler became ready", zap.Object("event", event))
					eventBroker.Publish(ctx, event)
				} else if oldReady && !newReady {
					event := WatchEvent{kind: eventKindDeleted, info: newSchedulerInfo(oldPod)}
					logger.Info("existing scheduler no longer ready", zap.Object("event", event))
					eventBroker.Publish(ctx, event)
				}
			},
			DeleteFunc: func(pod *corev1.Pod, mayBeStale bool) {
				wasReady := isActivePod(pod)
				if wasReady {
					event := WatchEvent{kind: eventKindDeleted, info: newSchedulerInfo(pod)}
					logger.Info("Previously-ready scheduler deleted", zap.Object("event", event))
					eventBroker.Publish(ctx, event)
				}
			},
		},
	)

}
