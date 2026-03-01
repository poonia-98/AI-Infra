package internal

import (
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

type ClientConfig struct {
	KubeconfigPath string
}

func NewKubeClient(cfg ClientConfig) (*kubernetes.Clientset, error) {
	restConfig, err := buildConfig(cfg.KubeconfigPath)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(restConfig)
}

func buildConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}
	if inCluster, err := rest.InClusterConfig(); err == nil {
		return inCluster, nil
	}
	if home := homedir.HomeDir(); home != "" {
		candidate := filepath.Join(home, ".kube", "config")
		if _, err := os.Stat(candidate); err == nil {
			return clientcmd.BuildConfigFromFlags("", candidate)
		}
	}
	return rest.InClusterConfig()
}
