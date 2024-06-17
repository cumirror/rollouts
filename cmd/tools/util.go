package tools

import (
	"fmt"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"k8s.io/client-go/kubernetes"
	"k8s.io/controller-manager/pkg/clientbuilder"
)

func getK8sClientFromRest(config *rest.Config) (*kubernetes.Clientset, error) {
	config.QPS = 80
	config.Burst = 100
	rootClientBuilder := clientbuilder.SimpleControllerClientBuilder{
		ClientConfig: config,
	}
	return kubernetes.NewForConfig(rootClientBuilder.ConfigOrDie("deploy-rolout"))
}

// LoadsKubeConfigFromFile tries to load kubeconfig from specified kubeconfig file
func LoadsKubeConfigFromFile(fileName string) (*kubernetes.Clientset, error) {
	clientConfig, err := clientcmd.LoadFromFile(fileName)
	if err != nil {
		return nil, fmt.Errorf("error while loading kubeconfig from file %v: %v", fileName, err)
	}

	kubeConfig, err := clientcmd.NewDefaultClientConfig(*clientConfig, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, err
	}

	return getK8sClientFromRest(kubeConfig)
}
