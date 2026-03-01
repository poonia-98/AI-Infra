package internal

import (
	"context"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

type PodWatcher struct {
	manager *PodManager
}

type WatchEvent struct {
	PodName    string
	Namespace  string
	Phase      corev1.PodPhase
	Reason     string
	Restarted  bool
	OccurredAt time.Time
}

type WatchHandlers struct {
	OnRunning   func(WatchEvent)
	OnSucceeded func(WatchEvent)
	OnFailed    func(WatchEvent)
}

func NewPodWatcher(manager *PodManager) *PodWatcher {
	return &PodWatcher{manager: manager}
}

func (w *PodWatcher) WatchPod(ctx context.Context, namespace, podName string, maxRestarts int, restartDelay time.Duration, handlers WatchHandlers) error {
	if w.manager == nil {
		return errors.New("pod manager is required")
	}
	if namespace == "" || podName == "" {
		return errors.New("namespace and pod name are required")
	}
	restarts := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		watcher, err := w.manager.client.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
			FieldSelector: "metadata.name=" + podName,
		})
		if err != nil {
			return err
		}
		err = w.consumeWatch(ctx, watcher, namespace, podName, maxRestarts, restartDelay, &restarts, handlers)
		watcher.Stop()
		if err == nil {
			return nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (w *PodWatcher) consumeWatch(ctx context.Context, watcher watch.Interface, namespace, podName string, maxRestarts int, restartDelay time.Duration, restarts *int, handlers WatchHandlers) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return errors.New("watch channel closed")
			}
			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			switch event.Type {
			case watch.Added, watch.Modified:
				switch pod.Status.Phase {
				case corev1.PodRunning:
					if handlers.OnRunning != nil {
						handlers.OnRunning(WatchEvent{
							PodName:    podName,
							Namespace:  namespace,
							Phase:      corev1.PodRunning,
							OccurredAt: time.Now().UTC(),
						})
					}
				case corev1.PodSucceeded:
					if handlers.OnSucceeded != nil {
						handlers.OnSucceeded(WatchEvent{
							PodName:    podName,
							Namespace:  namespace,
							Phase:      corev1.PodSucceeded,
							OccurredAt: time.Now().UTC(),
						})
					}
					return nil
				case corev1.PodFailed:
					reason := PodFailureReason(pod)
					if handlers.OnFailed != nil {
						handlers.OnFailed(WatchEvent{
							PodName:    podName,
							Namespace:  namespace,
							Phase:      corev1.PodFailed,
							Reason:     reason,
							OccurredAt: time.Now().UTC(),
						})
					}
					if *restarts >= maxRestarts {
						return errors.New("max restarts reached")
					}
					*restarts++
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(restartDelay):
					}
					if err := w.manager.RestartPod(ctx, namespace, podName, 10); err != nil {
						return err
					}
					if handlers.OnRunning != nil {
						handlers.OnRunning(WatchEvent{
							PodName:    podName,
							Namespace:  namespace,
							Phase:      corev1.PodPending,
							Restarted:  true,
							OccurredAt: time.Now().UTC(),
						})
					}
				}
			case watch.Deleted:
				return nil
			case watch.Error:
				return errors.New("pod watch error")
			}
		}
	}
}

