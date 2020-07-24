package server

import (
	"github.com/mattermost/mattermost-cloud/k8s"
	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Deployment struct {
	Namespace      string
	ImageTag       string
	DeployFilePath string
}

func namespaceExists(kc *k8s.KubeClient, namespaceName string) (bool, error) {
	_, err := kc.Clientset.CoreV1().Namespaces().Get(namespaceName, metav1.GetOptions{})
	if err != nil && !k8sErrors.IsNotFound(err) {
		return false, err
	}

	if err != nil && k8sErrors.IsNotFound(err) {
		return false, nil
	}
	return true, nil
}

func getOrCreateNamespace(kc *k8s.KubeClient, namespaceName string) (*corev1.Namespace, error) {
	namespace, err := kc.Clientset.CoreV1().Namespaces().Get(namespaceName, metav1.GetOptions{})
	if err != nil && !k8sErrors.IsNotFound(err) {
		return nil, err
	}

	if err != nil && k8sErrors.IsNotFound(err) {
		return kc.CreateOrUpdateNamespace(namespaceName)
	}
	return namespace, nil
}

func deleteNamespace(kc *k8s.KubeClient, nameSpaceName string) error {
	namespace := []string{nameSpaceName}
	err := kc.DeleteNamespaces(namespace)
	if err != nil {
		return err
	}
	return nil
}
