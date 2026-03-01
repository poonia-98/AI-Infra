package internal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type PodManager struct {
	client kubernetes.Interface
}

func NewPodManager(client kubernetes.Interface) *PodManager {
	return &PodManager{client: client}
}

func (p *PodManager) EnsureNamespace(ctx context.Context, namespace string) error {
	if namespace == "" {
		return errors.New("namespace is required")
	}
	_, err := p.client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	_, err = p.client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	}, metav1.CreateOptions{})
	return err
}

func (p *PodManager) CreatePod(ctx context.Context, namespace string, pod *corev1.Pod) (*corev1.Pod, error) {
	if pod == nil {
		return nil, errors.New("pod spec is required")
	}
	if err := p.EnsureNamespace(ctx, namespace); err != nil {
		return nil, err
	}
	return p.client.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
}

func (p *PodManager) CreateDeployment(ctx context.Context, namespace string, deployment *appsv1.Deployment) (*appsv1.Deployment, error) {
	if deployment == nil {
		return nil, errors.New("deployment spec is required")
	}
	if err := p.EnsureNamespace(ctx, namespace); err != nil {
		return nil, err
	}
	return p.client.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
}

func (p *PodManager) CreateJob(ctx context.Context, namespace string, job *batchv1.Job) (*batchv1.Job, error) {
	if job == nil {
		return nil, errors.New("job spec is required")
	}
	if err := p.EnsureNamespace(ctx, namespace); err != nil {
		return nil, err
	}
	return p.client.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
}

func (p *PodManager) StreamPodLogs(ctx context.Context, namespace, podName, container string, follow bool) (io.ReadCloser, error) {
	if namespace == "" || podName == "" {
		return nil, errors.New("namespace and pod name are required")
	}
	req := p.client.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container:  container,
		Follow:     follow,
		Timestamps: true,
	})
	return req.Stream(ctx)
}

func (p *PodManager) GetPod(ctx context.Context, namespace, podName string) (*corev1.Pod, error) {
	return p.client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
}

func (p *PodManager) RestartPod(ctx context.Context, namespace, podName string, graceSeconds int64) error {
	if namespace == "" || podName == "" {
		return errors.New("namespace and pod name are required")
	}
	err := p.client.CoreV1().Pods(namespace).Delete(ctx, podName, metav1.DeleteOptions{
		GracePeriodSeconds: &graceSeconds,
	})
	if err != nil {
		return err
	}
	timeout := time.After(30 * time.Second)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timeout waiting for pod deletion: %s/%s", namespace, podName)
		case <-ticker.C:
			if _, err := p.GetPod(ctx, namespace, podName); err != nil {
				return nil
			}
		}
	}
}

func PodFailureReason(pod *corev1.Pod) string {
	if pod == nil {
		return "pod is nil"
	}
	for _, st := range pod.Status.ContainerStatuses {
		if st.State.Waiting != nil {
			return st.State.Waiting.Reason
		}
		if st.State.Terminated != nil {
			if st.State.Terminated.Message != "" {
				return st.State.Terminated.Message
			}
			return st.State.Terminated.Reason
		}
	}
	if pod.Status.Reason != "" {
		return pod.Status.Reason
	}
	if pod.Status.Message != "" {
		return pod.Status.Message
	}
	return "unknown"
}

