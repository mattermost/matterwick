package server

import (
	"log"
	"time"

	"github.com/mattermost/mattermost-cloud/k8s"
	"github.com/mattermost/mattermost-server/v5/mlog"
	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Deployment struct {
	Namespace      string
	ImageTag       string
	DeployFilePath string
	Environment    CWS
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

func waitForIPAssignment(kc *k8s.KubeClient, deployment Deployment) (string, error) {
	mlog.Info("Waiting for external IP to be assigned")
	IP := ""
	for {
		if IP != "" {
			break
		} else {
			log.Printf("No IP for now.\n")
		}

		lb, _ := kc.Clientset.CoreV1().Services(deployment.Namespace).Get("cws-test-service", metav1.GetOptions{})

		if len(lb.Status.LoadBalancer.Ingress) > 0 {
			IP = lb.Status.LoadBalancer.Ingress[0].Hostname
		}

		time.Sleep(1 * time.Second)
	}

	return IP, nil

}
